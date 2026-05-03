package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
	onRun           func(string) error
}

func (f *fakeAgent) RunPrompt(_ context.Context, session *claude.Session, prompt string) error {
	f.prompts = append(f.prompts, prompt)
	f.sessionIDs = append(f.sessionIDs, session.ID)

	if f.onRun != nil {
		if err := f.onRun(prompt); err != nil {
			return err
		}
	}
	return handleFakePrompt(prompt, f.planPath)
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

	return handleFakePrompt(prompt, f.planPath)
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

func TestTaskExecutionPathsUseMainWorkspaceByDefault(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	paths := taskExecutionPaths(&config.Config{
		Workspace: dir,
		PlanFile:  planPath,
	}, "scaffold", 1, false)

	if paths.Isolated {
		t.Fatal("expected default execution paths not to be isolated")
	}
	if paths.Workspace != dir {
		t.Fatalf("expected workspace %q, got %q", dir, paths.Workspace)
	}
	if paths.PlanFile != planPath {
		t.Fatalf("expected plan file %q, got %q", planPath, paths.PlanFile)
	}
	if paths.TaskResultPath != automation.ResultPath(dir, "scaffold") {
		t.Fatalf("expected result path %q, got %q", automation.ResultPath(dir, "scaffold"), paths.TaskResultPath)
	}
	if paths.ReviewPath != automation.ReviewPath(dir, "scaffold") {
		t.Fatalf("expected review path %q, got %q", automation.ReviewPath(dir, "scaffold"), paths.ReviewPath)
	}
	if paths.TaskNotesPath != tasknotes.Path(dir) {
		t.Fatalf("expected task notes path %q, got %q", tasknotes.Path(dir), paths.TaskNotesPath)
	}
}

func TestTaskExecutionPathsBuildDeterministicIsolatedPaths(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plans", "vibedrive-plan.yaml")
	cfg := &config.Config{
		Workspace: dir,
		PlanFile:  planPath,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 4,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
	}

	paths := taskExecutionPaths(cfg, "api/db", 7, true)
	again := taskExecutionPaths(cfg, "api/db", 7, true)
	if paths.Name != again.Name {
		t.Fatalf("expected deterministic workspace name, got %q then %q", paths.Name, again.Name)
	}
	if !regexp.MustCompile(`^007-api-db-[a-f0-9]{12}$`).MatchString(paths.Name) {
		t.Fatalf("unexpected workspace name %q", paths.Name)
	}

	sameSlug := taskExecutionPaths(cfg, "api:db", 7, true)
	if sameSlug.Name == paths.Name {
		t.Fatalf("expected hash suffix to distinguish task IDs with the same slug, got %q", paths.Name)
	}

	wantWorkspace := filepath.Join(cfg.Parallel.WorktreeRoot, paths.Name)
	if paths.Workspace != wantWorkspace {
		t.Fatalf("expected workspace %q, got %q", wantWorkspace, paths.Workspace)
	}
	wantPlanFile := filepath.Join(paths.Workspace, "plans", "vibedrive-plan.yaml")
	if paths.PlanFile != wantPlanFile {
		t.Fatalf("expected isolated plan file %q, got %q", wantPlanFile, paths.PlanFile)
	}
	wantArtifactRoot := filepath.Join(cfg.Parallel.ArtifactRoot, paths.Name)
	if paths.ArtifactRoot != wantArtifactRoot {
		t.Fatalf("expected artifact root %q, got %q", wantArtifactRoot, paths.ArtifactRoot)
	}
	if paths.ArtifactBaseRoot != cfg.Parallel.ArtifactRoot {
		t.Fatalf("expected artifact base root %q, got %q", cfg.Parallel.ArtifactRoot, paths.ArtifactBaseRoot)
	}
	wantResultPath := filepath.Join(wantArtifactRoot, "task-results", "api_db.json")
	if paths.TaskResultPath != wantResultPath {
		t.Fatalf("expected result path %q, got %q", wantResultPath, paths.TaskResultPath)
	}
	wantReviewPath := filepath.Join(wantArtifactRoot, "reviews", "api_db.json")
	if paths.ReviewPath != wantReviewPath {
		t.Fatalf("expected review path %q, got %q", wantReviewPath, paths.ReviewPath)
	}
	wantNotesPath := filepath.Join(wantArtifactRoot, "task-notes.yaml")
	if paths.TaskNotesPath != wantNotesPath {
		t.Fatalf("expected task notes path %q, got %q", wantNotesPath, paths.TaskNotesPath)
	}
}

func TestCleanupTaskExecutionPathsRemovesOnlyOwnedChildren(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := filepath.Join(dir, "worktrees")
	artifactRoot := filepath.Join(dir, "task-runs")
	paths := TaskExecutionPaths{
		Isolated:         true,
		WorktreeRoot:     workspaceRoot,
		Workspace:        filepath.Join(workspaceRoot, "001-task-abcdef123456"),
		ArtifactBaseRoot: artifactRoot,
		ArtifactRoot:     filepath.Join(artifactRoot, "001-task-abcdef123456"),
	}
	if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.MkdirAll(paths.ArtifactRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	if err := cleanupTaskExecutionPaths(paths); err != nil {
		t.Fatalf("cleanupTaskExecutionPaths returned error: %v", err)
	}
	if _, err := os.Stat(paths.Workspace); !os.IsNotExist(err) {
		t.Fatalf("expected workspace to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(paths.ArtifactRoot); !os.IsNotExist(err) {
		t.Fatalf("expected artifact root to be removed, stat err=%v", err)
	}

	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	err := cleanupTaskExecutionPaths(TaskExecutionPaths{
		Isolated:         true,
		WorktreeRoot:     workspaceRoot,
		Workspace:        outside,
		ArtifactBaseRoot: artifactRoot,
		ArtifactRoot:     filepath.Join(artifactRoot, "002-task-abcdef123456"),
	})
	if err == nil {
		t.Fatal("expected cleanup to reject workspace outside owned root")
	}
	if !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("unexpected cleanup error: %v", err)
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("expected outside directory to remain, stat err=%v", statErr)
	}

	outsideArtifact := filepath.Join(dir, "outside-artifact")
	ownedWorkspace := filepath.Join(workspaceRoot, "003-task-abcdef123456")
	if err := os.MkdirAll(ownedWorkspace, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.MkdirAll(outsideArtifact, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	err = cleanupTaskExecutionPaths(TaskExecutionPaths{
		Isolated:         true,
		WorktreeRoot:     workspaceRoot,
		Workspace:        ownedWorkspace,
		ArtifactBaseRoot: artifactRoot,
		ArtifactRoot:     outsideArtifact,
	})
	if err == nil {
		t.Fatal("expected cleanup to reject artifact path outside owned root")
	}
	if _, statErr := os.Stat(outsideArtifact); statErr != nil {
		t.Fatalf("expected outside artifact directory to remain, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(ownedWorkspace); statErr != nil {
		t.Fatalf("expected owned workspace to remain when artifact path is unsafe, stat err=%v", statErr)
	}
}

func TestRunKeepsConfiguredParallelismSerialUntilOrchestrationExists(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: todo
  - id: docs
    title: Docs
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
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 2,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "finish", Type: config.StepTypeClaude, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	var mu sync.Mutex
	active := 0
	maxActive := 0
	agent := &fakeAgent{
		planPath: planPath,
		onRun: func(_ string) error {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
			return nil
		},
	}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: agent,
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

	if maxActive != 1 {
		t.Fatalf("expected configured parallelism to remain serial in this phase, observed %d active prompts", maxActive)
	}
	if got := strings.Join(agent.prompts, "\n"); got != "finish task api\nfinish task docs" {
		t.Fatalf("unexpected prompt order:\n%s", got)
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

func TestRunAsksAgentToRepairInvalidTaskNotesYAML(t *testing.T) {
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
					{Name: "break-notes", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "break task notes {{ .Task.ID }}"},
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

	if len(codexAgent.prompts) != 3 {
		t.Fatalf("expected original, repair, and finish prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if codexAgent.prompts[0] != "break task notes scaffold" {
		t.Fatalf("expected first prompt to break task notes, got %q", codexAgent.prompts[0])
	}
	if !strings.Contains(codexAgent.prompts[1], "does not parse") {
		t.Fatalf("expected repair prompt to explain parse failure, got %q", codexAgent.prompts[1])
	}
	if !strings.Contains(codexAgent.prompts[1], "task-notes.yaml") {
		t.Fatalf("expected repair prompt to name task notes path, got %q", codexAgent.prompts[1])
	}
	if codexAgent.prompts[2] != "finish task scaffold" {
		t.Fatalf("expected runner to continue after notes repair, got %q", codexAgent.prompts[2])
	}

	notesFile, err := tasknotes.Load(tasknotes.Path(dir))
	if err != nil {
		t.Fatalf("expected repaired task notes to parse, got %v", err)
	}
	note, ok := notesFile.Find("scaffold")
	if !ok {
		t.Fatal("expected scaffold task note")
	}
	if note.Status != plan.StatusDone {
		t.Fatalf("expected task note status %q, got %q", plan.StatusDone, note.Status)
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

func handleFakePrompt(prompt, planPath string) error {
	if strings.HasPrefix(prompt, "write ") {
		return writeOutput(strings.TrimPrefix(prompt, "write "))
	}
	if strings.HasPrefix(prompt, "finish task ") {
		taskID := strings.TrimPrefix(prompt, "finish task ")
		return updateTask(planPath, taskID, plan.StatusDone, "done")
	}
	if strings.HasPrefix(prompt, "progress task ") {
		taskID := strings.TrimPrefix(prompt, "progress task ")
		return updateTask(planPath, taskID, plan.StatusInProgress, "still working")
	}
	if strings.HasPrefix(prompt, "break task notes ") {
		taskID := strings.TrimPrefix(prompt, "break task notes ")
		return writeTaskNotes(filepath.Dir(planPath), "tasks:\n  - id: "+taskID+"\n    notes: [broken\n")
	}
	if strings.Contains(prompt, "does not parse") && strings.Contains(prompt, "task-notes.yaml") {
		return writeTaskNotes(filepath.Dir(planPath), "tasks:\n  - id: scaffold\n    status: in_progress\n    notes: repaired\n")
	}

	return nil
}

func writeTaskNotes(workspace, content string) error {
	path := tasknotes.Path(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func writeOutput(path string) error {
	return os.WriteFile(path, []byte("{}\n"), 0o644)
}
