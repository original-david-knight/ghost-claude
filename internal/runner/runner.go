package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ghost_claude/internal/automation"
	"ghost_claude/internal/claude"
	codexcli "ghost_claude/internal/codex"
	"ghost_claude/internal/config"
	"ghost_claude/internal/plan"
	"ghost_claude/internal/render"
	"ghost_claude/internal/todo"
)

type Runner struct {
	cfg    *config.Config
	stdout io.Writer
	stderr io.Writer
	claude claudeClient
	codex  codexClient

	executablePath string
	newSession     func(string) (*claude.Session, error)
}

type TemplateData struct {
	ConfigPath     string
	ExecutablePath string
	Iteration      int
	SessionID      string
	TaskResultPath string
	ReviewPath     string
	Workspace      string
	TodoFile       string
	NextTodo       todo.Item
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
	RunPrompt(ctx context.Context, prompt string) error
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

	codexAgent, err := codexcli.New(
		cfg.Codex.Command,
		cfg.Codex.Args,
		cfg.Workspace,
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
	}, nil
}

func (r *Runner) Run(ctx context.Context) (err error) {
	if strings.TrimSpace(r.cfg.PlanFile) != "" {
		return r.runPlan(ctx)
	}
	return r.runTodo(ctx)
}

func (r *Runner) runTodo(ctx context.Context) error {
	stalled := 0

	for iteration := 1; ; iteration++ {
		if r.cfg.MaxIterations > 0 && iteration > r.cfg.MaxIterations {
			return fmt.Errorf("stopped after reaching max_iterations=%d", r.cfg.MaxIterations)
		}

		nextTodo, err := todo.FindNextIncomplete(r.cfg.TodoFile)
		if err != nil {
			if errors.Is(err, todo.ErrNoIncompleteItems) {
				if r.shouldLogProgress() {
					fmt.Fprintln(r.stdout, "No incomplete TODO items remain.")
				}
				return nil
			}
			return err
		}

		if r.shouldLogProgress() {
			fmt.Fprintf(r.stdout, "\n== Iteration %d ==\n", iteration)
			fmt.Fprintf(r.stdout, "Next TODO: %s (line %d)\n", nextTodo.Text, nextTodo.Line)
		}

		data := TemplateData{
			ConfigPath:     r.cfg.Path,
			ExecutablePath: r.executablePath,
			Iteration:      iteration,
			Workspace:      r.cfg.Workspace,
			TodoFile:       r.cfg.TodoFile,
			NextTodo:       nextTodo,
			Now:            time.Now(),
		}

		if err := r.runSteps(ctx, r.cfg.Steps, data); err != nil {
			return err
		}

		if r.cfg.DryRun {
			fmt.Fprintln(r.stdout, "\nDry run complete.")
			return nil
		}

		nextAfterIteration, err := todo.FindNextIncomplete(r.cfg.TodoFile)
		if err != nil {
			if errors.Is(err, todo.ErrNoIncompleteItems) {
				if r.shouldLogProgress() {
					fmt.Fprintln(r.stdout, "\nAll TODO items are complete.")
				}
				return nil
			}
			return err
		}

		if nextAfterIteration.Signature() == nextTodo.Signature() {
			stalled++
			if stalled >= r.cfg.MaxStalledIterations {
				return fmt.Errorf(
					"iteration %d made no TODO progress; %q is still the next incomplete item. "+
						"ghost-claude only advances when the first incomplete checkbox changes in %s. "+
						"This usually means no step edited the TODO file. "+
						"Raise max_stalled_iterations if you want automatic retries on the same item",
					iteration,
					nextAfterIteration.Text,
					r.cfg.TodoFile,
				)
			}
			if r.shouldLogProgress() {
				fmt.Fprintf(r.stderr, "warning: no TODO progress after iteration %d; retrying (%d/%d)\n", iteration, stalled, r.cfg.MaxStalledIterations)
			}
		} else {
			stalled = 0
		}
	}
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
			ReviewPath:     automation.ReviewPath(r.cfg.Workspace, task.ID),
			Workspace:      r.cfg.Workspace,
			TodoFile:       r.cfg.TodoFile,
			PlanFile:       r.cfg.PlanFile,
			Plan:           currentPlan,
			Task:           task,
			Now:            time.Now(),
		}

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

		if updatedTask.ProgressSignature() == task.ProgressSignature() {
			stalled++
			if stalled >= r.cfg.MaxStalledIterations {
				return fmt.Errorf(
					"iteration %d made no task progress; %q (%s) still has status %q in %s. "+
						"The workflow must update the selected task's status or notes when work progresses",
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

func (r *Runner) runSteps(ctx context.Context, steps []config.Step, data TemplateData) error {
	var sharedSession *claude.Session
	closeSharedSession := func(runErr error) error {
		if sharedSession == nil {
			return runErr
		}

		closeErr := r.claude.Close(sharedSession)
		if runErr != nil {
			if closeErr != nil {
				return fmt.Errorf("%w; also failed to close claude session: %v", runErr, closeErr)
			}
			return runErr
		}

		return closeErr
	}

	for _, step := range steps {
		if step.Disabled {
			continue
		}

		err := func() error {
			var (
				target           string
				session          *claude.Session
				closeStepSession bool
				err              error
			)

			target, err = r.stepAgent(step)
			if err != nil {
				return err
			}

			if target == config.AgentClaude {
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
					session = sharedSession
				default:
					session = sharedSession
				}
			}

			runErr := r.runStep(ctx, session, step, data)
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

func (r *Runner) runStep(ctx context.Context, session *claude.Session, step config.Step, data TemplateData) error {
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

	target, err := r.stepAgent(step)
	if err != nil {
		return err
	}

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
		return r.claude.RunPrompt(stepCtx, session, prompt)
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
		}
		if r.cfg.DryRun {
			fmt.Fprintln(r.stdout, strings.TrimSpace(prompt))
			return nil
		}
		return r.codex.RunPrompt(stepCtx, prompt)
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
	if r.claude == nil {
		return true
	}
	return !r.claude.IsFullscreenTUI()
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
