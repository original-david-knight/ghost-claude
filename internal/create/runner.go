package create

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"vibedrive/internal/agentlaunch"
)

const designFileName = "DESIGN.md"

func DesignFileName() string {
	return designFileName
}

type AgentLauncher func(agentType, role string, stdout, stderr io.Writer) (agentlaunch.Runner, error)

type ConfirmFunc func(ctx context.Context, prompt string) (bool, error)

type interactivePromptRunner interface {
	RunInteractivePrompt(ctx context.Context, prompt string) error
}

type StageRunner struct {
	Workspace string
	Launch    AgentLauncher
	Confirm   ConfirmFunc
	Output    io.Writer
}

func NewStageRunner(workspace string, launch AgentLauncher, confirm ConfirmFunc, output io.Writer) StageRunner {
	return StageRunner{
		Workspace: workspace,
		Launch:    launch,
		Confirm:   confirm,
		Output:    output,
	}
}

func (r StageRunner) Run(ctx context.Context, stage Stage, authorAgent, criticAgent string) error {
	if err := r.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	authorPrompt, err := authorPromptForStage(stage)
	if err != nil {
		return err
	}
	stageName, err := stageDisplayName(stage)
	if err != nil {
		return err
	}
	interactiveAuthor := stageUsesInteractiveAuthor(stage)

	output := r.output()
	if err := r.runPhase(ctx, authorAgent, "author", authorPrompt, output, output, interactiveAuthor); err != nil {
		return err
	}
	if err := Write(r.Workspace, State{LastStage: stage}); err != nil {
		return err
	}

	wantsCritic, err := r.Confirm(ctx, fmt.Sprintf("Would you like a second opinion on %s?", stageName))
	if err != nil {
		return err
	}
	if !wantsCritic {
		return nil
	}

	criticPrompt, err := criticPromptForStage(stage)
	if err != nil {
		return err
	}
	var criticFeedback bytes.Buffer
	criticStdout := io.MultiWriter(output, &criticFeedback)
	if err := r.runPhase(ctx, criticAgent, "critic", criticPrompt, criticStdout, output, false); err != nil {
		return err
	}

	followUpPrompt := renderAuthorFollowUpPrompt(criticFeedback.String())
	if err := r.runPhase(ctx, authorAgent, "author", followUpPrompt, output, output, false); err != nil {
		return err
	}
	return Write(r.Workspace, State{LastStage: stage})
}

func (r StageRunner) validate() error {
	if r.Workspace == "" {
		return fmt.Errorf("workspace is required")
	}
	if r.Launch == nil {
		return fmt.Errorf("agent launcher is required")
	}
	if r.Confirm == nil {
		return fmt.Errorf("confirm function is required")
	}
	return nil
}

func (r StageRunner) output() io.Writer {
	if r.Output == nil {
		return io.Discard
	}
	return r.Output
}

func (r StageRunner) runPhase(ctx context.Context, agentType, role, prompt string, stdout, stderr io.Writer, interactive bool) error {
	runner, err := r.Launch(agentType, role, stdout, stderr)
	if err != nil {
		return err
	}

	runErr := runStagePrompt(ctx, runner, prompt, stdout, interactive)
	closeErr := runner.Close()
	if runErr != nil {
		if closeErr != nil {
			return fmt.Errorf("%w; also failed to close %s phase: %v", runErr, role, closeErr)
		}
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}

	return nil
}

func runStagePrompt(ctx context.Context, runner agentlaunch.Runner, prompt string, output io.Writer, interactive bool) error {
	if !interactive {
		return runner.RunPrompt(ctx, prompt)
	}

	interactiveRunner, ok := runner.(interactivePromptRunner)
	if !ok {
		return runner.RunPrompt(ctx, prompt)
	}

	if output != nil {
		fmt.Fprintln(output)
		fmt.Fprintln(output, "This author stage is interactive. Exit the agent TUI after DESIGN.md is updated to return to Vibedrive.")
		fmt.Fprintln(output, "For Codex this is usually Ctrl-D; for Claude, type /exit.")
	}
	return interactiveRunner.RunInteractivePrompt(ctx, promptWithInteractiveExitInstructions(prompt))
}

func stageUsesInteractiveAuthor(stage Stage) bool {
	switch stage {
	case StageProductDefinition, StageFeatureRefactor:
		return true
	default:
		return false
	}
}

func authorPromptForStage(stage Stage) (string, error) {
	switch stage {
	case StageProductDefinition:
		return ProductDefinitionAuthor, nil
	case StageFeatureRefactor:
		return FeatureRefactorAuthor, nil
	case StageUXReview:
		return UXReviewAuthor, nil
	case StageTechnicalReview:
		return TechnicalReviewAuthor, nil
	default:
		return "", fmt.Errorf("unsupported create stage %q", stage)
	}
}

func criticPromptForStage(stage Stage) (string, error) {
	switch stage {
	case StageProductDefinition:
		return ProductDefinitionCritic, nil
	case StageFeatureRefactor:
		return FeatureRefactorCritic, nil
	case StageUXReview:
		return UXReviewCritic, nil
	case StageTechnicalReview:
		return TechnicalReviewCritic, nil
	default:
		return "", fmt.Errorf("unsupported create stage %q", stage)
	}
}

func stageDisplayName(stage Stage) (string, error) {
	switch stage {
	case StageProductDefinition:
		return "Product Definition", nil
	case StageFeatureRefactor:
		return "Feature/Refactor", nil
	case StageUXReview:
		return "UX Review", nil
	case StageTechnicalReview:
		return "Technical Review", nil
	default:
		return "", fmt.Errorf("unsupported create stage %q", stage)
	}
}

func renderAuthorFollowUpPrompt(criticFeedback string) string {
	return fmt.Sprintf("%s\n\nCritic feedback:\n%s", AuthorFollowUpFromCritic, criticFeedback)
}

func promptWithInteractiveExitInstructions(prompt string) string {
	return fmt.Sprintf(`%s

When DESIGN.md is updated and this stage is complete, tell the user to exit the agent TUI to return to Vibedrive. For Codex this is usually Ctrl-D; for Claude, type /exit.
`, prompt)
}
