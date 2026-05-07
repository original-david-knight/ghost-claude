package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"vibedrive/internal/automation"
	"vibedrive/internal/config"
	"vibedrive/internal/diagnostics"
	"vibedrive/internal/plan"
	"vibedrive/internal/render"
	"vibedrive/internal/runstate"
	"vibedrive/internal/tasknotes"
	"vibedrive/internal/tmuxagent"
	"vibedrive/pkg/agentcli/claude"
	"vibedrive/pkg/agentcli/codex"
)

type Runner struct {
	cfg    *config.Config
	stdout io.Writer
	stderr io.Writer
	claude claudeClient
	codex  codexClient

	executablePath  string
	newSession      func(string) (*claude.Session, error)
	newCodexSession func() (*codex.Session, error)
	tmux            *tmuxagent.Controller
	tmuxAnnounced   bool

	parentStdoutTail *diagnostics.TailBuffer
	parentStderrTail *diagnostics.TailBuffer

	runStatePath      string
	runStateID        string
	runStateWorkspace string
	runStatePlanFile  string
}

type TemplateData struct {
	ConfigPath     string
	ExecutablePath string
	Iteration      int
	SessionID      string
	WorkflowName   string
	TaskResultPath string
	TaskNotesPath  string
	ReviewPath     string
	ArtifactRoot   string
	Workspace      string
	PlanFile       string
	Plan           *plan.File
	Task           plan.Task
	Now            time.Time
}

type TaskExecutionPaths struct {
	TaskID           string
	Name             string
	Workspace        string
	PlanFile         string
	WorktreeRoot     string
	ArtifactBaseRoot string
	ArtifactRoot     string
	TaskResultPath   string
	TaskNotesPath    string
	ReviewPath       string
	Isolated         bool
}

type claudeClient interface {
	RunPrompt(ctx context.Context, session *claude.Session, prompt string) error
	Close(session *claude.Session) error
	IsFullscreenTUI() bool
}

type codexClient interface {
	RunPrompt(ctx context.Context, session *codex.Session, prompt string) error
	Close(session *codex.Session) error
	IsFullscreenTUI() bool
}

func New(cfg *config.Config, stdout, stderr io.Writer) (*Runner, error) {
	if err := cfg.CheckPinnedAgentVersions(); err != nil {
		return nil, err
	}

	claudeAgent, err := claude.New(
		cfg.Claude.Command,
		cfg.Claude.Args,
		cfg.Workspace,
		cfg.Claude.Transport,
		cfg.Claude.StartupTimeout,
		stdout,
		stderr,
	)
	if err != nil {
		return nil, err
	}

	codexAgent, err := codex.New(
		cfg.Codex.Command,
		cfg.Codex.Args,
		cfg.Workspace,
		cfg.Codex.Transport,
		cfg.Codex.StartupTimeout,
		stdout,
		stderr,
	)
	if err != nil {
		return nil, err
	}

	executablePath, err := os.Executable()
	if err != nil {
		executablePath = os.Args[0]
	}
	if !filepath.IsAbs(executablePath) {
		if absPath, absErr := filepath.Abs(executablePath); absErr == nil {
			executablePath = absPath
		}
	}

	return &Runner{
		cfg:            cfg,
		stdout:         stdout,
		stderr:         stderr,
		claude:         claudeAgent,
		codex:          codexAgent,
		executablePath: executablePath,
		newSession: func(strategy string) (*claude.Session, error) {
			return claude.NewSession(strategy)
		},
		newCodexSession: func() (*codex.Session, error) {
			return codex.NewSession()
		},
	}, nil
}

func (r *Runner) Run(ctx context.Context) error {
	if strings.TrimSpace(r.cfg.PlanFile) == "" {
		return fmt.Errorf("plan_file is required")
	}
	r.ensureRunState()
	r.ensureParentDiagnosticTails()
	if !r.cfg.DryRun {
		defer r.clearRuntimeRun()
	}
	runTmux := false
	if !r.cfg.DryRun && r.shouldUseRunTmux() {
		if err := r.ensureRunTmux(ctx); err != nil {
			return err
		}
		runTmux = true
	}
	if runTmux || (r.cfg.EffectiveParallelism() > 1 && !r.cfg.DryRun) {
		defer r.closeRunTmux()
	}
	if r.cfg.EffectiveParallelism() > 1 && !r.cfg.DryRun {
		return r.runParallelPlan(ctx)
	}
	return r.runPlan(ctx)
}

func (r *Runner) runPlan(ctx context.Context) error {
	stalled := 0

	for iteration := 1; ; iteration++ {
		if r.cfg.MaxIterations > 0 && iteration > r.cfg.MaxIterations {
			return fmt.Errorf("stopped after reaching max_iterations=%d", r.cfg.MaxIterations)
		}

		currentPlan, err := plan.Load(r.cfg.PlanFile)
		if err != nil {
			return err
		}

		task, err := currentPlan.FindNextReady()
		if err != nil {
			switch {
			case errors.Is(err, plan.ErrAllTasksDone):
				if r.shouldLogProgress() {
					fmt.Fprintln(r.stdout, "All plan tasks are complete.")
				}
				return nil
			case errors.Is(err, plan.ErrNoReadyTasks):
				return fmt.Errorf("no ready tasks remain in %s; unfinished tasks: %s", r.cfg.PlanFile, summarizeUnfinishedTasks(currentPlan.UnfinishedTasks()))
			default:
				return err
			}
		}

		stop, err := r.runSerialIteration(ctx, currentPlan, task, iteration, &stalled)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
}

func (r *Runner) runParallelPlan(ctx context.Context) error {
	stalled := 0

	for iteration := 1; ; iteration++ {
		if r.cfg.MaxIterations > 0 && iteration > r.cfg.MaxIterations {
			return fmt.Errorf("stopped after reaching max_iterations=%d", r.cfg.MaxIterations)
		}

		currentPlan, err := plan.Load(r.cfg.PlanFile)
		if err != nil {
			return err
		}

		analysis, err := currentPlan.AnalyzeReadyBatch(r.cfg.EffectiveParallelism())
		if err != nil {
			return err
		}

		if len(analysis.Selected) == 0 {
			_, err := currentPlan.FindNextReady()
			switch {
			case errors.Is(err, plan.ErrAllTasksDone):
				if r.shouldLogProgress() {
					fmt.Fprintln(r.stdout, "All plan tasks are complete.")
				}
				return nil
			case errors.Is(err, plan.ErrNoReadyTasks):
				return fmt.Errorf("no ready tasks remain in %s; unfinished tasks: %s", r.cfg.PlanFile, summarizeUnfinishedTasks(currentPlan.UnfinishedTasks()))
			case err != nil:
				return err
			default:
				return fmt.Errorf("ready-batch analysis selected no tasks even though a ready task exists")
			}
		}

		if len(analysis.Selected) == 1 {
			stop, err := r.runSerialIteration(ctx, currentPlan, analysis.Selected[0], iteration, &stalled)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
			continue
		}

		if r.shouldLogProgress() {
			fmt.Fprintf(r.stdout, "\n== Iteration %d ==\n", iteration)
			fmt.Fprintf(r.stdout, "Parallel ready batch (%d/%d): %s\n", len(analysis.Selected), analysis.Limit, summarizeTaskIDs(analysis.Selected))
		}

		results, err := r.runParallelBatch(ctx, currentPlan, analysis.Selected, iteration)
		if r.shouldLogProgress() {
			for _, result := range results {
				if result.Err != nil {
					fmt.Fprintf(r.stderr, "parallel task %s failed: %v\n", result.Task.ID, result.Err)
					continue
				}
				fmt.Fprintf(r.stdout, "parallel task %s completed in %s with status %q\n", result.Task.ID, result.Paths.Workspace, result.Status)
			}
		}
		integrateErr := r.integrateParallelBatch(ctx, results)
		if integrateErr != nil {
			return integrateErr
		}
		if err != nil {
			return err
		}
	}
}

func (r *Runner) runSerialIteration(ctx context.Context, currentPlan *plan.File, task plan.Task, iteration int, stalled *int) (bool, error) {
	steps, workflowName, err := r.stepsForTask(task)
	if err != nil {
		return false, err
	}

	if r.shouldLogProgress() {
		fmt.Fprintf(r.stdout, "\n== Iteration %d ==\n", iteration)
		fmt.Fprintf(r.stdout, "Next task: %s (%s) via workflow %s\n", task.Title, task.ID, workflowName)
	}

	paths := taskExecutionPaths(r.cfg, task.ID, iteration, false)
	data := TemplateData{
		ConfigPath:     r.cfg.Path,
		ExecutablePath: r.executablePath,
		Iteration:      iteration,
		WorkflowName:   workflowName,
		TaskResultPath: paths.TaskResultPath,
		TaskNotesPath:  paths.TaskNotesPath,
		ReviewPath:     paths.ReviewPath,
		ArtifactRoot:   paths.ArtifactRoot,
		Workspace:      paths.Workspace,
		PlanFile:       paths.PlanFile,
		Plan:           currentPlan,
		Task:           task,
		Now:            time.Now(),
	}

	if err := ensurePlanArtifactDirectories(data); err != nil {
		return false, err
	}

	currentNotes, err := tasknotes.Load(data.TaskNotesPath)
	if err != nil {
		return false, err
	}
	currentSignature := taskProgressSignature(task, currentNotes)

	if err := r.runSteps(ctx, steps, data); err != nil {
		return false, err
	}

	if r.cfg.DryRun {
		fmt.Fprintln(r.stdout, "\nDry run complete.")
		return true, nil
	}

	nextPlan, err := plan.Load(r.cfg.PlanFile)
	if err != nil {
		return false, err
	}

	updatedTask, ok := nextPlan.FindTask(task.ID)
	if !ok {
		return false, fmt.Errorf("task %q disappeared from %s during iteration %d", task.ID, r.cfg.PlanFile, iteration)
	}
	if updatedTask.IsTerminal() {
		r.clearParallelRecoveryArtifact(task.ID)
	}

	nextNotes, err := tasknotes.Load(data.TaskNotesPath)
	if err != nil {
		return false, err
	}

	if taskProgressSignature(updatedTask, nextNotes) == currentSignature {
		(*stalled)++
		if *stalled >= r.cfg.MaxStalledIterations {
			return false, fmt.Errorf(
				"iteration %d made no task progress; %q (%s) still has status %q in %s. "+
					"The workflow must update the selected task's status or task notes when work progresses",
				iteration,
				updatedTask.Title,
				updatedTask.ID,
				updatedTask.Status,
				r.cfg.PlanFile,
			)
		}
		if r.shouldLogProgress() {
			fmt.Fprintf(r.stderr, "warning: no task progress after iteration %d; retrying (%d/%d)\n", iteration, *stalled, r.cfg.MaxStalledIterations)
		}
	} else {
		*stalled = 0
	}

	return false, nil
}

type parallelTaskResult struct {
	Task         plan.Task
	Paths        TaskExecutionPaths
	BaseRevision string
	Status       string
	Notes        string
	Err          error
}

type parallelRecoveryArtifact struct {
	Dir              string `json:"-"`
	PatchPath        string `json:"patch_path"`
	MetadataPath     string `json:"metadata_path"`
	InstructionsPath string `json:"instructions_path"`
}

type parallelRecoveryMetadata struct {
	TaskID          string `json:"task_id"`
	WorkerWorkspace string `json:"worker_workspace"`
	BaseRevision    string `json:"base_revision,omitempty"`
	Status          string `json:"status,omitempty"`
	Notes           string `json:"notes,omitempty"`
	Error           string `json:"error"`
	CreatedAt       string `json:"created_at"`
}

type parallelPatchApplication struct {
	Applied         bool
	FileSync        bool
	FileSyncEntries []parallelChangedPath
}

type parallelChangedPath struct {
	Status byte
	Path   string
}

type preparedParallelTask struct {
	Task             plan.Task
	Paths            TaskExecutionPaths
	Steps            []config.Step
	WorkflowName     string
	IsolatedPlan     *plan.File
	IsolatedTask     plan.Task
	InitialSignature string
	BaseRevision     string
}

func (r *Runner) runParallelBatch(ctx context.Context, currentPlan *plan.File, tasks []plan.Task, iteration int) ([]parallelTaskResult, error) {
	needsTmux, err := r.parallelBatchNeedsTmux(tasks)
	if err != nil {
		return nil, err
	}
	if needsTmux {
		if err := r.ensureParallelTmux(ctx); err != nil {
			return nil, err
		}
	}

	results := make([]parallelTaskResult, len(tasks))
	prepared := make([]preparedParallelTask, len(tasks))
	ready := make([]int, 0, len(tasks))

	for i, task := range tasks {
		paths := taskExecutionPaths(r.cfg, task.ID, iteration, true)
		results[i] = parallelTaskResult{Task: task, Paths: paths}

		work, err := r.prepareParallelTask(ctx, currentPlan, task, paths)
		if err != nil {
			results[i].Err = err
			continue
		}

		prepared[i] = work
		ready = append(ready, i)
	}

	var wg sync.WaitGroup
	for _, i := range ready {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = r.runPreparedParallelTask(ctx, prepared[i], iteration)
		}(i)
	}
	wg.Wait()

	errs := make([]error, 0)
	for _, result := range results {
		if result.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.Task.ID, result.Err))
		}
	}
	if len(errs) > 0 {
		return results, errors.Join(errs...)
	}
	return results, nil
}

func (r *Runner) prepareParallelTask(ctx context.Context, currentPlan *plan.File, task plan.Task, paths TaskExecutionPaths) (preparedParallelTask, error) {
	steps, workflowName, err := r.stepsForTask(task)
	if err != nil {
		return preparedParallelTask{}, err
	}

	if err := r.prepareParallelTaskWorkspace(ctx, currentPlan, paths); err != nil {
		return preparedParallelTask{}, err
	}

	isolatedPlan, err := plan.Load(paths.PlanFile)
	if err != nil {
		return preparedParallelTask{}, err
	}

	isolatedTask, ok := isolatedPlan.FindTask(task.ID)
	if !ok {
		return preparedParallelTask{}, fmt.Errorf("task %q disappeared from isolated plan %s", task.ID, paths.PlanFile)
	}

	if err := ensurePlanArtifactDirectories(TemplateData{
		TaskResultPath: paths.TaskResultPath,
		ReviewPath:     paths.ReviewPath,
	}); err != nil {
		return preparedParallelTask{}, err
	}

	currentNotes, err := tasknotes.Load(paths.TaskNotesPath)
	if err != nil {
		return preparedParallelTask{}, err
	}

	baseRevision, err := gitRevision(ctx, r.cfg.Workspace, "HEAD")
	if err != nil {
		baseRevision = ""
	}

	return preparedParallelTask{
		Task:             task,
		Paths:            paths,
		Steps:            steps,
		WorkflowName:     workflowName,
		IsolatedPlan:     isolatedPlan,
		IsolatedTask:     isolatedTask,
		InitialSignature: taskProgressSignature(isolatedTask, currentNotes),
		BaseRevision:     baseRevision,
	}, nil
}

func (r *Runner) runPreparedParallelTask(ctx context.Context, work preparedParallelTask, iteration int) parallelTaskResult {
	result := parallelTaskResult{Task: work.Task, Paths: work.Paths, BaseRevision: work.BaseRevision}

	data := TemplateData{
		ConfigPath:     r.cfg.Path,
		ExecutablePath: r.executablePath,
		Iteration:      iteration,
		WorkflowName:   work.WorkflowName,
		TaskResultPath: work.Paths.TaskResultPath,
		TaskNotesPath:  work.Paths.TaskNotesPath,
		ReviewPath:     work.Paths.ReviewPath,
		ArtifactRoot:   work.Paths.ArtifactRoot,
		Workspace:      work.Paths.Workspace,
		PlanFile:       work.Paths.PlanFile,
		Plan:           work.IsolatedPlan,
		Task:           work.IsolatedTask,
		Now:            time.Now(),
	}

	worker, err := r.isolatedRunner(work.Paths, work.Steps)
	if err != nil {
		result.Err = err
		return result
	}

	if err := worker.runSteps(ctx, work.Steps, data); err != nil {
		result.Err = err
		return result
	}

	status, notes, err := collectParallelTaskProgress(work.Paths, work.Task.ID, work.InitialSignature)
	if err != nil {
		result.Err = err
		return result
	}
	result.Status = status
	result.Notes = notes
	return result
}

func (r *Runner) integrateParallelBatch(ctx context.Context, results []parallelTaskResult) error {
	errs := make([]error, 0)
	for _, result := range results {
		if err := r.integrateParallelResult(ctx, result); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.Task.ID, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (r *Runner) integrateParallelResult(ctx context.Context, result parallelTaskResult) error {
	cleanup := result.Paths.Isolated
	if result.Paths.Isolated {
		defer func() {
			if cleanup {
				r.cleanupIntegratedParallelTask(ctx, result.Paths)
			}
		}()
	}

	if result.Err != nil {
		cleanup = false
		notes := appendIntegrationNote(result.Notes, "Parallel worker failed: "+shortError(result.Err)+".")
		if err := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); err != nil {
			return fmt.Errorf("record worker failure: %w", err)
		}
		return result.Err
	}

	status := normalizeCollectedTaskStatus(result.Status)
	if status == "" {
		cleanup = false
		err := fmt.Errorf("unsupported parallel task status %q", result.Status)
		notes := appendIntegrationNote(result.Notes, "Parallel worker returned an unsupported status.")
		if recordErr := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); recordErr != nil {
			return fmt.Errorf("%w; also failed to record follow-up: %v", err, recordErr)
		}
		return err
	}

	switch status {
	case plan.StatusBlocked, plan.StatusManual:
		if err := r.recordParallelTaskStatus(ctx, result.Task, status, result.Notes); err != nil {
			return err
		}
		return fmt.Errorf("parallel task reported %s", status)
	case plan.StatusDone, plan.StatusInProgress:
	default:
		return fmt.Errorf("unsupported parallel task status %q", status)
	}

	patch, err := r.parallelWorkerPatch(ctx, result)
	if err != nil {
		cleanup = false
		notes := appendIntegrationNote(result.Notes, "Parallel worker changes could not be collected: "+shortError(err)+".")
		if recordErr := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); recordErr != nil {
			return fmt.Errorf("%w; also failed to record follow-up: %v", err, recordErr)
		}
		return err
	}

	application, err := r.applyParallelWorkerPatch(ctx, result, patch)
	if err != nil {
		cleanup = false
		recovery, recoveryErr := r.writeParallelRecoveryArtifact(result, patch, err)
		notes := appendParallelRecoveryNote(result.Notes, err, recovery, recoveryErr)
		if recordErr := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); recordErr != nil {
			return fmt.Errorf("%w; also failed to record follow-up: %v", err, recordErr)
		}
		return err
	}
	if application.FileSync {
		result.Notes = appendIntegrationNote(result.Notes, "Git reported an oversized patch; integrated changed files with the safe file-sync fallback.")
	}

	resultPath, err := writeRootParallelTaskResult(r.cfg.Workspace, result.Task.ID, status, result.Notes)
	if err != nil {
		return err
	}

	err = automation.Finalize(ctx, automation.FinalizeOptions{
		Workspace:     r.cfg.Workspace,
		PlanFile:      r.cfg.PlanFile,
		TaskID:        result.Task.ID,
		ResultPath:    resultPath,
		CommitMessage: taskCommitMessage(result.Task),
	}, r.stdout, r.stderr)
	if err == nil {
		r.clearParallelRecoveryArtifact(result.Task.ID)
		return nil
	}
	cleanup = false

	if application.Applied && status == plan.StatusDone {
		var reverseErr error
		if application.FileSync {
			reverseErr = rollbackParallelWorkerFileSync(ctx, r.cfg.Workspace, result.BaseRevision, application.FileSyncEntries)
		} else {
			reverseErr = gitApplyPatchFunc(ctx, r.cfg.Workspace, patch, false, true)
		}
		if reverseErr != nil {
			return fmt.Errorf("%w; also failed to roll back unverified worker changes: %v", err, reverseErr)
		}
	}
	if status == plan.StatusDone {
		if commitErr := automation.CommitIfNeeded(ctx, r.cfg.Workspace, parallelFollowUpCommitMessage(result.Task.ID), r.stdout, r.stderr); commitErr != nil {
			return fmt.Errorf("%w; also failed to commit verification follow-up: %v", err, commitErr)
		}
	}
	return err
}

func (r *Runner) parallelWorkerPatch(ctx context.Context, result parallelTaskResult) ([]byte, error) {
	if strings.TrimSpace(result.BaseRevision) == "" {
		return nil, fmt.Errorf("parallel integration requires a git base revision for task %s", result.Task.ID)
	}
	if strings.TrimSpace(result.Paths.Workspace) == "" {
		return nil, fmt.Errorf("parallel worker workspace is required for task %s", result.Task.ID)
	}

	excludes := workerIntegrationExcludes(result.Paths)
	if err := automation.StageAllChangesExcept(ctx, result.Paths.Workspace, nil, nil, excludes...); err != nil {
		return nil, err
	}

	diffArgs := []string{"-C", result.Paths.Workspace, "diff", "--binary", result.BaseRevision, "--", "."}
	diffArgs = append(diffArgs, excludes...)
	cmd := exec.CommandContext(ctx, "git", diffArgs...)
	patch, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff worker changes: %w", err)
	}
	return patch, nil
}

func (r *Runner) applyParallelWorkerPatch(ctx context.Context, result parallelTaskResult, patch []byte) (parallelPatchApplication, error) {
	if len(bytes.TrimSpace(patch)) == 0 {
		return parallelPatchApplication{}, nil
	}

	if err := gitApplyPatchFunc(ctx, r.cfg.Workspace, patch, true, false); err != nil {
		return r.applyParallelWorkerPatchFallback(ctx, result, err)
	}
	if err := gitApplyPatchFunc(ctx, r.cfg.Workspace, patch, false, false); err != nil {
		return r.applyParallelWorkerPatchFallback(ctx, result, err)
	}
	return parallelPatchApplication{Applied: true}, nil
}

func (r *Runner) applyParallelWorkerPatchFallback(ctx context.Context, result parallelTaskResult, applyErr error) (parallelPatchApplication, error) {
	if !isGitApplyPatchTooLarge(applyErr) {
		return parallelPatchApplication{}, applyErr
	}

	entries, err := r.applyParallelWorkerFiles(ctx, result)
	if err != nil {
		return parallelPatchApplication{}, fmt.Errorf("%w; oversized patch file-sync fallback failed: %v", applyErr, err)
	}
	return parallelPatchApplication{
		Applied:         true,
		FileSync:        true,
		FileSyncEntries: entries,
	}, nil
}

func isGitApplyPatchTooLarge(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "patch too large")
}

func (r *Runner) applyParallelWorkerFiles(ctx context.Context, result parallelTaskResult) ([]parallelChangedPath, error) {
	entries, err := parallelWorkerChangedPaths(ctx, result)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return entries, nil
	}

	for _, entry := range entries {
		if err := validateParallelChangedPath(entry.Path); err != nil {
			return nil, err
		}
		if err := rootPathUnchangedSinceBase(ctx, r.cfg.Workspace, result.BaseRevision, entry); err != nil {
			return nil, err
		}
	}

	for _, entry := range entries {
		if err := syncParallelWorkerPath(r.cfg.Workspace, result.Paths.Workspace, entry); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func parallelWorkerChangedPaths(ctx context.Context, result parallelTaskResult) ([]parallelChangedPath, error) {
	if strings.TrimSpace(result.BaseRevision) == "" {
		return nil, fmt.Errorf("parallel integration requires a git base revision for task %s", result.Task.ID)
	}
	if strings.TrimSpace(result.Paths.Workspace) == "" {
		return nil, fmt.Errorf("parallel worker workspace is required for task %s", result.Task.ID)
	}

	excludes := workerIntegrationExcludes(result.Paths)
	if err := automation.StageAllChangesExcept(ctx, result.Paths.Workspace, nil, nil, excludes...); err != nil {
		return nil, err
	}

	diffArgs := []string{"-C", result.Paths.Workspace, "diff", "--name-status", "-z", "--no-renames", result.BaseRevision, "--", "."}
	diffArgs = append(diffArgs, excludes...)
	cmd := exec.CommandContext(ctx, "git", diffArgs...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff worker changed paths: %w", err)
	}

	entries, err := parseGitNameStatusZ(output)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := validateParallelChangedPath(entry.Path); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func parseGitNameStatusZ(output []byte) ([]parallelChangedPath, error) {
	if len(output) == 0 {
		return nil, nil
	}

	fields := bytes.Split(output, []byte{0})
	entries := make([]parallelChangedPath, 0, len(fields)/2)
	for i := 0; i < len(fields); {
		if len(fields[i]) == 0 {
			i++
			continue
		}

		status := fields[i][0]
		i++
		if i >= len(fields) || len(fields[i]) == 0 {
			return nil, fmt.Errorf("parse git name-status output: missing path for status %q", string(status))
		}

		path := string(fields[i])
		i++
		entries = append(entries, parallelChangedPath{Status: status, Path: path})
	}
	return entries, nil
}

func rootPathUnchangedSinceBase(ctx context.Context, workspace, baseRevision string, entry parallelChangedPath) error {
	gitPath, localPath, err := cleanParallelChangedPath(entry.Path)
	if err != nil {
		return err
	}

	if entry.Status == 'A' {
		if _, statErr := os.Lstat(filepath.Join(workspace, localPath)); statErr == nil {
			return fmt.Errorf("root path %s already exists; refusing oversized patch file-sync fallback", gitPath)
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}

	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "--quiet", baseRevision, "--", gitPath)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return fmt.Errorf("root path %s changed since %s; refusing oversized patch file-sync fallback", gitPath, shortRevision(baseRevision))
	}
	return fmt.Errorf("git diff root path %s: %w: %s", gitPath, err, strings.TrimSpace(string(output)))
}

func syncParallelWorkerPath(rootWorkspace, workerWorkspace string, entry parallelChangedPath) error {
	gitPath, localPath, err := cleanParallelChangedPath(entry.Path)
	if err != nil {
		return err
	}

	if entry.Status == 'D' {
		return removeRootFilePath(rootWorkspace, localPath)
	}

	source := filepath.Join(workerWorkspace, localPath)
	target := filepath.Join(rootWorkspace, localPath)
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("read worker path %s: %w", gitPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("worker path %s is a directory; expected a file or symlink", gitPath)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := removeRootFilePath(rootWorkspace, localPath); err != nil {
			return err
		}
		return os.Symlink(linkTarget, target)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("worker path %s has unsupported mode %s", gitPath, info.Mode())
	}

	if existing, err := os.Lstat(target); err == nil && existing.IsDir() {
		return fmt.Errorf("root path %s is a directory; refusing to replace it with a file", gitPath)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return copyRegularFile(source, target, info.Mode().Perm())
}

func rollbackParallelWorkerFileSync(ctx context.Context, workspace, baseRevision string, entries []parallelChangedPath) error {
	for _, entry := range entries {
		gitPath, localPath, err := cleanParallelChangedPath(entry.Path)
		if err != nil {
			return err
		}

		if entry.Status == 'A' {
			if err := removeRootFilePath(workspace, localPath); err != nil {
				return err
			}
			continue
		}

		cmd := exec.CommandContext(ctx, "git", "-C", workspace, "checkout", baseRevision, "--", gitPath)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout %s -- %s: %w: %s", shortRevision(baseRevision), gitPath, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func removeRootFilePath(rootWorkspace, localPath string) error {
	target := filepath.Join(rootWorkspace, localPath)
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("root path %s is a directory; refusing to remove it as a file", filepath.ToSlash(localPath))
	}
	return os.Remove(target)
}

func validateParallelChangedPath(path string) error {
	_, _, err := cleanParallelChangedPath(path)
	return err
}

func cleanParallelChangedPath(path string) (string, string, error) {
	gitPath := strings.TrimSpace(path)
	if gitPath == "" || strings.HasPrefix(gitPath, "/") {
		return "", "", fmt.Errorf("invalid changed path %q", path)
	}
	localPath := filepath.Clean(filepath.FromSlash(gitPath))
	if !isLocalRelativePath(localPath) {
		return "", "", fmt.Errorf("invalid changed path %q", path)
	}
	gitPath = filepath.ToSlash(localPath)
	if gitPath == ".git" || strings.HasPrefix(gitPath, ".git/") {
		return "", "", fmt.Errorf("invalid changed path %q", path)
	}
	return gitPath, localPath, nil
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func (r *Runner) writeParallelRecoveryArtifact(result parallelTaskResult, patch []byte, applyErr error) (parallelRecoveryArtifact, error) {
	artifact := parallelRecoveryArtifactForTask(r.cfg, result.Task.ID)
	if len(bytes.TrimSpace(patch)) == 0 {
		return artifact, fmt.Errorf("parallel recovery patch is empty")
	}
	if err := os.MkdirAll(artifact.Dir, 0o755); err != nil {
		return artifact, err
	}
	if err := os.WriteFile(artifact.PatchPath, patch, 0o644); err != nil {
		return artifact, err
	}

	metadata := parallelRecoveryMetadata{
		TaskID:          result.Task.ID,
		WorkerWorkspace: result.Paths.Workspace,
		BaseRevision:    result.BaseRevision,
		Status:          result.Status,
		Notes:           strings.TrimSpace(result.Notes),
		Error:           shortError(applyErr),
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return artifact, err
	}
	if err := os.WriteFile(artifact.MetadataPath, append(data, '\n'), 0o644); err != nil {
		return artifact, err
	}

	instructions := fmt.Sprintf(`# Parallel Recovery: %s

The parallel worker completed, but its patch did not apply cleanly to the root workspace.

Files:
- rejected.patch: worker changes that failed root integration
- metadata.json: task, base revision, and integration error

On the next Vibedrive run, the coder prompt for this task will include this recovery patch. Reconcile useful changes manually against the current workspace; do not apply the patch blindly.
`, result.Task.ID)
	if err := os.WriteFile(artifact.InstructionsPath, []byte(instructions), 0o644); err != nil {
		return artifact, err
	}
	return artifact, nil
}

func (r *Runner) withParallelRecoveryPrompt(prompt string, data TemplateData, target string, step config.Step) string {
	if !isAgentTarget(target) {
		return prompt
	}
	if strings.TrimSpace(strings.ToLower(step.Actor)) == config.StepActorReviewer {
		return prompt
	}

	artifact, ok := r.parallelRecoveryArtifact(data.Task.ID)
	if !ok {
		return prompt
	}

	recovery := fmt.Sprintf(`Parallel recovery context:
A previous parallel worker for this task completed, but its patch did not apply cleanly to the root workspace.

Recovery patch: %s
Recovery metadata: %s

Before making new edits, inspect the recovery patch and reconcile any still-useful changes against the current workspace. Do not apply it blindly; adapt it to the current files and discard stale hunks. Record what you reused or discarded in %s.

`, artifact.PatchPath, artifact.MetadataPath, data.TaskNotesPath)
	return recovery + strings.TrimLeft(prompt, "\n")
}

func (r *Runner) parallelRecoveryArtifact(taskID string) (parallelRecoveryArtifact, bool) {
	artifact := parallelRecoveryArtifactForTask(r.cfg, taskID)
	info, err := os.Stat(artifact.PatchPath)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return artifact, false
	}
	return artifact, true
}

func (r *Runner) clearParallelRecoveryArtifact(taskID string) {
	artifact := parallelRecoveryArtifactForTask(r.cfg, taskID)
	_ = os.RemoveAll(artifact.Dir)
}

func parallelRecoveryArtifactForTask(cfg *config.Config, taskID string) parallelRecoveryArtifact {
	root := config.DefaultParallelArtifactRoot
	if cfg != nil {
		root = cfg.ParallelArtifactRoot()
	}
	key := parallelRecoveryTaskKey(taskID)
	dir := filepath.Join(root, "recovery", key)
	return parallelRecoveryArtifact{
		Dir:              dir,
		PatchPath:        filepath.Join(dir, "rejected.patch"),
		MetadataPath:     filepath.Join(dir, "metadata.json"),
		InstructionsPath: filepath.Join(dir, "README.md"),
	}
}

func parallelRecoveryTaskKey(taskID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(taskID)))
	return taskIDSlug(taskID) + "-" + hex.EncodeToString(sum[:])[:12]
}

func appendParallelRecoveryNote(notes string, applyErr error, artifact parallelRecoveryArtifact, recoveryErr error) string {
	suffix := "Parallel worker changes did not apply cleanly: " + shortError(applyErr) + "."
	if recoveryErr != nil {
		suffix += " Failed to preserve recovery patch: " + shortError(recoveryErr) + "."
	} else {
		suffix += " Recovery patch preserved at " + artifact.PatchPath + "; the next coder run should reconcile useful changes from that patch."
	}
	return appendIntegrationNote(notes, suffix)
}

func (r *Runner) recordParallelTaskStatus(ctx context.Context, task plan.Task, status, notes string) error {
	currentPlan, err := plan.Load(r.cfg.PlanFile)
	if err != nil {
		return err
	}

	found := false
	for i := range currentPlan.Tasks {
		if currentPlan.Tasks[i].ID != task.ID {
			continue
		}
		currentPlan.Tasks[i].Status = status
		currentPlan.Tasks[i].Notes = ""
		found = true
		break
	}
	if !found {
		return fmt.Errorf("task %q not found in %s", task.ID, r.cfg.PlanFile)
	}

	notesFile, err := tasknotes.Load(tasknotes.Path(r.cfg.Workspace))
	if err != nil {
		return err
	}
	if err := notesFile.Upsert(task.ID, status, notes); err != nil {
		return err
	}
	if err := notesFile.Save(); err != nil {
		return err
	}
	if err := currentPlan.Save(); err != nil {
		return err
	}
	return automation.CommitIfNeeded(ctx, r.cfg.Workspace, parallelFollowUpCommitMessage(task.ID), r.stdout, r.stderr)
}

func (r *Runner) cleanupIntegratedParallelTask(ctx context.Context, paths TaskExecutionPaths) {
	if err := removeExistingIsolatedWorkspace(ctx, r.cfg.Workspace, paths); err != nil && r.shouldLogProgress() {
		fmt.Fprintf(r.stderr, "warning: failed to clean isolated workspace for task %s: %v\n", paths.TaskID, err)
	}
}

func (r *Runner) isolatedRunner(paths TaskExecutionPaths, steps []config.Step) (*Runner, error) {
	workerCfg := *r.cfg
	workerCfg.Workspace = paths.Workspace
	workerCfg.PlanFile = paths.PlanFile

	worker := *r
	worker.cfg = &workerCfg

	needsClaude, needsCodex, err := r.agentTargetsForSteps(steps)
	if err != nil {
		return nil, err
	}
	if needsClaude {
		var claudeAgent claudeClient
		var err error
		if r.tmux != nil {
			claudeAgent, err = newTmuxClaudeClient(
				workerCfg.Claude.Command,
				workerCfg.Claude.Args,
				workerCfg.Workspace,
				workerCfg.Claude.StartupTimeout,
				r.tmux,
				paths.Name,
			)
		} else {
			claudeAgent, err = claude.New(
				workerCfg.Claude.Command,
				workerCfg.Claude.Args,
				workerCfg.Workspace,
				workerCfg.Claude.Transport,
				workerCfg.Claude.StartupTimeout,
				r.stdout,
				r.stderr,
			)
		}
		if err != nil {
			return nil, err
		}
		worker.claude = claudeAgent
	}
	if needsCodex {
		var codexAgent codexClient
		var err error
		if r.tmux != nil {
			codexAgent, err = newTmuxCodexClient(
				workerCfg.Codex.Command,
				workerCfg.Codex.Args,
				workerCfg.Workspace,
				workerCfg.Codex.StartupTimeout,
				r.tmux,
				paths.Name,
			)
		} else {
			codexAgent, err = codex.New(
				workerCfg.Codex.Command,
				workerCfg.Codex.Args,
				workerCfg.Workspace,
				workerCfg.Codex.Transport,
				workerCfg.Codex.StartupTimeout,
				r.stdout,
				r.stderr,
			)
		}
		if err != nil {
			return nil, err
		}
		worker.codex = codexAgent
	}

	return &worker, nil
}

func (r *Runner) agentTargetsForSteps(steps []config.Step) (bool, bool, error) {
	var needsClaude, needsCodex bool
	for _, step := range steps {
		if step.Disabled {
			continue
		}
		target, err := r.stepAgent(step)
		if err != nil {
			return false, false, err
		}
		switch target {
		case config.AgentClaude:
			needsClaude = true
		case config.AgentCodex:
			needsCodex = true
		}
	}
	return needsClaude, needsCodex, nil
}

func (r *Runner) parallelBatchNeedsTmux(tasks []plan.Task) (bool, error) {
	for _, task := range tasks {
		steps, _, err := r.stepsForTask(task)
		if err != nil {
			return false, err
		}
		needsClaude, needsCodex, err := r.agentTargetsForSteps(steps)
		if err != nil {
			return false, err
		}
		if needsClaude || needsCodex {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runner) ensureParallelTmux(ctx context.Context) error {
	if r.tmux == nil {
		r.tmux = tmuxagent.NewController(tmuxagent.Options{
			SessionName: tmuxagent.RunSessionName(r.cfg.Workspace, r.cfg.PlanFile, os.Getpid()),
			Stderr:      r.stderr,
		})
	}
	if err := r.tmux.Start(ctx); err != nil {
		return err
	}
	if !r.tmuxAnnounced && r.stderr != nil {
		fmt.Fprintf(r.stderr, "parallel TUI tmux session: %s\nattach with: %s\n", r.tmux.SessionName(), r.tmux.AttachCommand())
		r.tmuxAnnounced = true
	}
	return nil
}

func (r *Runner) shouldUseRunTmux() bool {
	if r == nil || r.cfg == nil {
		return false
	}
	return (r.claude != nil && r.claude.IsFullscreenTUI()) || (r.codex != nil && r.codex.IsFullscreenTUI())
}

func (r *Runner) ensureRunTmux(ctx context.Context) error {
	if r.tmux == nil {
		r.tmux = r.newRunTmuxController()
	}
	if err := r.tmux.Start(ctx); err != nil {
		return err
	}
	if err := r.useRootTmuxClients(); err != nil {
		return err
	}
	if !r.tmuxAnnounced && r.stderr != nil {
		fmt.Fprintf(r.stderr, "vibedrive tmux session: %s\nattach with: %s\n", r.tmux.SessionName(), r.tmux.AttachCommand())
		r.tmuxAnnounced = true
	}
	if err := r.openRunTmuxClient(ctx); err != nil {
		return err
	}
	return nil
}

func (r *Runner) newRunTmuxController() *tmuxagent.Controller {
	return tmuxagent.NewController(tmuxagent.Options{
		SessionName:   tmuxagent.RunSessionName(r.cfg.Workspace, r.cfg.PlanFile, os.Getpid()),
		Stderr:        r.stderr,
		StatusCommand: "sh",
		StatusArgs:    []string{"-lc", r.tmuxStatusScript()},
		StatusWorkdir: r.cfg.Workspace,
	})
}

func (r *Runner) tmuxStatusScript() string {
	executable := strings.TrimSpace(r.executablePath)
	if executable == "" {
		executable = os.Args[0]
	}
	args := []string{shellArg(executable), "view", "--active-only"}
	if strings.TrimSpace(r.cfg.Path) != "" {
		args = append(args, "--config", shellArg(r.cfg.Path))
	}
	args = append(args, "--workspace", shellArg(r.cfg.Workspace), "--plan", shellArg(r.cfg.PlanFile))
	return fmt.Sprintf("while :; do clear; %s; sleep 1; done", strings.Join(args, " "))
}

func shellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (r *Runner) useRootTmuxClients() error {
	if r.tmux == nil {
		return nil
	}
	if r.claude != nil && r.claude.IsFullscreenTUI() {
		claudeAgent, err := newTmuxClaudeClient(
			r.cfg.Claude.Command,
			r.cfg.Claude.Args,
			r.cfg.Workspace,
			r.cfg.Claude.StartupTimeout,
			r.tmux,
			"main",
		)
		if err != nil {
			return err
		}
		r.claude = claudeAgent
	}
	if r.codex != nil && r.codex.IsFullscreenTUI() {
		codexAgent, err := newTmuxCodexClient(
			r.cfg.Codex.Command,
			r.cfg.Codex.Args,
			r.cfg.Workspace,
			r.cfg.Codex.StartupTimeout,
			r.tmux,
			"main",
		)
		if err != nil {
			return err
		}
		r.codex = codexAgent
	}
	return nil
}

func (r *Runner) openRunTmuxClient(ctx context.Context) error {
	if r.tmux == nil {
		return nil
	}
	return r.tmux.OpenClient(ctx, os.Stdin, os.Stdout, os.Stderr)
}

func (r *Runner) closeRunTmux() {
	if r.tmux == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.tmux.Kill(ctx); err != nil && r.stderr != nil {
		fmt.Fprintf(r.stderr, "warning: failed to close tmux session %s: %v\n", r.tmux.SessionName(), err)
	}
}

func (r *Runner) prepareParallelTaskWorkspace(ctx context.Context, currentPlan *plan.File, paths TaskExecutionPaths) error {
	if err := removeExistingIsolatedWorkspace(ctx, r.cfg.Workspace, paths); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.WorktreeRoot, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.ArtifactRoot, 0o755); err != nil {
		return err
	}

	if err := checkGitWorkspace(ctx, r.cfg.Workspace); err != nil {
		if copyErr := copyWorkspace(r.cfg.Workspace, paths.Workspace, paths.WorktreeRoot, paths.ArtifactBaseRoot); copyErr != nil {
			return fmt.Errorf("prepare non-git isolated workspace copy: %w; fallback copy failed: %v", err, copyErr)
		}
	} else if err := addGitWorktree(ctx, r.cfg.Workspace, paths.Workspace); err != nil {
		return err
	}

	if err := writeIsolatedPlanSnapshot(currentPlan, paths.PlanFile); err != nil {
		return err
	}
	return copyTaskNotesSnapshot(tasknotes.Path(r.cfg.Workspace), paths.TaskNotesPath)
}

func removeExistingIsolatedWorkspace(ctx context.Context, rootWorkspace string, paths TaskExecutionPaths) error {
	if paths.Isolated {
		_ = exec.CommandContext(ctx, "git", "-C", rootWorkspace, "worktree", "remove", "--force", paths.Workspace).Run()
		_ = exec.CommandContext(ctx, "git", "-C", rootWorkspace, "worktree", "prune").Run()
	}
	return cleanupTaskExecutionPaths(paths)
}

func checkGitWorkspace(ctx context.Context, rootWorkspace string) error {
	check := exec.CommandContext(ctx, "git", "-C", rootWorkspace, "rev-parse", "--is-inside-work-tree")
	if output, err := check.CombinedOutput(); err != nil {
		return fmt.Errorf("git workspace check: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func addGitWorktree(ctx context.Context, rootWorkspace, workspace string) error {
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", rootWorkspace, "worktree", "add", "--detach", workspace, "HEAD")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func gitRevision(ctx context.Context, workspace, revision string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "--verify", revision)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w: %s", revision, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func runGitInWorkspace(ctx context.Context, workspace string, args ...string) error {
	cmdArgs := append([]string{"-C", workspace}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

var gitApplyPatchFunc = gitApplyPatch

func gitApplyPatch(ctx context.Context, workspace string, patch []byte, check, reverse bool) error {
	if len(bytes.TrimSpace(patch)) == 0 {
		return nil
	}

	args := []string{"-C", workspace, "apply"}
	if check {
		args = append(args, "--check")
	}
	if reverse {
		args = append(args, "--reverse")
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = bytes.NewReader(patch)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git apply: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func workerIntegrationExcludes(paths TaskExecutionPaths) []string {
	excludes := []string{
		":(exclude).vibedrive/run-state.json",
		":(exclude).vibedrive/task-notes.yaml",
		":(exclude).vibedrive/task-results/**",
		":(exclude).vibedrive/reviews/**",
		":(exclude).vibedrive/task-runs/**",
		":(exclude).vibedrive/worktrees/**",
		":(exclude)target/**",
	}
	if rel, ok := workspaceRelativeGitPath(paths.Workspace, paths.PlanFile); ok {
		excludes = append(excludes, ":(exclude)"+rel)
	}
	return excludes
}

func workspaceRelativeGitPath(workspace, path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(workspace), filepath.Clean(path))
	if err != nil || !isLocalRelativePath(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func writeRootParallelTaskResult(workspace, taskID, status, notes string) (string, error) {
	resultPath := automation.ResultPath(workspace, taskID)
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		return "", err
	}

	data, err := json.Marshal(automation.TaskResult{
		Status: status,
		Notes:  strings.TrimSpace(notes),
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(resultPath, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return resultPath, nil
}

func taskCommitMessage(task plan.Task) string {
	if message := strings.TrimSpace(task.CommitMessage); message != "" {
		return message
	}
	if title := strings.TrimSpace(task.Title); title != "" {
		return title
	}
	return "task " + strings.TrimSpace(task.ID)
}

func parallelFollowUpCommitMessage(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		taskID = "task"
	}
	return "chore: record parallel follow-up for " + taskID
}

func appendIntegrationNote(notes, suffix string) string {
	notes = strings.TrimSpace(notes)
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return notes
	}
	if notes == "" {
		return suffix
	}
	return notes + " " + suffix
}

func shortError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	message = strings.Join(strings.Fields(message), " ")
	const max = 240
	if len(message) > max {
		return strings.TrimSpace(message[:max]) + "..."
	}
	return message
}

func copyWorkspace(sourceRoot, destRoot string, excludedRoots ...string) error {
	absSource, err := filepath.Abs(sourceRoot)
	if err != nil {
		return err
	}
	absDest, err := filepath.Abs(destRoot)
	if err != nil {
		return err
	}

	excluded := []string{absDest}
	for _, root := range excludedRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		excluded = append(excluded, absRoot)
	}

	return filepath.WalkDir(absSource, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if shouldSkipCopiedPath(absSource, absPath, excluded) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(absSource, absPath)
		if err != nil {
			return err
		}
		target := filepath.Join(absDest, rel)
		if rel == "." {
			return os.MkdirAll(target, 0o755)
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(absPath)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		case entry.Type().IsRegular():
			return copyRegularFile(absPath, target, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func shouldSkipCopiedPath(sourceRoot, path string, excludedRoots []string) bool {
	rel, err := filepath.Rel(sourceRoot, path)
	if err == nil {
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			return true
		}
	}

	for _, excluded := range excludedRoots {
		rel, err := filepath.Rel(excluded, path)
		if err == nil && (rel == "." || isLocalRelativePath(rel)) {
			return true
		}
	}
	return false
}

func copyRegularFile(source, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeIsolatedPlanSnapshot(currentPlan *plan.File, path string) error {
	if currentPlan == nil {
		return fmt.Errorf("plan file is nil")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	snapshot := *currentPlan
	snapshot.Path = path
	snapshot.Tasks = append([]plan.Task(nil), currentPlan.Tasks...)
	return snapshot.Save()
}

func copyTaskNotesSnapshot(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

func collectParallelTaskProgress(paths TaskExecutionPaths, taskID, initialSignature string) (string, string, error) {
	if result, ok, err := loadParallelTaskResult(paths.TaskResultPath); err != nil {
		return "", "", err
	} else if ok {
		status := normalizeCollectedTaskStatus(result.Status)
		if status == "" {
			return "", "", fmt.Errorf("task result %s has unsupported status %q", paths.TaskResultPath, result.Status)
		}
		return status, strings.TrimSpace(result.Notes), nil
	}

	workerPlan, err := plan.Load(paths.PlanFile)
	if err != nil {
		return "", "", err
	}
	updatedTask, ok := workerPlan.FindTask(taskID)
	if !ok {
		return "", "", fmt.Errorf("task %q not found in isolated plan %s", taskID, paths.PlanFile)
	}
	notesFile, err := tasknotes.Load(paths.TaskNotesPath)
	if err != nil {
		return "", "", err
	}
	if taskProgressSignature(updatedTask, notesFile) == initialSignature {
		return "", "", fmt.Errorf("task %q made no isolated task progress; expected a task result or isolated plan/task notes update", taskID)
	}

	status := normalizeCollectedTaskStatus(updatedTask.Status)
	notes := strings.TrimSpace(updatedTask.Notes)
	if note, ok := notesFile.Find(taskID); ok {
		if noteStatus := normalizeCollectedTaskStatus(note.Status); noteStatus != "" {
			status = noteStatus
		}
		if strings.TrimSpace(note.Notes) != "" {
			notes = strings.TrimSpace(note.Notes)
		}
	}
	if status == "" {
		return "", "", fmt.Errorf("task %q has unsupported collected status %q", taskID, updatedTask.Status)
	}
	return status, notes, nil
}

func loadParallelTaskResult(path string) (automation.TaskResult, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return automation.TaskResult{}, false, nil
		}
		return automation.TaskResult{}, false, err
	}

	var result automation.TaskResult
	if err := json.Unmarshal(data, &result); err != nil {
		return automation.TaskResult{}, false, fmt.Errorf("parse task result %s: %w", path, err)
	}
	return result, true, nil
}

func normalizeCollectedTaskStatus(status string) string {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case plan.StatusDone:
		return plan.StatusDone
	case plan.StatusInProgress:
		return plan.StatusInProgress
	case plan.StatusBlocked:
		return plan.StatusBlocked
	case plan.StatusManual:
		return plan.StatusManual
	default:
		return ""
	}
}

func (r *Runner) createSession() (*claude.Session, error) {
	if r.newSession != nil {
		return r.newSession(r.cfg.Claude.SessionStrategy)
	}
	return claude.NewSession(r.cfg.Claude.SessionStrategy)
}

func (r *Runner) createCodexSession() (*codex.Session, error) {
	if r.newCodexSession != nil {
		return r.newCodexSession()
	}
	return codex.NewSession()
}

func (r *Runner) runSteps(ctx context.Context, steps []config.Step, data TemplateData) error {
	if !r.cfg.DryRun {
		defer r.clearRuntimeTask(data.Task.ID)
	}

	var sharedSession *claude.Session
	var sharedCodexSession *codex.Session
	type sessionCloser struct {
		label string
		close func() error
	}
	var sharedClosers []sessionCloser
	closeSharedSession := func(runErr error) error {
		for i := len(sharedClosers) - 1; i >= 0; i-- {
			closeErr := sharedClosers[i].close()
			if runErr != nil {
				if closeErr != nil {
					return fmt.Errorf("%w; also failed to close %s session: %v", runErr, sharedClosers[i].label, closeErr)
				}
				continue
			}
			if closeErr != nil {
				return closeErr
			}
		}

		return runErr
	}

	for stepIndex, step := range steps {
		if step.Disabled {
			continue
		}
		r.recordRuntimeStep(data, step, stepIndex, len(steps))

		err := func() error {
			var (
				target           string
				session          *claude.Session
				codexSession     *codex.Session
				closeStepSession bool
				closeCodexStep   bool
				err              error
			)

			target, err = r.stepAgent(step)
			if err != nil {
				return err
			}

			switch target {
			case config.AgentClaude:
				switch {
				case step.FreshSession:
					session, err = r.createSession()
					if err != nil {
						return err
					}
					closeStepSession = true
				case sharedSession == nil:
					sharedSession, err = r.createSession()
					if err != nil {
						return err
					}
					sessionToClose := sharedSession
					sharedClosers = append(sharedClosers, sessionCloser{
						label: "claude",
						close: func() error {
							return r.claude.Close(sessionToClose)
						},
					})
					session = sharedSession
				default:
					session = sharedSession
				}
			case config.AgentCodex:
				if r.codex == nil {
					return fmt.Errorf("codex step %q requires a codex client", step.Name)
				}

				switch {
				case step.FreshSession:
					codexSession, err = r.createCodexSession()
					if err != nil {
						return err
					}
					closeCodexStep = true
				case sharedCodexSession == nil:
					sharedCodexSession, err = r.createCodexSession()
					if err != nil {
						return err
					}
					sessionToClose := sharedCodexSession
					sharedClosers = append(sharedClosers, sessionCloser{
						label: "codex",
						close: func() error {
							return r.codex.Close(sessionToClose)
						},
					})
					codexSession = sharedCodexSession
				default:
					codexSession = sharedCodexSession
				}
			}

			runErr := r.runStep(ctx, session, codexSession, step, data)
			if closeStepSession {
				closeErr := r.claude.Close(session)
				if runErr != nil {
					if closeErr != nil {
						return fmt.Errorf("%w; also failed to close claude session: %v", runErr, closeErr)
					}
					return runErr
				}
				if closeErr != nil {
					return closeErr
				}
			}
			if closeCodexStep {
				closeErr := r.codex.Close(codexSession)
				if runErr != nil {
					if closeErr != nil {
						return fmt.Errorf("%w; also failed to close codex session: %v", runErr, closeErr)
					}
					return runErr
				}
				if closeErr != nil {
					return closeErr
				}
			}

			return runErr
		}()
		if err != nil {
			if step.ContinueOnError {
				if r.shouldLogProgress() {
					fmt.Fprintf(r.stderr, "warning: step %q failed but continue_on_error is set: %v\n", step.Name, err)
				}
				continue
			}
			return closeSharedSession(fmt.Errorf("step %q failed: %w", step.Name, err))
		}
	}

	return closeSharedSession(nil)
}

func (r *Runner) ensureRunState() {
	if r == nil || r.cfg == nil {
		return
	}
	if strings.TrimSpace(r.runStateWorkspace) == "" {
		r.runStateWorkspace = r.cfg.Workspace
	}
	if strings.TrimSpace(r.runStatePlanFile) == "" {
		r.runStatePlanFile = r.cfg.PlanFile
	}
	if strings.TrimSpace(r.runStatePath) == "" {
		r.runStatePath = runstate.Path(r.runStateWorkspace)
	}
	if strings.TrimSpace(r.runStateID) == "" {
		r.runStateID = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
}

func (r *Runner) recordRuntimeStep(data TemplateData, step config.Step, stepIndex, stepTotal int) {
	if r == nil || r.cfg == nil || r.cfg.DryRun {
		return
	}
	r.ensureRunState()
	if strings.TrimSpace(r.runStatePath) == "" || strings.TrimSpace(r.runStateID) == "" {
		return
	}

	err := runstate.UpsertTask(r.runStatePath, r.runtimeRun(), runstate.Task{
		ID:        data.Task.ID,
		Status:    plan.StatusInProgress,
		Workflow:  data.WorkflowName,
		StepName:  step.Name,
		StepType:  step.Type,
		StepActor: step.Actor,
		StepIndex: stepIndex,
		StepTotal: stepTotal,
		Workspace: r.runStateWorkspace,
		PlanFile:  r.runStatePlanFile,
	})
	if err != nil && r.stderr != nil {
		fmt.Fprintf(r.stderr, "warning: failed to update vibedrive run state: %v\n", err)
	}
}

func (r *Runner) clearRuntimeTask(taskID string) {
	if r == nil || r.cfg == nil || r.cfg.DryRun {
		return
	}
	r.ensureRunState()
	if strings.TrimSpace(r.runStatePath) == "" || strings.TrimSpace(r.runStateID) == "" {
		return
	}
	if err := runstate.ClearTask(r.runStatePath, r.runStateID, taskID); err != nil && r.stderr != nil {
		fmt.Fprintf(r.stderr, "warning: failed to clear vibedrive run state: %v\n", err)
	}
}

func (r *Runner) clearRuntimeRun() {
	if r == nil || r.cfg == nil || r.cfg.DryRun {
		return
	}
	r.ensureRunState()
	if strings.TrimSpace(r.runStatePath) == "" || strings.TrimSpace(r.runStateID) == "" {
		return
	}
	if err := runstate.ClearRun(r.runStatePath, r.runStateID); err != nil && r.stderr != nil {
		fmt.Fprintf(r.stderr, "warning: failed to clear vibedrive run state: %v\n", err)
	}
}

func (r *Runner) runtimeRun() runstate.Run {
	return runstate.Run{
		ID:        r.runStateID,
		PID:       os.Getpid(),
		Workspace: r.runStateWorkspace,
		PlanFile:  r.runStatePlanFile,
	}
}

func (r *Runner) ensureParentDiagnosticTails() {
	if r == nil {
		return
	}
	if r.stdout != nil && r.parentStdoutTail == nil {
		tail := diagnostics.NewTailBuffer(diagnostics.ParentOutputLimit)
		r.stdout = io.MultiWriter(r.stdout, tail)
		r.parentStdoutTail = tail
	}
	if r.stderr != nil && r.parentStderrTail == nil {
		tail := diagnostics.NewTailBuffer(diagnostics.ParentOutputLimit)
		r.stderr = io.MultiWriter(r.stderr, tail)
		r.parentStderrTail = tail
	}
}

func (r *Runner) diagnosticsWorkspace() string {
	if r != nil && strings.TrimSpace(r.runStateWorkspace) != "" {
		return r.runStateWorkspace
	}
	if r != nil && r.cfg != nil && strings.TrimSpace(r.cfg.Workspace) != "" {
		return r.cfg.Workspace
	}
	return "."
}

func (r *Runner) parentStdoutArtifact() diagnostics.ByteArtifact {
	if r == nil || r.parentStdoutTail == nil {
		return diagnostics.UnavailableBytes()
	}
	return r.parentStdoutTail.Snapshot().Bytes()
}

func (r *Runner) parentStderrArtifact() diagnostics.ByteArtifact {
	if r == nil || r.parentStderrTail == nil {
		return diagnostics.UnavailableBytes()
	}
	return r.parentStderrTail.Snapshot().Bytes()
}

func (r *Runner) captureExecFailure(
	err error,
	command []string,
	workdir string,
	envMap map[string]string,
	stdoutSnapshot diagnostics.TailSnapshot,
	stderrSnapshot diagnostics.TailSnapshot,
	combinedSnapshot diagnostics.TailSnapshot,
	stepCtx context.Context,
	step config.Step,
	data TemplateData,
) {
	r.ensureRunState()
	exitCode, signal := execFailureStatus(err)
	execCommand := diagnostics.ExecCommand{
		Argv:       append([]string(nil), command...),
		WorkingDir: workdir,
		ExitCode:   exitCode,
		Signal:     signal,
		TimedOut:   errors.Is(stepCtx.Err(), context.DeadlineExceeded),
		Env: diagnostics.ExecEnvironment{
			Step:          copyStringMap(envMap),
			InheritedKeys: inheritedEnvKeys(os.Environ()),
		},
	}
	if errors.Is(stepCtx.Err(), context.Canceled) && !execCommand.TimedOut {
		execCommand.Extra = map[string]any{"cancelled": true}
	}

	_, captureErr := diagnostics.New(r.diagnosticsWorkspace()).CaptureExec(diagnostics.ExecCapture{
		Identity: diagnostics.Identity{
			RunID:    r.runStateID,
			TaskID:   data.Task.ID,
			StepName: step.Name,
		},
		Failure: diagnostics.Failure{
			Path:    "exec_step_non_zero_exit",
			Message: fmt.Sprintf("run command: %v", err),
		},
		Transport:    diagnostics.Transport{Kind: "exec"},
		Command:      execCommand,
		Stdout:       stdoutSnapshot.Bytes(),
		Stderr:       stderrSnapshot.Bytes(),
		Combined:     combinedSnapshot.Bytes(),
		ParentStdout: r.parentStdoutArtifact(),
		ParentStderr: r.parentStderrArtifact(),
	})
	if captureErr != nil && r.stderr != nil {
		fmt.Fprintf(r.stderr, "warning: failed to capture exec diagnostics: %v\n", captureErr)
	}
}

func execFailureStatus(err error) (*int, string) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil, ""
	}
	exitCode := exitErr.ExitCode()
	if exitCode >= 0 {
		return &exitCode, ""
	}
	if exitErr.ProcessState == nil {
		return nil, ""
	}
	const signalPrefix = "signal: "
	state := exitErr.ProcessState.String()
	if strings.HasPrefix(state, signalPrefix) {
		return nil, strings.TrimPrefix(state, signalPrefix)
	}
	return nil, ""
}

func inheritedEnvKeys(env []string) []string {
	seen := make(map[string]struct{}, len(env))
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func (r *Runner) runStep(ctx context.Context, session *claude.Session, codexSession *codex.Session, step config.Step, data TemplateData) error {
	stepCtx := ctx
	var cancel context.CancelFunc
	if step.Timeout != "" {
		timeout, err := time.ParseDuration(step.Timeout)
		if err != nil {
			return fmt.Errorf("invalid timeout %q: %w", step.Timeout, err)
		}
		stepCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	requiredOutputs, err := renderRequiredOutputs(step.RequiredOutputs, data, r.cfg.Workspace)
	if err != nil {
		return fmt.Errorf("render required_outputs: %w", err)
	}
	if !r.cfg.DryRun {
		if err := prepareOutputDirectories(requiredOutputs); err != nil {
			return fmt.Errorf("prepare required_outputs: %w", err)
		}
	}

	target, err := r.stepAgent(step)
	if err != nil {
		return err
	}

	var notesBefore taskNotesSnapshot
	if !r.cfg.DryRun && isAgentTarget(target) && strings.TrimSpace(data.TaskNotesPath) != "" {
		notesBefore, err = readTaskNotesSnapshot(data.TaskNotesPath)
		if err != nil {
			return fmt.Errorf("read task notes before step %q: %w", step.Name, err)
		}
	}

	run := func() error {
		switch target {
		case config.AgentClaude:
			if session == nil {
				return fmt.Errorf("claude step %q requires a session", step.Name)
			}

			stepData := data
			if r.claude != nil && r.claude.IsFullscreenTUI() {
				stepData.SessionID = session.ID
			}

			prompt, err := render.String(step.Prompt, stepData)
			if err != nil {
				return fmt.Errorf("render prompt: %w", err)
			}
			prompt = r.withParallelRecoveryPrompt(prompt, stepData, target, step)

			if r.shouldLogProgress() {
				fmt.Fprintf(r.stdout, "\n--> claude step: %s\n", step.Name)
			}
			if r.cfg.DryRun {
				fmt.Fprintln(r.stdout, strings.TrimSpace(prompt))
				return nil
			}
			return r.runAgentPrompt(stepCtx, target, session, codexSession, data.Task.ID, step.Name, prompt)
		case config.AgentCodex:
			if r.codex == nil {
				return fmt.Errorf("codex step %q requires a codex client", step.Name)
			}

			prompt, err := render.String(step.Prompt, data)
			if err != nil {
				return fmt.Errorf("render prompt: %w", err)
			}
			prompt = r.withParallelRecoveryPrompt(prompt, data, target, step)

			if r.shouldLogProgress() {
				fmt.Fprintf(r.stdout, "\n--> codex step: %s\n", step.Name)
				writePromptPreview(r.stdout, prompt)
			}
			if r.cfg.DryRun {
				fmt.Fprintln(r.stdout, strings.TrimSpace(prompt))
				return nil
			}
			return r.runAgentPrompt(stepCtx, target, session, codexSession, data.Task.ID, step.Name, prompt)
		case config.StepTypeExec:
			r.ensureParentDiagnosticTails()

			command, err := render.Strings(step.Command, data)
			if err != nil {
				return fmt.Errorf("render command: %w", err)
			}
			if len(command) == 0 {
				return fmt.Errorf("rendered command is empty")
			}

			workdir := r.cfg.Workspace
			if step.WorkingDir != "" {
				workdir, err = render.String(step.WorkingDir, data)
				if err != nil {
					return fmt.Errorf("render working_dir: %w", err)
				}
				if !filepath.IsAbs(workdir) {
					workdir = filepath.Join(r.cfg.Workspace, workdir)
				}
				workdir = filepath.Clean(workdir)
			}

			envMap, err := render.Map(step.Env, data)
			if err != nil {
				return fmt.Errorf("render env: %w", err)
			}

			if r.shouldLogProgress() {
				fmt.Fprintf(r.stdout, "\n--> exec step: %s\n", step.Name)
				fmt.Fprintf(r.stdout, "    %s\n", strings.Join(command, " "))
			}
			if r.cfg.DryRun {
				return nil
			}

			cmd := exec.CommandContext(stepCtx, command[0], command[1:]...)
			cmd.Dir = workdir
			stdoutTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
			stderrTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
			combinedTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
			if r.stdout != nil {
				cmd.Stdout = io.MultiWriter(r.stdout, stdoutTail, combinedTail)
			} else {
				cmd.Stdout = io.MultiWriter(stdoutTail, combinedTail)
			}
			if r.stderr != nil {
				cmd.Stderr = io.MultiWriter(r.stderr, stderrTail, combinedTail)
			} else {
				cmd.Stderr = io.MultiWriter(stderrTail, combinedTail)
			}
			cmd.Env = os.Environ()
			for key, value := range envMap {
				cmd.Env = append(cmd.Env, key+"="+value)
			}

			if err := cmd.Run(); err != nil {
				stdoutSnapshot := stdoutTail.Snapshot()
				stderrSnapshot := stderrTail.Snapshot()
				combinedSnapshot := combinedTail.Snapshot()
				r.captureExecFailure(err, command, workdir, envMap, stdoutSnapshot, stderrSnapshot, combinedSnapshot, stepCtx, step, data)
				return fmt.Errorf("run command: %w%s", err, commandOutputSuffix(string(combinedSnapshot.Data)))
			}
			return nil
		default:
			return fmt.Errorf("unsupported step type %q", step.Type)
		}
	}

	if err := run(); err != nil {
		return err
	}
	if r.cfg.DryRun {
		return nil
	}
	if isAgentTarget(target) && strings.TrimSpace(data.TaskNotesPath) != "" {
		if err := r.validateTaskNotesAfterAgentStep(stepCtx, target, session, codexSession, step.Name, data.Task.ID, data.TaskNotesPath, notesBefore); err != nil {
			return err
		}
	}
	if err := r.ensureRequiredOutputsAfterStep(stepCtx, target, session, codexSession, step.Name, data, requiredOutputs); err != nil {
		return err
	}

	return nil
}

func commandOutputSuffix(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	const max = 4000
	if len(output) > max {
		output = "... " + output[len(output)-max:]
	}
	return "\ncommand output:\n" + output
}

func (r *Runner) runAgentPrompt(ctx context.Context, target string, session *claude.Session, codexSession *codex.Session, taskID, stepName, prompt string) error {
	r.ensureRunState()
	r.ensureParentDiagnosticTails()
	ctx = withTmuxDiagnosticsIdentity(ctx, r.runStateID, taskID, stepName)
	ctx = withTmuxDiagnosticsParentOutput(ctx, r.parentStdoutArtifact(), r.parentStderrArtifact())
	switch target {
	case config.AgentClaude:
		if session == nil {
			return fmt.Errorf("claude step %q requires a session", stepName)
		}
		ctx = claude.WithDiagnostics(ctx, claude.Diagnostics{
			Identity: diagnostics.Identity{
				RunID:    r.runStateID,
				TaskID:   taskID,
				StepName: stepName,
			},
			ParentStdout: r.parentStdoutArtifact(),
			ParentStderr: r.parentStderrArtifact(),
		})
		return r.claude.RunPrompt(ctx, session, prompt)
	case config.AgentCodex:
		if r.codex == nil {
			return fmt.Errorf("codex step %q requires a codex client", stepName)
		}
		return r.codex.RunPrompt(ctx, codexSession, prompt)
	default:
		return fmt.Errorf("step %q does not target an agent", stepName)
	}
}

func (r *Runner) validateTaskNotesAfterAgentStep(ctx context.Context, target string, session *claude.Session, codexSession *codex.Session, stepName, taskID, notesPath string, before taskNotesSnapshot) error {
	after, err := readTaskNotesSnapshot(notesPath)
	if err != nil {
		return fmt.Errorf("read task notes after step %q: %w", stepName, err)
	}
	if !before.changed(after) {
		return nil
	}

	if _, err := tasknotes.Load(notesPath); err == nil {
		return nil
	} else {
		prompt := taskNotesRepairPrompt(notesPath, taskID, err)
		if r.shouldLogProgress() {
			fmt.Fprintf(r.stderr, "warning: task notes YAML is invalid after step %q; asking %s to repair it\n", stepName, target)
		}
		if err := r.runAgentPrompt(ctx, target, session, codexSession, taskID, stepName, prompt); err != nil {
			return fmt.Errorf("ask %s to repair task notes YAML after step %q: %w", target, stepName, err)
		}
		if _, err := tasknotes.Load(notesPath); err != nil {
			return fmt.Errorf("task notes YAML is still invalid after repair prompt for step %q: %w", stepName, err)
		}
	}

	return nil
}

func (r *Runner) ensureRequiredOutputsAfterStep(ctx context.Context, target string, session *claude.Session, codexSession *codex.Session, stepName string, data TemplateData, requiredOutputs []string) error {
	issues, err := inspectRequiredOutputs(requiredOutputs, data)
	if err != nil {
		return err
	}
	if len(issues) == 0 {
		return nil
	}

	outputErr := requiredOutputsIssueError(stepName, issues)
	if !isAgentTarget(target) {
		return outputErr
	}

	if r.shouldLogProgress() {
		fmt.Fprintf(r.stderr, "warning: step %q did not produce valid required outputs; asking %s to repair them\n", stepName, target)
	}
	if err := r.runAgentPrompt(ctx, target, session, codexSession, data.Task.ID, stepName, requiredOutputsRepairPrompt(stepName, data.Task.ID, issues)); err != nil {
		return fmt.Errorf("ask %s to create required outputs after step %q: %w", target, stepName, err)
	}

	issues, err = inspectRequiredOutputs(requiredOutputs, data)
	if err != nil {
		return err
	}
	if len(issues) > 0 {
		return requiredOutputsIssueError(stepName, issues)
	}
	return nil
}

type requiredOutputIssue struct {
	Path    string
	Problem string
	Missing bool
}

type taskNotesSnapshot struct {
	exists bool
	data   []byte
}

func readTaskNotesSnapshot(path string) (taskNotesSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return taskNotesSnapshot{}, nil
		}
		return taskNotesSnapshot{}, err
	}
	return taskNotesSnapshot{exists: true, data: data}, nil
}

func (s taskNotesSnapshot) changed(next taskNotesSnapshot) bool {
	if s.exists != next.exists {
		return true
	}
	return !bytes.Equal(s.data, next.data)
}

func isAgentTarget(target string) bool {
	switch target {
	case config.AgentClaude, config.AgentCodex:
		return true
	default:
		return false
	}
}

func taskNotesRepairPrompt(path, taskID string, parseErr error) string {
	return fmt.Sprintf(`The task notes YAML at %s does not parse after your previous step for task %s.

Parse error:
%s

Fix %s so it is valid YAML. Use this shape:
tasks:
  - id: <task id>
    status: done|in_progress|blocked|manual
    notes: <brief notes>

Preserve existing task notes and statuses as much as possible. Do not edit vibedrive-plan.yaml or make unrelated changes.`, path, taskID, parseErr, path)
}

func requiredOutputsRepairPrompt(stepName, taskID string, issues []requiredOutputIssue) string {
	var b strings.Builder
	missing := requiredOutputIssuePaths(issues, true)
	if len(missing) > 0 {
		fmt.Fprintf(&b, "Your previous step %q for task %s finished without creating these required output files:\n", stepName, taskID)
		for _, path := range missing {
			fmt.Fprintf(&b, "- %s\n", path)
		}
	}

	invalid := requiredOutputInvalidIssues(issues)
	if len(invalid) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "Your previous step %q for task %s created these required output files, but they are invalid:\n", stepName, taskID)
		for _, issue := range invalid {
			fmt.Fprintf(&b, "- %s: %s\n", issue.Path, issue.Problem)
		}
	}
	b.WriteString(`
Create or repair the required output files now. Follow the previous step instructions for each file's content and schema.
For task result JSON, use this schema:
{"status":"done|in_progress|blocked","notes":"brief phase notes"}
For peer review JSON, use this schema:
{"decision":"approved|changes_requested","summary":"brief summary","findings":["actionable finding","..."]}
If the task is complete, record the appropriate done/approved artifact. If more work is required or the task is blocked, write the required artifact with the accurate in_progress or blocked status instead of leaving it absent.
Do not edit vibedrive-plan.yaml or make unrelated changes.`)
	return b.String()
}

func requiredOutputIssuePaths(issues []requiredOutputIssue, missing bool) []string {
	paths := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Missing == missing {
			paths = append(paths, issue.Path)
		}
	}
	return paths
}

func requiredOutputInvalidIssues(issues []requiredOutputIssue) []requiredOutputIssue {
	invalid := make([]requiredOutputIssue, 0, len(issues))
	for _, issue := range issues {
		if !issue.Missing {
			invalid = append(invalid, issue)
		}
	}
	return invalid
}

func taskExecutionPaths(cfg *config.Config, taskID string, iteration int, isolated bool) TaskExecutionPaths {
	if cfg == nil {
		cfg = &config.Config{}
	}

	if !isolated {
		artifacts := automation.WorkspaceArtifactPaths(cfg.Workspace, taskID)
		return TaskExecutionPaths{
			TaskID:         taskID,
			Name:           "main",
			Workspace:      cfg.Workspace,
			PlanFile:       cfg.PlanFile,
			ArtifactRoot:   artifacts.RootDir,
			TaskResultPath: artifacts.ResultPath,
			TaskNotesPath:  artifacts.TaskNotesPath,
			ReviewPath:     artifacts.ReviewPath,
		}
	}

	name := isolatedWorkspaceName(cfg, taskID, iteration)
	worktreeRoot := cfg.ParallelWorktreeRoot()
	artifactBaseRoot := cfg.ParallelArtifactRoot()
	artifactRoot := filepath.Join(artifactBaseRoot, name)
	workspace := filepath.Join(worktreeRoot, name)
	artifacts := automation.IsolatedArtifactPaths(artifactRoot, taskID)

	return TaskExecutionPaths{
		TaskID:           taskID,
		Name:             name,
		Workspace:        workspace,
		PlanFile:         planFileInWorkspace(cfg.Workspace, cfg.PlanFile, workspace),
		WorktreeRoot:     worktreeRoot,
		ArtifactBaseRoot: artifactBaseRoot,
		ArtifactRoot:     artifactRoot,
		TaskResultPath:   artifacts.ResultPath,
		TaskNotesPath:    artifacts.TaskNotesPath,
		ReviewPath:       artifacts.ReviewPath,
		Isolated:         true,
	}
}

func isolatedWorkspaceName(cfg *config.Config, taskID string, iteration int) string {
	if iteration < 1 {
		iteration = 1
	}
	slug := taskIDSlug(taskID)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		filepath.Clean(cfg.Workspace),
		filepath.Clean(cfg.PlanFile),
		strconv.Itoa(iteration),
		taskID,
	}, "\x00")))
	return fmt.Sprintf("%03d-%s-%s", iteration, slug, hex.EncodeToString(sum[:])[:12])
}

func taskIDSlug(taskID string) string {
	taskID = strings.ToLower(strings.TrimSpace(taskID))
	var b strings.Builder
	lastDash := false
	for _, r := range taskID {
		isAlpha := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if isAlpha || isDigit {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "task"
	}
	if len(slug) > 48 {
		slug = strings.TrimRight(slug[:48], "-")
		if slug == "" {
			slug = "task"
		}
	}
	return slug
}

func planFileInWorkspace(rootWorkspace, rootPlanFile, workspace string) string {
	rootPlanFile = strings.TrimSpace(rootPlanFile)
	if rootPlanFile == "" {
		return ""
	}

	rel, err := filepath.Rel(filepath.Clean(rootWorkspace), filepath.Clean(rootPlanFile))
	if err != nil || !isLocalRelativePath(rel) {
		return filepath.Join(workspace, filepath.Base(rootPlanFile))
	}
	return filepath.Join(workspace, rel)
}

func cleanupTaskExecutionPaths(paths TaskExecutionPaths) error {
	if !paths.Isolated {
		return nil
	}

	workspace, err := ownedChildPath(paths.WorktreeRoot, paths.Workspace)
	if err != nil {
		return err
	}
	artifactRoot, err := ownedChildPath(paths.ArtifactBaseRoot, paths.ArtifactRoot)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(workspace); err != nil {
		return err
	}
	return os.RemoveAll(artifactRoot)
}

func ownedChildPath(root, target string) (string, error) {
	root = strings.TrimSpace(root)
	target = strings.TrimSpace(target)
	if root == "" || target == "" {
		return "", fmt.Errorf("refusing to remove isolated workspace with empty root or target")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return "", err
	}
	if !isLocalRelativePath(rel) {
		return "", fmt.Errorf("refusing to remove %s outside isolation root %s", absTarget, absRoot)
	}
	return absTarget, nil
}

func isLocalRelativePath(path string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == ".." || filepath.IsAbs(path) {
		return false
	}
	return !strings.HasPrefix(path, ".."+string(os.PathSeparator))
}

func ensurePlanArtifactDirectories(data TemplateData) error {
	return prepareOutputDirectories([]string{data.TaskResultPath, data.ReviewPath})
}

func taskProgressSignature(task plan.Task, notesFile *tasknotes.File) string {
	note := strings.TrimSpace(task.Notes)
	if noteEntry, ok := notesFile.Find(task.ID); ok {
		note = strings.TrimSpace(noteEntry.Notes)
	}
	return fmt.Sprintf("%s:%s:%s", task.ID, strings.TrimSpace(task.Status), note)
}

func renderRequiredOutputs(outputs []string, data TemplateData, workspace string) ([]string, error) {
	if len(outputs) == 0 {
		return nil, nil
	}

	rendered, err := render.Strings(outputs, data)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(rendered))
	normalized := make([]string, 0, len(rendered))
	for _, path := range rendered {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, path)
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		normalized = append(normalized, path)
	}

	return normalized, nil
}

func prepareOutputDirectories(paths []string) error {
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}

	return nil
}

func verifyRequiredOutputs(stepName string, paths []string) error {
	missing, err := missingRequiredOutputs(paths)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	return requiredOutputsMissingError(stepName, missing)
}

func inspectRequiredOutputs(paths []string, data TemplateData) ([]requiredOutputIssue, error) {
	issues := make([]requiredOutputIssue, 0)
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				issues = append(issues, requiredOutputIssue{
					Path:    path,
					Problem: "file is missing",
					Missing: true,
				})
				continue
			}
			return nil, fmt.Errorf("stat required output %q: %w", path, err)
		}
		if info.IsDir() {
			issues = append(issues, requiredOutputIssue{
				Path:    path,
				Problem: "path is a directory; expected a file",
			})
			continue
		}

		switch {
		case sameRequiredOutputPath(path, data.TaskResultPath):
			if err := validateTaskResultOutput(path); err != nil {
				issues = append(issues, requiredOutputIssue{Path: path, Problem: err.Error()})
			}
		case sameRequiredOutputPath(path, data.ReviewPath):
			if err := validateReviewOutput(path); err != nil {
				issues = append(issues, requiredOutputIssue{Path: path, Problem: err.Error()})
			}
		}
	}
	return issues, nil
}

func sameRequiredOutputPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(absA) == filepath.Clean(absB)
}

func validateTaskResultOutput(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var result automation.TaskResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("malformed task result JSON: %w", err)
	}
	if normalizeCollectedTaskStatus(result.Status) == "" {
		return fmt.Errorf("task result JSON has unsupported status %q; expected done, in_progress, blocked, or manual", result.Status)
	}
	return nil
}

type reviewOutput struct {
	Decision string   `json:"decision"`
	Summary  string   `json:"summary"`
	Findings []string `json:"findings"`
}

func validateReviewOutput(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var review reviewOutput
	if err := json.Unmarshal(data, &review); err != nil {
		return fmt.Errorf("malformed peer review JSON: %w", err)
	}
	switch strings.TrimSpace(strings.ToLower(review.Decision)) {
	case "approved", "changes_requested":
		return nil
	default:
		return fmt.Errorf("peer review JSON has unsupported decision %q; expected approved or changes_requested", review.Decision)
	}
}

func missingRequiredOutputs(paths []string) ([]string, error) {
	missing := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, path)
				continue
			}
			return nil, fmt.Errorf("stat required output %q: %w", path, err)
		}
	}
	return missing, nil
}

func requiredOutputsMissingError(stepName string, missing []string) error {
	switch len(missing) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("step %q did not produce required output %s", stepName, missing[0])
	default:
		return fmt.Errorf("step %q did not produce required outputs %s", stepName, strings.Join(missing, ", "))
	}
}

func requiredOutputsIssueError(stepName string, issues []requiredOutputIssue) error {
	if len(issues) == 0 {
		return nil
	}
	missing := requiredOutputIssuePaths(issues, true)
	if len(missing) == len(issues) {
		return requiredOutputsMissingError(stepName, missing)
	}
	if len(issues) == 1 {
		issue := issues[0]
		if issue.Missing {
			return requiredOutputsMissingError(stepName, []string{issue.Path})
		}
		return fmt.Errorf("step %q produced invalid required output %s: %s", stepName, issue.Path, issue.Problem)
	}

	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Missing {
			parts = append(parts, issue.Path+" is missing")
			continue
		}
		parts = append(parts, issue.Path+": "+issue.Problem)
	}
	return fmt.Errorf("step %q did not produce valid required outputs: %s", stepName, strings.Join(parts, "; "))
}

func (r *Runner) stepAgent(step config.Step) (string, error) {
	switch strings.ToLower(step.Type) {
	case config.StepTypeClaude:
		return config.AgentClaude, nil
	case config.StepTypeCodex:
		return config.AgentCodex, nil
	case config.StepTypeAgent:
		switch strings.ToLower(step.Actor) {
		case config.StepActorCoder:
			return r.cfg.CoderAgent(), nil
		case config.StepActorReviewer:
			return r.cfg.ReviewerAgent(), nil
		default:
			return "", fmt.Errorf("agent step %q has unsupported actor %q", step.Name, step.Actor)
		}
	case config.StepTypeExec:
		return config.StepTypeExec, nil
	default:
		return "", fmt.Errorf("unsupported step type %q", step.Type)
	}
}

func (r *Runner) shouldLogProgress() bool {
	if r.cfg.DryRun {
		return true
	}
	if r.claude != nil && r.claude.IsFullscreenTUI() {
		return false
	}
	if r.codex != nil && r.codex.IsFullscreenTUI() {
		return false
	}
	return true
}

func (r *Runner) stepsForTask(task plan.Task) ([]config.Step, string, error) {
	if len(r.cfg.Workflows) == 0 {
		if len(r.cfg.Steps) == 0 {
			return nil, "", fmt.Errorf("no steps configured")
		}
		return r.cfg.Steps, "default", nil
	}

	workflowName := strings.TrimSpace(task.Workflow)
	if workflowName == "" {
		workflowName = strings.TrimSpace(r.cfg.DefaultWorkflow)
	}
	if workflowName == "" && len(r.cfg.Workflows) == 1 {
		for name := range r.cfg.Workflows {
			workflowName = name
		}
	}
	if workflowName == "" {
		return nil, "", fmt.Errorf("task %q does not declare a workflow and no default_workflow is configured", task.ID)
	}

	workflow, ok := r.cfg.Workflows[workflowName]
	if !ok {
		return nil, "", fmt.Errorf("task %q references unknown workflow %q", task.ID, workflowName)
	}
	return workflow.Steps, workflowName, nil
}

func summarizeUnfinishedTasks(tasks []plan.Task) string {
	if len(tasks) == 0 {
		return "none"
	}

	parts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		parts = append(parts, fmt.Sprintf("%s(%s)", task.ID, task.Status))
	}
	return strings.Join(parts, ", ")
}

func summarizeTaskIDs(tasks []plan.Task) string {
	if len(tasks) == 0 {
		return "none"
	}

	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return strings.Join(ids, ", ")
}

func writePromptPreview(w io.Writer, prompt string) {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return
	}

	for _, line := range strings.Split(trimmed, "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}
