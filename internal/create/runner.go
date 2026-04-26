package create

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"vibedrive/internal/agentlaunch"
)

const designFileName = "DESIGN.md"

type AgentLauncher func(agentType, role string, stdout, stderr io.Writer) (agentlaunch.Runner, error)

type ConfirmFunc func(ctx context.Context, prompt string) (bool, error)

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

	output := r.output()
	if err := r.runPhase(ctx, authorAgent, "author", authorPrompt, output, output); err != nil {
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
	if err := r.runPhase(ctx, criticAgent, "critic", criticPrompt, criticStdout, output); err != nil {
		return err
	}

	followUpPrompt := renderAuthorFollowUpPrompt(criticFeedback.String())
	if err := r.runPhase(ctx, authorAgent, "author", followUpPrompt, output, output); err != nil {
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

func (r StageRunner) runPhase(ctx context.Context, agentType, role, prompt string, stdout, stderr io.Writer) error {
	runner, err := r.Launch(agentType, role, stdout, stderr)
	if err != nil {
		return err
	}

	runErr := runner.RunPrompt(ctx, prompt)
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

func authorPromptForStage(stage Stage) (string, error) {
	switch stage {
	case StageProductDefinition:
		return ProductDefinitionAuthor, nil
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
