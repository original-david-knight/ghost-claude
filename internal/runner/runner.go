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
	"vibedrive/internal/plan"
	"vibedrive/internal/render"
	"vibedrive/internal/tasknotes"
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
}

type TemplateData struct {
	ConfigPath     string
	ExecutablePath string
	Iteration      int
	SessionID      string
	TaskResultPath string
	TaskNotesPath  string
	ReviewPath     string
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
	if r.cfg.EffectiveParallelism() > 1 && !r.cfg.DryRun {
		if target := r.fullscreenParallelAgent(); target != "" {
			fmt.Fprintf(r.stderr, "warning: parallel execution needs non-fullscreen agent transports; %s is configured for fullscreen TUI, continuing serially\n", target)
			return r.runPlan(ctx)
		}
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
		TaskResultPath: paths.TaskResultPath,
		TaskNotesPath:  paths.TaskNotesPath,
		ReviewPath:     paths.ReviewPath,
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

type preparedParallelTask struct {
	Task             plan.Task
	Paths            TaskExecutionPaths
	Steps            []config.Step
	IsolatedPlan     *plan.File
	IsolatedTask     plan.Task
	InitialSignature string
	BaseRevision     string
}

func (r *Runner) runParallelBatch(ctx context.Context, currentPlan *plan.File, tasks []plan.Task, iteration int) ([]parallelTaskResult, error) {
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
	steps, _, err := r.stepsForTask(task)
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
		TaskResultPath: work.Paths.TaskResultPath,
		TaskNotesPath:  work.Paths.TaskNotesPath,
		ReviewPath:     work.Paths.ReviewPath,
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
	if result.Paths.Isolated {
		defer r.cleanupIntegratedParallelTask(ctx, result.Paths)
	}

	if result.Err != nil {
		notes := appendIntegrationNote(result.Notes, "Parallel worker failed: "+shortError(result.Err)+".")
		if err := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); err != nil {
			return fmt.Errorf("record worker failure: %w", err)
		}
		return result.Err
	}

	status := normalizeCollectedTaskStatus(result.Status)
	if status == "" {
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
		notes := appendIntegrationNote(result.Notes, "Parallel worker changes could not be collected: "+shortError(err)+".")
		if recordErr := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); recordErr != nil {
			return fmt.Errorf("%w; also failed to record follow-up: %v", err, recordErr)
		}
		return err
	}

	applied := false
	if len(bytes.TrimSpace(patch)) > 0 {
		if err := gitApplyPatch(ctx, r.cfg.Workspace, patch, true, false); err != nil {
			notes := appendIntegrationNote(result.Notes, "Parallel worker changes did not apply cleanly: "+shortError(err)+".")
			if recordErr := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); recordErr != nil {
				return fmt.Errorf("%w; also failed to record follow-up: %v", err, recordErr)
			}
			return err
		}
		if err := gitApplyPatch(ctx, r.cfg.Workspace, patch, false, false); err != nil {
			notes := appendIntegrationNote(result.Notes, "Parallel worker changes did not apply cleanly: "+shortError(err)+".")
			if recordErr := r.recordParallelTaskStatus(ctx, result.Task, plan.StatusInProgress, notes); recordErr != nil {
				return fmt.Errorf("%w; also failed to record follow-up: %v", err, recordErr)
			}
			return err
		}
		applied = true
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
		return nil
	}

	if applied && status == plan.StatusDone {
		if reverseErr := gitApplyPatch(ctx, r.cfg.Workspace, patch, false, true); reverseErr != nil {
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
	args := []string{"add", "-A", "--", "."}
	args = append(args, excludes...)
	if err := runGitInWorkspace(ctx, result.Paths.Workspace, args...); err != nil {
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
		claudeAgent, err := claude.New(
			workerCfg.Claude.Command,
			workerCfg.Claude.Args,
			workerCfg.Workspace,
			workerCfg.Claude.Transport,
			workerCfg.Claude.StartupTimeout,
			r.stdout,
			r.stderr,
		)
		if err != nil {
			return nil, err
		}
		worker.claude = claudeAgent
	}
	if needsCodex {
		codexAgent, err := codex.New(
			workerCfg.Codex.Command,
			workerCfg.Codex.Args,
			workerCfg.Workspace,
			workerCfg.Codex.Transport,
			workerCfg.Codex.StartupTimeout,
			r.stdout,
			r.stderr,
		)
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

func (r *Runner) fullscreenParallelAgent() string {
	needsClaude, needsCodex, err := r.configuredAgentTargets()
	if err != nil {
		return ""
	}
	if needsClaude && r.claude != nil && r.claude.IsFullscreenTUI() {
		return config.AgentClaude
	}
	if needsCodex && r.codex != nil && r.codex.IsFullscreenTUI() {
		return config.AgentCodex
	}
	return ""
}

func (r *Runner) configuredAgentTargets() (bool, bool, error) {
	var needsClaude, needsCodex bool
	merge := func(steps []config.Step) error {
		stepNeedsClaude, stepNeedsCodex, err := r.agentTargetsForSteps(steps)
		if err != nil {
			return err
		}
		needsClaude = needsClaude || stepNeedsClaude
		needsCodex = needsCodex || stepNeedsCodex
		return nil
	}

	if err := merge(r.cfg.Steps); err != nil {
		return false, false, err
	}
	for _, workflow := range r.cfg.Workflows {
		if err := merge(workflow.Steps); err != nil {
			return false, false, err
		}
	}
	return needsClaude, needsCodex, nil
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
		":(exclude).vibedrive/task-notes.yaml",
		":(exclude).vibedrive/task-results/**",
		":(exclude).vibedrive/reviews/**",
		":(exclude).vibedrive/task-runs/**",
		":(exclude).vibedrive/worktrees/**",
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

	for _, step := range steps {
		if step.Disabled {
			continue
		}

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
			stepData.SessionID = session.ID

			prompt, err := render.String(step.Prompt, stepData)
			if err != nil {
				return fmt.Errorf("render prompt: %w", err)
			}

			if r.shouldLogProgress() {
				fmt.Fprintf(r.stdout, "\n--> claude step: %s\n", step.Name)
			}
			if r.cfg.DryRun {
				fmt.Fprintln(r.stdout, strings.TrimSpace(prompt))
				return nil
			}
			return r.runAgentPrompt(stepCtx, target, session, codexSession, step.Name, prompt)
		case config.AgentCodex:
			if r.codex == nil {
				return fmt.Errorf("codex step %q requires a codex client", step.Name)
			}

			prompt, err := render.String(step.Prompt, data)
			if err != nil {
				return fmt.Errorf("render prompt: %w", err)
			}

			if r.shouldLogProgress() {
				fmt.Fprintf(r.stdout, "\n--> codex step: %s\n", step.Name)
				writePromptPreview(r.stdout, prompt)
			}
			if r.cfg.DryRun {
				fmt.Fprintln(r.stdout, strings.TrimSpace(prompt))
				return nil
			}
			return r.runAgentPrompt(stepCtx, target, session, codexSession, step.Name, prompt)
		case config.StepTypeExec:
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
			cmd.Stdout = r.stdout
			cmd.Stderr = r.stderr
			cmd.Env = os.Environ()
			for key, value := range envMap {
				cmd.Env = append(cmd.Env, key+"="+value)
			}

			if err := cmd.Run(); err != nil {
				return fmt.Errorf("run command: %w", err)
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
	if err := verifyRequiredOutputs(step.Name, requiredOutputs); err != nil {
		return err
	}

	return nil
}

func (r *Runner) runAgentPrompt(ctx context.Context, target string, session *claude.Session, codexSession *codex.Session, stepName, prompt string) error {
	switch target {
	case config.AgentClaude:
		if session == nil {
			return fmt.Errorf("claude step %q requires a session", stepName)
		}
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
		if err := r.runAgentPrompt(ctx, target, session, codexSession, stepName, prompt); err != nil {
			return fmt.Errorf("ask %s to repair task notes YAML after step %q: %w", target, stepName, err)
		}
		if _, err := tasknotes.Load(notesPath); err != nil {
			return fmt.Errorf("task notes YAML is still invalid after repair prompt for step %q: %w", stepName, err)
		}
	}

	return nil
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
	missing := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, path)
				continue
			}
			return fmt.Errorf("stat required output %q: %w", path, err)
		}
	}

	switch len(missing) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("step %q did not produce required output %s", stepName, missing[0])
	default:
		return fmt.Errorf("step %q did not produce required outputs %s", stepName, strings.Join(missing, ", "))
	}
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
