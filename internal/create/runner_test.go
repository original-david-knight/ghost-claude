package create

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/agentlaunch"
	"vibedrive/internal/config"
)

type stageRunEvent struct {
	Kind      string
	Role      string
	AgentType string
	ID        int
}

type stagePrompt struct {
	Role      string
	AgentType string
	ID        int
	Prompt    string
}

type fakeStageLauncher struct {
	nextID  int
	events  []stageRunEvent
	prompts []stagePrompt
	onRun   func(ctx context.Context, role string, id int, prompt string, stdout io.Writer) error
}

func (f *fakeStageLauncher) launch(agentType, role string, stdout, stderr io.Writer) (agentlaunch.Runner, error) {
	f.nextID++
	id := f.nextID
	f.events = append(f.events, stageRunEvent{Kind: "launch", Role: role, AgentType: agentType, ID: id})
	return &fakeStagePhaseRunner{
		parent:    f,
		id:        id,
		role:      role,
		agentType: agentType,
		stdout:    stdout,
	}, nil
}

type fakeStagePhaseRunner struct {
	parent    *fakeStageLauncher
	id        int
	role      string
	agentType string
	stdout    io.Writer
}

func (f *fakeStagePhaseRunner) RunPrompt(ctx context.Context, prompt string) error {
	f.parent.events = append(f.parent.events, stageRunEvent{Kind: "prompt", Role: f.role, AgentType: f.agentType, ID: f.id})
	f.parent.prompts = append(f.parent.prompts, stagePrompt{Role: f.role, AgentType: f.agentType, ID: f.id, Prompt: prompt})
	if f.parent.onRun != nil {
		return f.parent.onRun(ctx, f.role, f.id, prompt, f.stdout)
	}
	return ctx.Err()
}

func (f *fakeStagePhaseRunner) Close() error {
	f.parent.events = append(f.parent.events, stageRunEvent{Kind: "close", Role: f.role, AgentType: f.agentType, ID: f.id})
	return nil
}

type fakeInteractiveStageLauncher struct {
	nextID  int
	events  []stageRunEvent
	prompts []stagePrompt
}

func (f *fakeInteractiveStageLauncher) launch(agentType, role string, stdout, stderr io.Writer) (agentlaunch.Runner, error) {
	f.nextID++
	id := f.nextID
	f.events = append(f.events, stageRunEvent{Kind: "launch", Role: role, AgentType: agentType, ID: id})
	return &fakeInteractiveStagePhaseRunner{
		parent:    f,
		id:        id,
		role:      role,
		agentType: agentType,
	}, nil
}

type fakeInteractiveStagePhaseRunner struct {
	parent    *fakeInteractiveStageLauncher
	id        int
	role      string
	agentType string
}

func (f *fakeInteractiveStagePhaseRunner) RunPrompt(_ context.Context, prompt string) error {
	f.parent.events = append(f.parent.events, stageRunEvent{Kind: "prompt", Role: f.role, AgentType: f.agentType, ID: f.id})
	f.parent.prompts = append(f.parent.prompts, stagePrompt{Role: f.role, AgentType: f.agentType, ID: f.id, Prompt: prompt})
	return nil
}

func (f *fakeInteractiveStagePhaseRunner) RunInteractivePrompt(_ context.Context, prompt string) error {
	f.parent.events = append(f.parent.events, stageRunEvent{Kind: "interactive_prompt", Role: f.role, AgentType: f.agentType, ID: f.id})
	f.parent.prompts = append(f.parent.prompts, stagePrompt{Role: f.role, AgentType: f.agentType, ID: f.id, Prompt: prompt})
	return nil
}

func (f *fakeInteractiveStagePhaseRunner) Close() error {
	f.parent.events = append(f.parent.events, stageRunEvent{Kind: "close", Role: f.role, AgentType: f.agentType, ID: f.id})
	return nil
}

type confirmRecorder struct {
	answer  bool
	calls   int
	prompts []string
}

func (c *confirmRecorder) confirm(ctx context.Context, prompt string) (bool, error) {
	c.calls++
	c.prompts = append(c.prompts, prompt)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.answer, nil
}

func TestStageRunnerDecliningCriticUpdatesStateAndSkipsCritic(t *testing.T) {
	dir := t.TempDir()
	launcher := &fakeStageLauncher{}
	confirm := &confirmRecorder{answer: false}
	var output bytes.Buffer

	runner := NewStageRunner(dir, launcher.launch, confirm.confirm, &output)
	if err := runner.Run(context.Background(), StageProductDefinition, config.AgentCodex, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	wantEvents := []stageRunEvent{
		{Kind: "launch", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "prompt", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "close", Role: "author", AgentType: config.AgentCodex, ID: 1},
	}
	assertStageEvents(t, launcher.events, wantEvents)
	if len(launcher.prompts) != 1 {
		t.Fatalf("expected one author prompt, got %d", len(launcher.prompts))
	}
	if launcher.prompts[0].Prompt != ProductDefinitionAuthor {
		t.Fatalf("expected Product Definition author prompt, got %q", launcher.prompts[0].Prompt)
	}
	if confirm.calls != 1 {
		t.Fatalf("expected confirm to be called once, got %d", confirm.calls)
	}
	if !strings.Contains(confirm.prompts[0], "Product Definition") {
		t.Fatalf("expected confirm prompt to name the stage, got %q", confirm.prompts[0])
	}
	assertLastStage(t, dir, StageProductDefinition)
	if output.String() != "" {
		t.Fatalf("expected no critic output when critic is declined, got %q", output.String())
	}
}

func TestStageRunnerAuthorPromptWritesOrUpdatesDesign(t *testing.T) {
	dir := t.TempDir()
	designPath := filepath.Join(dir, designFileName)
	if err := os.WriteFile(designPath, []byte("old design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	launcher := &fakeStageLauncher{}
	launcher.onRun = func(_ context.Context, role string, _ int, prompt string, _ io.Writer) error {
		if role != "author" || prompt != ProductDefinitionAuthor {
			return nil
		}
		return os.WriteFile(designPath, []byte("updated design\n"), 0o644)
	}
	confirm := &confirmRecorder{answer: false}

	runner := NewStageRunner(dir, launcher.launch, confirm.confirm, io.Discard)
	if err := runner.Run(context.Background(), StageProductDefinition, config.AgentCodex, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	design, err := os.ReadFile(designPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(design) != "updated design\n" {
		t.Fatalf("expected DESIGN.md to be updated by author prompt, got %q", string(design))
	}
	assertLastStage(t, dir, StageProductDefinition)
}

func TestStageRunnerRerunsUseFreshAuthorInstances(t *testing.T) {
	dir := t.TempDir()
	launcher := &fakeStageLauncher{}
	confirm := &confirmRecorder{answer: false}
	runner := NewStageRunner(dir, launcher.launch, confirm.confirm, io.Discard)

	for i := 0; i < 2; i++ {
		if err := runner.Run(context.Background(), StageUXReview, config.AgentCodex, config.AgentClaude); err != nil {
			t.Fatalf("Run %d returned error: %v", i+1, err)
		}
	}

	if len(launcher.prompts) != 2 {
		t.Fatalf("expected two author prompts, got %d", len(launcher.prompts))
	}
	for idx, prompt := range launcher.prompts {
		wantID := idx + 1
		if prompt.Role != "author" || prompt.ID != wantID {
			t.Fatalf("prompt %d: expected fresh author id %d, got %#v", idx, wantID, prompt)
		}
		if prompt.Prompt != UXReviewAuthor {
			t.Fatalf("prompt %d: expected UX Review author prompt, got %q", idx, prompt.Prompt)
		}
	}
	assertLastStage(t, dir, StageUXReview)
}

func TestStageRunnerUsesInteractivePromptForInterviewStages(t *testing.T) {
	tests := []struct {
		name  string
		stage Stage
	}{
		{name: "product definition", stage: StageProductDefinition},
		{name: "feature refactor", stage: StageFeatureRefactor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			launcher := &fakeInteractiveStageLauncher{}
			confirm := &confirmRecorder{answer: false}
			var output bytes.Buffer

			runner := NewStageRunner(dir, launcher.launch, confirm.confirm, &output)
			if err := runner.Run(context.Background(), tt.stage, config.AgentCodex, config.AgentClaude); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}

			wantEvents := []stageRunEvent{
				{Kind: "launch", Role: "author", AgentType: config.AgentCodex, ID: 1},
				{Kind: "interactive_prompt", Role: "author", AgentType: config.AgentCodex, ID: 1},
				{Kind: "close", Role: "author", AgentType: config.AgentCodex, ID: 1},
			}
			assertStageEvents(t, launcher.events, wantEvents)
			if len(launcher.prompts) != 1 {
				t.Fatalf("expected one author prompt, got %d", len(launcher.prompts))
			}
			if !strings.Contains(launcher.prompts[0].Prompt, "exit the agent TUI to return to Vibedrive") {
				t.Fatalf("expected interactive prompt to include exit instructions, got %q", launcher.prompts[0].Prompt)
			}
			if !strings.Contains(output.String(), "This author stage is interactive") {
				t.Fatalf("expected user-facing interactive instructions, got %q", output.String())
			}
			assertLastStage(t, dir, tt.stage)
		})
	}
}

func TestStageRunnerUsesNormalPromptForAutonomousStages(t *testing.T) {
	dir := t.TempDir()
	launcher := &fakeInteractiveStageLauncher{}
	confirm := &confirmRecorder{answer: false}

	runner := NewStageRunner(dir, launcher.launch, confirm.confirm, io.Discard)
	if err := runner.Run(context.Background(), StageUXReview, config.AgentCodex, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	wantEvents := []stageRunEvent{
		{Kind: "launch", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "prompt", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "close", Role: "author", AgentType: config.AgentCodex, ID: 1},
	}
	assertStageEvents(t, launcher.events, wantEvents)
}

func TestStageRunnerAcceptingCriticRunsFreshCriticAndFollowUpAuthorWithFeedback(t *testing.T) {
	dir := t.TempDir()
	criticFeedback := "critic says clarify onboarding\n"
	launcher := &fakeStageLauncher{}
	launcher.onRun = func(_ context.Context, role string, _ int, prompt string, stdout io.Writer) error {
		if role == "critic" {
			_, err := fmt.Fprint(stdout, criticFeedback)
			return err
		}
		if role == "author" && strings.Contains(prompt, AuthorFollowUpFromCritic) {
			return Write(dir, State{LastStage: StageUXReview})
		}
		return nil
	}
	confirm := &confirmRecorder{answer: true}
	var output bytes.Buffer

	runner := NewStageRunner(dir, launcher.launch, confirm.confirm, &output)
	if err := runner.Run(context.Background(), StageProductDefinition, config.AgentCodex, config.AgentCodex); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	wantEvents := []stageRunEvent{
		{Kind: "launch", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "prompt", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "close", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "launch", Role: "critic", AgentType: config.AgentCodex, ID: 2},
		{Kind: "prompt", Role: "critic", AgentType: config.AgentCodex, ID: 2},
		{Kind: "close", Role: "critic", AgentType: config.AgentCodex, ID: 2},
		{Kind: "launch", Role: "author", AgentType: config.AgentCodex, ID: 3},
		{Kind: "prompt", Role: "author", AgentType: config.AgentCodex, ID: 3},
		{Kind: "close", Role: "author", AgentType: config.AgentCodex, ID: 3},
	}
	assertStageEvents(t, launcher.events, wantEvents)
	if len(launcher.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(launcher.prompts))
	}
	if launcher.prompts[1].Prompt != ProductDefinitionCritic {
		t.Fatalf("expected critic prompt to be used as-is, got %q", launcher.prompts[1].Prompt)
	}
	followUpPrompt := launcher.prompts[2].Prompt
	if !strings.Contains(followUpPrompt, AuthorFollowUpFromCritic) {
		t.Fatalf("expected follow-up prompt to contain author follow-up prompt, got %q", followUpPrompt)
	}
	if !strings.Contains(followUpPrompt, criticFeedback) {
		t.Fatalf("expected follow-up prompt to receive critic feedback %q, got %q", criticFeedback, followUpPrompt)
	}
	if !strings.Contains(output.String(), criticFeedback) {
		t.Fatalf("expected critic output to be written to terminal output, got %q", output.String())
	}
	assertLastStage(t, dir, StageProductDefinition)
}

func TestStageRunnerSelectsMatchingPromptsForAllStages(t *testing.T) {
	tests := []struct {
		name         string
		stage        Stage
		displayName  string
		authorPrompt string
		criticPrompt string
	}{
		{
			name:         "product definition",
			stage:        StageProductDefinition,
			displayName:  "Product Definition",
			authorPrompt: ProductDefinitionAuthor,
			criticPrompt: ProductDefinitionCritic,
		},
		{
			name:         "feature refactor",
			stage:        StageFeatureRefactor,
			displayName:  "Feature/Refactor",
			authorPrompt: FeatureRefactorAuthor,
			criticPrompt: FeatureRefactorCritic,
		},
		{
			name:         "ux review",
			stage:        StageUXReview,
			displayName:  "UX Review",
			authorPrompt: UXReviewAuthor,
			criticPrompt: UXReviewCritic,
		},
		{
			name:         "technical review",
			stage:        StageTechnicalReview,
			displayName:  "Technical Review",
			authorPrompt: TechnicalReviewAuthor,
			criticPrompt: TechnicalReviewCritic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			launcher := &fakeStageLauncher{}
			launcher.onRun = func(_ context.Context, role string, _ int, _ string, stdout io.Writer) error {
				if role == "critic" {
					_, err := fmt.Fprint(stdout, "stage feedback\n")
					return err
				}
				return nil
			}
			confirm := &confirmRecorder{answer: true}

			runner := NewStageRunner(dir, launcher.launch, confirm.confirm, io.Discard)
			if err := runner.Run(context.Background(), tt.stage, config.AgentCodex, config.AgentClaude); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}

			if len(launcher.prompts) != 3 {
				t.Fatalf("expected 3 prompts, got %d", len(launcher.prompts))
			}
			if launcher.prompts[0].Prompt != tt.authorPrompt {
				t.Fatalf("expected author prompt %q, got %q", tt.authorPrompt, launcher.prompts[0].Prompt)
			}
			if launcher.prompts[1].Prompt != tt.criticPrompt {
				t.Fatalf("expected critic prompt to be used as-is, got %q", launcher.prompts[1].Prompt)
			}
			if !strings.Contains(launcher.prompts[2].Prompt, "stage feedback") {
				t.Fatalf("expected follow-up prompt to include critic feedback, got %q", launcher.prompts[2].Prompt)
			}
			if confirm.calls != 1 || !strings.Contains(confirm.prompts[0], tt.displayName) {
				t.Fatalf("expected one confirm prompt naming %s, got %#v", tt.displayName, confirm.prompts)
			}
			assertLastStage(t, dir, tt.stage)
		})
	}
}

func TestStageRunnerCancellationPreservesDesignAndPreviouslySucceededState(t *testing.T) {
	tests := []struct {
		name           string
		cancelPhase    string
		wantDesign     string
		wantStage      Stage
		wantConfirmCnt int
	}{
		{
			name:           "initial author",
			cancelPhase:    "initial-author",
			wantDesign:     "canceled author mutation\n",
			wantStage:      StageProductDefinition,
			wantConfirmCnt: 0,
		},
		{
			name:           "critic",
			cancelPhase:    "critic",
			wantDesign:     "canceled critic mutation\n",
			wantStage:      StageTechnicalReview,
			wantConfirmCnt: 1,
		},
		{
			name:           "follow-up author",
			cancelPhase:    "follow-up-author",
			wantDesign:     "canceled follow-up mutation\n",
			wantStage:      StageTechnicalReview,
			wantConfirmCnt: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			designPath := filepath.Join(dir, designFileName)
			if err := os.WriteFile(designPath, []byte("existing design\n"), 0o644); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}
			if err := Write(dir, State{LastStage: StageProductDefinition}); err != nil {
				t.Fatalf("Write returned error: %v", err)
			}

			launcher := &fakeStageLauncher{}
			launcher.onRun = func(_ context.Context, role string, _ int, prompt string, stdout io.Writer) error {
				switch {
				case role == "author" && prompt == TechnicalReviewAuthor && tt.cancelPhase == "initial-author":
					if err := os.WriteFile(designPath, []byte("canceled author mutation\n"), 0o644); err != nil {
						return err
					}
					return context.Canceled
				case role == "author" && prompt == TechnicalReviewAuthor:
					return os.WriteFile(designPath, []byte("author success\n"), 0o644)
				case role == "critic" && tt.cancelPhase == "critic":
					if err := os.WriteFile(designPath, []byte("canceled critic mutation\n"), 0o644); err != nil {
						return err
					}
					return context.Canceled
				case role == "critic":
					_, err := fmt.Fprint(stdout, "critic feedback\n")
					return err
				case role == "author" && strings.Contains(prompt, AuthorFollowUpFromCritic) && tt.cancelPhase == "follow-up-author":
					if err := os.WriteFile(designPath, []byte("canceled follow-up mutation\n"), 0o644); err != nil {
						return err
					}
					return context.Canceled
				default:
					return nil
				}
			}
			confirm := &confirmRecorder{answer: true}

			runner := NewStageRunner(dir, launcher.launch, confirm.confirm, io.Discard)
			err := runner.Run(context.Background(), StageTechnicalReview, config.AgentCodex, config.AgentClaude)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context.Canceled, got %v", err)
			}
			design, err := os.ReadFile(designPath)
			if err != nil {
				t.Fatalf("ReadFile returned error: %v", err)
			}
			if string(design) != tt.wantDesign {
				t.Fatalf("expected DESIGN.md %q, got %q", tt.wantDesign, string(design))
			}
			assertLastStage(t, dir, tt.wantStage)
			if confirm.calls != tt.wantConfirmCnt {
				t.Fatalf("expected confirm calls %d, got %d", tt.wantConfirmCnt, confirm.calls)
			}
		})
	}
}

func assertStageEvents(t *testing.T, got, want []stageRunEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %d events, got %d: %#v", len(want), len(got), got)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("event %d: expected %#v, got %#v", idx, want[idx], got[idx])
		}
	}
}

func assertLastStage(t *testing.T, workspace string, want Stage) {
	t.Helper()
	state, err := Read(workspace)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if state.LastStage != want {
		t.Fatalf("expected last_stage %q, got %q", want, state.LastStage)
	}
}
