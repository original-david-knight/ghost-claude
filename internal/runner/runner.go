package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

		steps, workflowName, err := r.stepsForTask(task)
		if err != nil {
			return err
		}

		if r.shouldLogProgress() {
			fmt.Fprintf(r.stdout, "\n== Iteration %d ==\n", iteration)
			fmt.Fprintf(r.stdout, "Next task: %s (%s) via workflow %s\n", task.Title, task.ID, workflowName)
		}

		data := TemplateData{
			ConfigPath:     r.cfg.Path,
			ExecutablePath: r.executablePath,
			Iteration:      iteration,
			TaskResultPath: automation.ResultPath(r.cfg.Workspace, task.ID),
			TaskNotesPath:  tasknotes.Path(r.cfg.Workspace),
			ReviewPath:     automation.ReviewPath(r.cfg.Workspace, task.ID),
			Workspace:      r.cfg.Workspace,
			PlanFile:       r.cfg.PlanFile,
			Plan:           currentPlan,
			Task:           task,
			Now:            time.Now(),
		}

		if err := ensurePlanArtifactDirectories(data); err != nil {
			return err
		}

		currentNotes, err := tasknotes.Load(data.TaskNotesPath)
		if err != nil {
			return err
		}
		currentSignature := taskProgressSignature(task, currentNotes)

		if err := r.runSteps(ctx, steps, data); err != nil {
			return err
		}

		if r.cfg.DryRun {
			fmt.Fprintln(r.stdout, "\nDry run complete.")
			return nil
		}

		nextPlan, err := plan.Load(r.cfg.PlanFile)
		if err != nil {
			return err
		}

		updatedTask, ok := nextPlan.FindTask(task.ID)
		if !ok {
			return fmt.Errorf("task %q disappeared from %s during iteration %d", task.ID, r.cfg.PlanFile, iteration)
		}

		nextNotes, err := tasknotes.Load(data.TaskNotesPath)
		if err != nil {
			return err
		}

		if taskProgressSignature(updatedTask, nextNotes) == currentSignature {
			stalled++
			if stalled >= r.cfg.MaxStalledIterations {
				return fmt.Errorf(
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
				fmt.Fprintf(r.stderr, "warning: no task progress after iteration %d; retrying (%d/%d)\n", iteration, stalled, r.cfg.MaxStalledIterations)
			}
		} else {
			stalled = 0
		}
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

func writePromptPreview(w io.Writer, prompt string) {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return
	}

	for _, line := range strings.Split(trimmed, "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}
