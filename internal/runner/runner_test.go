package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/automation"
	"vibedrive/internal/config"
	"vibedrive/internal/plan"
	"vibedrive/internal/tasknotes"
	"vibedrive/pkg/agentcli/claude"
	"vibedrive/pkg/agentcli/codex"
)

type fakeAgent struct {
	prompts         []string
	sessionIDs      []string
	closedSessionID []string
	planPath        string
	closeEvents     *[]string
	closeLabel      string
}

func (f *fakeAgent) RunPrompt(_ context.Context, session *claude.Session, prompt string) error {
	f.prompts = append(f.prompts, prompt)
	f.sessionIDs = append(f.sessionIDs, session.ID)

	if strings.HasPrefix(prompt, "write ") {
		return writeOutput(strings.TrimPrefix(prompt, "write "))
	}
	if strings.HasPrefix(prompt, "finish task ") {
		taskID := strings.TrimPrefix(prompt, "finish task ")
		return updateTask(f.planPath, taskID, plan.StatusDone, "done")
	}
	if strings.HasPrefix(prompt, "progress task ") {
		taskID := strings.TrimPrefix(prompt, "progress task ")
		return updateTask(f.planPath, taskID, plan.StatusInProgress, "still working")
	}

	return nil
}

func (f *fakeAgent) Close(session *claude.Session) error {
	f.closedSessionID = append(f.closedSessionID, session.ID)
	if f.closeEvents != nil {
		label := f.closeLabel
		if label == "" {
			label = "claude"
		}
		*f.closeEvents = append(*f.closeEvents, label)
	}
	return nil
}

func (f *fakeAgent) IsFullscreenTUI() bool {
	return false
}

type fakeCodex struct {
	prompts         []string
	closedSessionID []string
	planPath        string
	closeEvents     *[]string
	closeLabel      string
}

func (f *fakeCodex) RunPrompt(_ context.Context, session *codex.Session, prompt string) error {
	f.prompts = append(f.prompts, prompt)

	if strings.HasPrefix(prompt, "write ") {
		return writeOutput(strings.TrimPrefix(prompt, "write "))
	}
	if strings.HasPrefix(prompt, "finish task ") {
		taskID := strings.TrimPrefix(prompt, "finish task ")
		return updateTask(f.planPath, taskID, plan.StatusDone, "done")
	}
	if strings.HasPrefix(prompt, "progress task ") {
		taskID := strings.TrimPrefix(prompt, "progress task ")
		return updateTask(f.planPath, taskID, plan.StatusInProgress, "still working")
	}

	return nil
}

func (f *fakeCodex) Close(_ *codex.Session) error {
	f.closedSessionID = append(f.closedSessionID, "closed")
	if f.closeEvents != nil {
		label := f.closeLabel
		if label == "" {
			label = "codex"
		}
		*f.closeEvents = append(*f.closeEvents, label)
	}
	return nil
}

func (f *fakeCodex) IsFullscreenTUI() bool {
	return false
}

func TestRunExecutesReadyPlanTasksByDependencyOrder(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
  - id: inspect
    title: Implement inspect
    workflow: implement
    status: todo
    deps:
      - scaffold
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "analyze", Type: config.StepTypeClaude, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeClaude, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	agent := &fakeAgent{planPath: planPath}
	sessionCount := 0
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: agent,
		newSession: func(_ string) (*claude.Session, error) {
			sessionCount++
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-" + string(rune('0'+sessionCount)),
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if sessionCount != 2 {
		t.Fatalf("expected 2 sessions, got %d", sessionCount)
	}

	loaded, err := plan.Load(planPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	for _, task := range loaded.Tasks {
		if task.Status != plan.StatusDone {
			t.Fatalf("expected task %q to be done, got %q", task.ID, task.Status)
		}
	}

	wantPrompts := []string{
		"analyze scaffold",
		"finish task scaffold",
		"analyze inspect",
		"finish task inspect",
	}
	if strings.Join(agent.prompts, "\n") != strings.Join(wantPrompts, "\n") {
		t.Fatalf("unexpected prompts:\n%s", strings.Join(agent.prompts, "\n"))
	}
}

func TestRunExplainsStalledPlanProgress(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "analyze", Type: config.StepTypeClaude, Prompt: "analyze {{ .Task.ID }}"},
				},
			},
		},
	}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: &fakeAgent{planPath: planPath},
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to fail when the plan task does not change")
	}

	message := err.Error()
	if !strings.Contains(message, "made no task progress") {
		t.Fatalf("expected plan stall explanation, got %q", message)
	}
	if !strings.Contains(message, "status") {
		t.Fatalf("expected plan stall error to mention status, got %q", message)
	}
}

func TestRunDispatchesCodexPlanStepsWithoutChangingWorkflowNames(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeCodex, Prompt: "finish task {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeClaude, Prompt: "review {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(codexAgent.prompts, "\n"); got != "finish task scaffold" {
		t.Fatalf("unexpected codex prompts:\n%s", got)
	}
	if got := strings.Join(claudeAgent.prompts, "\n"); got != "review scaffold" {
		t.Fatalf("unexpected claude prompts:\n%s", got)
	}
}

func TestRunStepLogsCodexPromptPreview(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer

	r := &Runner{
		cfg: &config.Config{
			Workspace: dir,
		},
		stdout: &stdout,
		stderr: io.Discard,
		codex:  &fakeCodex{},
	}

	err := r.runStep(context.Background(), nil, nil, config.Step{
		Name:   "review",
		Type:   config.StepTypeCodex,
		Prompt: "review {{ .Task.ID }}\ncheck acceptance criteria",
	}, TemplateData{
		Task: plan.Task{ID: "scaffold"},
	})
	if err != nil {
		t.Fatalf("runStep returned error: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, "\n--> codex step: review\n") {
		t.Fatalf("expected codex step header, got %q", got)
	}
	if !strings.Contains(got, "    review scaffold\n") {
		t.Fatalf("expected first prompt line in preview, got %q", got)
	}
	if !strings.Contains(got, "    check acceptance criteria\n") {
		t.Fatalf("expected second prompt line in preview, got %q", got)
	}
}

func TestRunPreparesPlanArtifactDirectories(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "write {{ .ReviewPath }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	reviewPath := automation.ReviewPath(dir, "scaffold")
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatalf("expected review artifact %s to exist, stat err=%v", reviewPath, err)
	}
}

func TestRunFailsWhenRequiredOutputMissing(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "review",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorReviewer,
						Prompt:          "review {{ .Task.ID }}",
						RequiredOutputs: []string{"{{ .ReviewPath }}"},
					},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to fail when a step does not create its required output")
	}
	if !strings.Contains(err.Error(), `step "review" failed: step "review" did not produce required output`) {
		t.Fatalf("expected missing required output error, got %q", err)
	}
	if got := strings.Join(codexAgent.prompts, "\n"); got != "review scaffold" {
		t.Fatalf("expected runner to stop before later steps, got prompts:\n%s", got)
	}
}

func TestRunDispatchesCoderAndReviewerStepsWithClaudeReviewer(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(codexAgent.prompts, "\n"); got != "analyze scaffold\nfinish task scaffold" {
		t.Fatalf("unexpected coder prompts:\n%s", got)
	}
	if got := strings.Join(claudeAgent.prompts, "\n"); got != "review scaffold" {
		t.Fatalf("unexpected reviewer prompts:\n%s", got)
	}
}

func TestRunClosesSharedSessionsInReverseCreationOrder(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	closeEvents := []string{}
	claudeAgent := &fakeAgent{planPath: planPath, closeEvents: &closeEvents}
	codexAgent := &fakeCodex{planPath: planPath, closeEvents: &closeEvents}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
		newCodexSession: func() (*codex.Session, error) {
			return &codex.Session{}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(closeEvents, ","); got != "claude,codex" {
		t.Fatalf("expected shared sessions to close in reverse creation order, got %q", got)
	}
}

func TestRunDispatchesCoderAndReviewerStepsWithCodexReviewer(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentClaude,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(claudeAgent.prompts, "\n"); got != "analyze scaffold\nfinish task scaffold" {
		t.Fatalf("unexpected coder prompts:\n%s", got)
	}
	if got := strings.Join(codexAgent.prompts, "\n"); got != "review scaffold" {
		t.Fatalf("unexpected reviewer prompts:\n%s", got)
	}
}

func TestRunAllowsSameAgentForCoderAndReviewer(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "progress task {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(codexAgent.prompts, "\n"); got != "analyze scaffold\nprogress task scaffold\nfinish task scaffold" {
		t.Fatalf("unexpected codex prompts when coder and reviewer match:\n%s", got)
	}
	if got := strings.Join(claudeAgent.prompts, "\n"); got != "" {
		t.Fatalf("expected claude to stay unused, got prompts:\n%s", got)
	}
}

func TestRunReusesSingleClaudeSessionWhenCoderAndReviewerMatch(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentClaude,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	sessionCount := 0

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		newSession: func(_ string) (*claude.Session, error) {
			sessionCount++
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-" + string(rune('0'+sessionCount)),
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if sessionCount != 1 {
		t.Fatalf("expected 1 shared claude session, got %d", sessionCount)
	}

	wantPromptSessions := []string{"session-1", "session-1", "session-1"}
	if strings.Join(claudeAgent.sessionIDs, ",") != strings.Join(wantPromptSessions, ",") {
		t.Fatalf("expected prompt session IDs %v, got %v", wantPromptSessions, claudeAgent.sessionIDs)
	}

	wantClosedSessions := []string{"session-1"}
	if strings.Join(claudeAgent.closedSessionID, ",") != strings.Join(wantClosedSessions, ",") {
		t.Fatalf("expected closed session IDs %v, got %v", wantClosedSessions, claudeAgent.closedSessionID)
	}
}

func updateTask(path, taskID, status, notes string) error {
	file, err := plan.Load(path)
	if err != nil {
		return err
	}

	for i := range file.Tasks {
		if file.Tasks[i].ID == taskID {
			file.Tasks[i].Status = status
			file.Tasks[i].Notes = notes
			if err := file.Save(); err != nil {
				return err
			}

			notesFile, err := tasknotes.Load(tasknotes.Path(filepath.Dir(path)))
			if err != nil {
				return err
			}
			if err := notesFile.Upsert(taskID, status, notes); err != nil {
				return err
			}
			return notesFile.Save()
		}
	}

	return os.ErrNotExist
}

func writeOutput(path string) error {
	return os.WriteFile(path, []byte("{}\n"), 0o644)
}
