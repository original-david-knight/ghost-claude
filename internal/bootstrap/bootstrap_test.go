package bootstrap

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/agentlaunch"
	"vibedrive/internal/config"
	"vibedrive/pkg/agentcli/claude"
)

type fakeClient struct {
	prompts []string
	closed  bool
}

func (f *fakeClient) RunPrompt(_ context.Context, _ *claude.Session, prompt string) error {
	f.prompts = append(f.prompts, prompt)
	return nil
}

func (f *fakeClient) Close(_ *claude.Session) error {
	f.closed = true
	return nil
}

type phaseEvent struct {
	Kind      string
	Role      string
	AgentType string
	ID        int
}

type capturedPhasePrompt struct {
	Role      string
	AgentType string
	Prompt    string
}

type fakePhaseLauncher struct {
	t            *testing.T
	feedbackPath string
	nextID       int
	events       []phaseEvent
	prompts      []capturedPhasePrompt
}

func (f *fakePhaseLauncher) launch(cfg *config.Config, agentType, role string, stdout, stderr io.Writer) (agentlaunch.Runner, error) {
	f.nextID++
	id := f.nextID
	f.events = append(f.events, phaseEvent{Kind: "launch", Role: role, AgentType: agentType, ID: id})
	return &fakePhaseRunner{
		parent:    f,
		id:        id,
		role:      role,
		agentType: agentType,
	}, nil
}

type fakePhaseRunner struct {
	parent    *fakePhaseLauncher
	id        int
	role      string
	agentType string
}

func (f *fakePhaseRunner) RunPrompt(_ context.Context, prompt string) error {
	f.parent.events = append(f.parent.events, phaseEvent{Kind: "prompt", Role: f.role, AgentType: f.agentType, ID: f.id})
	f.parent.prompts = append(f.parent.prompts, capturedPhasePrompt{Role: f.role, AgentType: f.agentType, Prompt: prompt})
	if f.role == "critic" && f.parent.feedbackPath != "" {
		if err := os.MkdirAll(filepath.Dir(f.parent.feedbackPath), 0o755); err != nil {
			f.parent.t.Fatalf("MkdirAll returned error: %v", err)
		}
		if err := os.WriteFile(f.parent.feedbackPath, []byte("critic feedback\n"), 0o644); err != nil {
			f.parent.t.Fatalf("WriteFile returned error: %v", err)
		}
	}
	return nil
}

func (f *fakePhaseRunner) Close() error {
	f.parent.events = append(f.parent.events, phaseEvent{Kind: "close", Role: f.role, AgentType: f.agentType, ID: f.id})
	return nil
}

func newFakePhaseInitializer(t *testing.T, workspace string) (*Initializer, *fakePhaseLauncher) {
	t.Helper()

	launcher := &fakePhaseLauncher{t: t, feedbackPath: initCriticFeedbackPath(workspace)}
	init := New(io.Discard, io.Discard)
	init.launchAgent = launcher.launch
	return init, launcher
}

func TestInitializerRunWritesConfigAndBootstrapsPlan(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	designPath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(designPath, []byte("# Design\n\nproject constraints\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	launcher := &fakePhaseLauncher{t: t, feedbackPath: initCriticFeedbackPath(dir)}
	init := New(io.Discard, io.Discard)
	init.launchAgent = func(cfg *config.Config, agentType, role string, stdout, stderr io.Writer) (agentlaunch.Runner, error) {
		if cfg.PlanFile != filepath.Join(dir, "vibedrive-plan.yaml") {
			t.Fatalf("expected plan path to resolve under workspace, got %q", cfg.PlanFile)
		}
		return launcher.launch(cfg, agentType, role, stdout, stderr)
	}

	if err := init.Run(context.Background(), configPath, []string{designPath}, false, config.AgentCodex, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	wantEvents := []phaseEvent{
		{Kind: "launch", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "prompt", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "close", Role: "author", AgentType: config.AgentCodex, ID: 1},
		{Kind: "launch", Role: "critic", AgentType: config.AgentClaude, ID: 2},
		{Kind: "prompt", Role: "critic", AgentType: config.AgentClaude, ID: 2},
		{Kind: "close", Role: "critic", AgentType: config.AgentClaude, ID: 2},
		{Kind: "launch", Role: "author", AgentType: config.AgentCodex, ID: 3},
		{Kind: "prompt", Role: "author", AgentType: config.AgentCodex, ID: 3},
		{Kind: "close", Role: "author", AgentType: config.AgentCodex, ID: 3},
	}
	if len(launcher.events) != len(wantEvents) {
		t.Fatalf("expected %d phase events, got %d: %#v", len(wantEvents), len(launcher.events), launcher.events)
	}
	for idx, want := range wantEvents {
		if launcher.events[idx] != want {
			t.Fatalf("event %d: expected %#v, got %#v", idx, want, launcher.events[idx])
		}
	}
	if len(launcher.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(launcher.prompts))
	}

	createPrompt := launcher.prompts[0].Prompt
	criticPrompt := launcher.prompts[1].Prompt
	revisionPrompt := launcher.prompts[2].Prompt

	if launcher.prompts[0].Role != "author" || launcher.prompts[1].Role != "critic" || launcher.prompts[2].Role != "author" {
		t.Fatalf("expected author, critic, author prompt roles, got %#v", launcher.prompts)
	}
	if !strings.Contains(createPrompt, "Create vibedrive-plan.yaml") {
		t.Fatalf("expected first prompt to create the plan file, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "what it learned in that phase") {
		t.Fatalf("expected first prompt to require per-task phase notes, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "keep testing, verification, and cleanup work attached to the implementation task") {
		t.Fatalf("expected first prompt to keep testing and cleanup inline by default, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "agents can verify their own work without manual help") {
		t.Fatalf("expected first prompt to require self-verifying tasks, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "before generating tasks, first identify the repository components") {
		t.Fatalf("expected first prompt to require boundary analysis before task generation, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "context reduction, not merely speed") {
		t.Fatalf("expected first prompt to optimize for context reduction, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "explicit edit authority") {
		t.Fatalf("expected first prompt to require explicit edit authority, got %q", createPrompt)
	}
	for _, want := range []string{"project.components", "component:", "owns_paths:", "reads_contracts:", "provides_contracts:", "conflicts_with:"} {
		if !strings.Contains(createPrompt, want) {
			t.Fatalf("expected first prompt to include boundary metadata %q, got %q", want, createPrompt)
		}
	}
	if !strings.Contains(createPrompt, "cross-cutting implementation task must depend on a preceding contract or foundation task") {
		t.Fatalf("expected first prompt to require foundation tasks before cross-cutting work, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "screenshot instrumentation or seeded test data") {
		t.Fatalf("expected first prompt to include preparatory verification tooling, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "expected to introduce a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface") {
		t.Fatalf("expected first prompt to describe trigger-based tech-debt rules, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "do not claim the plan can know actual changed-file counts or other finalize-time facts before execution") {
		t.Fatalf("expected first prompt to distinguish planning heuristics from finalize-time facts, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "do not add standalone tech-debt tasks on a fixed schedule") {
		t.Fatalf("expected first prompt to reject fixed tech-debt cadence, got %q", createPrompt)
	}
	if strings.Contains(createPrompt, "after every 5 significant dev steps") {
		t.Fatalf("expected first prompt to remove the old tech-debt cadence, got %q", createPrompt)
	}
	if strings.Contains(createPrompt, "Replace the file if it already exists.") {
		t.Fatalf("expected first prompt to omit replace instructions, got %q", createPrompt)
	}
	if !strings.Contains(createPrompt, "DESIGN.md") {
		t.Fatalf("expected first prompt to reference DESIGN.md, got %q", createPrompt)
	}
	if !strings.Contains(criticPrompt, "Perform a critical review of the plan") {
		t.Fatalf("expected second prompt to request a critical plan review, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "do not change vibedrive-plan.yaml") {
		t.Fatalf("expected critic prompt to forbid plan edits, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "capturing phase learnings") {
		t.Fatalf("expected second prompt to review note-capture coverage, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "self-verification path agents can run without manual help") {
		t.Fatalf("expected second prompt to review self-verification paths, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "missing component, ownership, contract, or integration-boundary analysis") {
		t.Fatalf("expected second prompt to review missing boundary analysis, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "excessive context requirements") {
		t.Fatalf("expected second prompt to review excessive context requirements, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "missing interfaces, shared contracts") {
		t.Fatalf("expected second prompt to review missing interfaces and contracts, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "ambiguous ownership or unsafe parallel assumptions") {
		t.Fatalf("expected second prompt to review ambiguous ownership and unsafe parallel assumptions, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "reject tasks that are cross-cutting without a preceding contract or foundation task") {
		t.Fatalf("expected second prompt to reject cross-cutting tasks without foundation tasks, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "screenshot capture") {
		t.Fatalf("expected second prompt to review screenshot instrumentation, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "missing trigger-justified standalone tech-debt tasks") {
		t.Fatalf("expected second prompt to review trigger-based tech-debt gaps, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "plan-time knowledge of actual changed-file counts or other finalize-time facts") {
		t.Fatalf("expected second prompt to review planning-boundary violations, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, "defer routine testing, verification, or cleanup work that should stay attached to implementation") {
		t.Fatalf("expected second prompt to keep routine testing and cleanup inline, got %q", criticPrompt)
	}
	if !strings.Contains(criticPrompt, ".vibedrive/init-critic-feedback.md") {
		t.Fatalf("expected critic prompt to write transient feedback, got %q", criticPrompt)
	}
	lowerCriticPrompt := strings.ToLower(criticPrompt)
	for _, forbidden := range []string{"incorporate", "apply actionable", "revise vibedrive-plan.yaml", "update vibedrive-plan.yaml"} {
		if strings.Contains(lowerCriticPrompt, forbidden) {
			t.Fatalf("expected critic prompt not to contain %q, got %q", forbidden, criticPrompt)
		}
	}
	if strings.Contains(criticPrompt, "required 2 tech-debt tasks after each block of 5 significant dev steps") {
		t.Fatalf("expected second prompt to remove the old tech-debt cadence review, got %q", criticPrompt)
	}
	if strings.Contains(criticPrompt, "/codex") {
		t.Fatalf("expected second prompt to stop requiring /codex, got %q", criticPrompt)
	}
	if !strings.Contains(revisionPrompt, "Revise the generated execution plan") {
		t.Fatalf("expected third prompt to revise the plan, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, ".vibedrive/init-critic-feedback.md") {
		t.Fatalf("expected third prompt to read transient critic feedback, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "Apply actionable critic feedback directly to vibedrive-plan.yaml") {
		t.Fatalf("expected third prompt to apply actionable critic feedback, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "keep testing, verification, and cleanup work attached to the implementation task") {
		t.Fatalf("expected third prompt to preserve inline testing and cleanup, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "self-verification path agents can run without manual help") {
		t.Fatalf("expected third prompt to preserve self-verification paths, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "components, public interfaces, shared contracts, owned paths, integration checkpoints") {
		t.Fatalf("expected third prompt to preserve boundary analysis requirements, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "context reduction, not merely speed") {
		t.Fatalf("expected third prompt to preserve context-reduction requirements, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "owns_paths, reads_contracts, provides_contracts, and conflicts_with metadata") {
		t.Fatalf("expected third prompt to preserve ownership and contract metadata requirements, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "cross-cutting implementation task that lacks a preceding contract or foundation task") {
		t.Fatalf("expected third prompt to reject cross-cutting tasks without foundation tasks, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "screenshot capture") {
		t.Fatalf("expected third prompt to preserve screenshot instrumentation, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "planning-time heuristics about expected breadth and discovered risk") {
		t.Fatalf("expected third prompt to preserve planning-time tech-debt framing, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "do not add standalone tech-debt tasks on a fixed schedule") {
		t.Fatalf("expected third prompt to reject fixed tech-debt cadence, got %q", revisionPrompt)
	}
	if !strings.Contains(revisionPrompt, "short phase notes about what was learned") {
		t.Fatalf("expected third prompt to preserve phase-note acceptance, got %q", revisionPrompt)
	}
	if _, err := os.Stat(launcher.feedbackPath); !os.IsNotExist(err) {
		t.Fatalf("expected transient critic feedback to be removed, stat err=%v", err)
	}
}

func TestInitializerRunUsesExistingConfigWithoutForce(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	sourcePath := filepath.Join(dir, "DESIGN.md")
	configContent := []byte(`workspace: .
plan_file: vibedrive-plan.yaml
steps:
  - name: noop
    type: exec
    command:
      - true
`)

	if err := os.WriteFile(configPath, configContent, 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("existing source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	launcher := &fakePhaseLauncher{t: t, feedbackPath: initCriticFeedbackPath(dir)}
	init := New(io.Discard, io.Discard)
	init.launchAgent = func(cfg *config.Config, agentType, role string, stdout, stderr io.Writer) (agentlaunch.Runner, error) {
		if cfg.PlanFile != filepath.Join(dir, "vibedrive-plan.yaml") {
			t.Fatalf("expected plan path to resolve under existing config workspace, got %q", cfg.PlanFile)
		}
		return launcher.launch(cfg, agentType, role, stdout, stderr)
	}

	if err := init.Run(context.Background(), configPath, []string{sourcePath}, false, config.AgentClaude, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(launcher.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(launcher.prompts))
	}
	if !strings.Contains(launcher.prompts[0].Prompt, "DESIGN.md") {
		t.Fatalf("expected first prompt to reference DESIGN.md, got %q", launcher.prompts[0].Prompt)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !bytes.Equal(content, configContent) {
		t.Fatalf("expected existing config to be preserved, got %q", string(content))
	}
}

func TestInitializerRunSkipsExistingPlanWithoutForce(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	sourcePath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(planPath, []byte("existing plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("existing source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	init, launcher := newFakePhaseInitializer(t, dir)

	if err := init.Run(context.Background(), configPath, []string{sourcePath}, false, config.AgentClaude, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(launcher.prompts) != 0 {
		t.Fatalf("expected no prompts when plan already exists, got %d", len(launcher.prompts))
	}
}

func TestInitializerRunRegeneratesPlanWithForce(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	sourcePath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(planPath, []byte("existing plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("existing source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	init, launcher := newFakePhaseInitializer(t, dir)

	if err := init.Run(context.Background(), configPath, []string{sourcePath}, true, config.AgentClaude, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(launcher.prompts) != 3 {
		t.Fatalf("expected forced init to regenerate the plan, got %d prompts", len(launcher.prompts))
	}
	if !strings.Contains(launcher.prompts[0].Prompt, "vibedrive-plan.yaml") {
		t.Fatalf("expected first prompt to mention the plan path, got %q", launcher.prompts[0].Prompt)
	}
	if _, err := os.Stat(planPath); !os.IsNotExist(err) {
		t.Fatalf("expected existing plan file to be removed before prompting, stat err=%v", err)
	}
}

func TestInitializerRunUsesWorkspaceFilesWhenSourceOmitted(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	if err := os.WriteFile(filepath.Join(dir, "DESIGN.md"), []byte("design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "TEST_PLAN.md"), []byte("tests\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	init, launcher := newFakePhaseInitializer(t, dir)

	if err := init.Run(context.Background(), configPath, nil, false, config.AgentClaude, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(launcher.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(launcher.prompts))
	}
	if !strings.Contains(launcher.prompts[0].Prompt, "- DESIGN.md") {
		t.Fatalf("expected first prompt to include DESIGN.md as a source, got %q", launcher.prompts[0].Prompt)
	}
	if !strings.Contains(launcher.prompts[0].Prompt, "- TEST_PLAN.md") {
		t.Fatalf("expected first prompt to include TEST_PLAN.md as a source, got %q", launcher.prompts[0].Prompt)
	}
	if strings.Contains(launcher.prompts[0].Prompt, "- vibedrive.yaml") {
		t.Fatalf("expected generated config to be excluded from default sources, got %q", launcher.prompts[0].Prompt)
	}
}

func TestInitializerRunRendersResolvedSourcesInSortedOrder(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	docsDir := filepath.Join(dir, "docs")

	if err := os.Mkdir(docsDir, 0o755); err != nil {
		t.Fatalf("Mkdir returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "zeta.md"), []byte("zeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "alpha.md"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	init, launcher := newFakePhaseInitializer(t, dir)

	if err := init.Run(context.Background(), configPath, []string{"docs/zeta.md", "docs"}, false, config.AgentClaude, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(launcher.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(launcher.prompts))
	}

	alphaIndex := strings.Index(launcher.prompts[0].Prompt, "- docs/alpha.md")
	zetaIndex := strings.Index(launcher.prompts[0].Prompt, "- docs/zeta.md")
	if alphaIndex == -1 || zetaIndex == -1 {
		t.Fatalf("expected prompt to include both resolved sources, got %q", launcher.prompts[0].Prompt)
	}
	if alphaIndex > zetaIndex {
		t.Fatalf("expected prompt to render sources in sorted order, got %q", launcher.prompts[0].Prompt)
	}
}

func TestInitializerRunRunsCriticWithFreshInstanceWhenAgentTypesMatch(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	sourcePath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(sourcePath, []byte("design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	init, launcher := newFakePhaseInitializer(t, dir)

	if err := init.Run(context.Background(), configPath, []string{sourcePath}, false, config.AgentCodex, config.AgentCodex); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(launcher.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(launcher.prompts))
	}
	wantRoles := []string{"author", "critic", "author"}
	for idx, wantRole := range wantRoles {
		if launcher.prompts[idx].Role != wantRole {
			t.Fatalf("prompt %d: expected role %q, got %q", idx, wantRole, launcher.prompts[idx].Role)
		}
		if launcher.prompts[idx].AgentType != config.AgentCodex {
			t.Fatalf("prompt %d: expected agent %q, got %q", idx, config.AgentCodex, launcher.prompts[idx].AgentType)
		}
	}

	if launcher.events[2].Kind != "close" || launcher.events[3].Kind != "launch" || launcher.events[2].ID == launcher.events[3].ID {
		t.Fatalf("expected first author instance to close before fresh critic launch, got %#v", launcher.events)
	}
	if launcher.events[5].Kind != "close" || launcher.events[6].Kind != "launch" || launcher.events[5].ID == launcher.events[6].ID {
		t.Fatalf("expected critic instance to close before fresh revision author launch, got %#v", launcher.events)
	}
}

func TestInitializerPrintSourcesResolvesPreviewWithoutWritingConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	if err := os.WriteFile(filepath.Join(dir, "DESIGN.md"), []byte("design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "TEST_PLAN.md"), []byte("tests\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(planPath, []byte("existing plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	var stdout bytes.Buffer
	init := New(&stdout, io.Discard)

	if err := init.PrintSources(configPath, nil); err != nil {
		t.Fatalf("PrintSources returned error: %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "- DESIGN.md") {
		t.Fatalf("expected preview output to include DESIGN.md, got %q", output)
	}
	if !strings.Contains(output, "- TEST_PLAN.md") {
		t.Fatalf("expected preview output to include TEST_PLAN.md, got %q", output)
	}
	if strings.Contains(output, "- vibedrive-plan.yaml") {
		t.Fatalf("expected preview output to exclude the plan file, got %q", output)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("expected PrintSources not to write config, stat err=%v", err)
	}
}

func TestResolveSourcesDedupesAndSortsResolvedFiles(t *testing.T) {
	dir := t.TempDir()
	docsDir := filepath.Join(dir, "docs")

	if err := os.Mkdir(docsDir, 0o755); err != nil {
		t.Fatalf("Mkdir returned error: %v", err)
	}
	alphaPath := filepath.Join(docsDir, "alpha.md")
	betaPath := filepath.Join(docsDir, "beta.md")
	if err := os.WriteFile(alphaPath, []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(betaPath, []byte("beta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	got, err := resolveSources(dir, []string{"docs/beta.md", "docs"})
	if err != nil {
		t.Fatalf("resolveSources returned error: %v", err)
	}

	if len(got.Files) != 2 {
		t.Fatalf("expected 2 unique resolved files, got %d", len(got.Files))
	}
	if got.Files[0] != alphaPath || got.Files[1] != betaPath {
		t.Fatalf("expected sorted files [%q %q], got %v", alphaPath, betaPath, got.Files)
	}
}

func TestResolveSourcesRejectsEmptySelection(t *testing.T) {
	if _, err := resolveSources(t.TempDir(), []string{"   "}); err == nil {
		t.Fatal("expected resolveSources to reject an empty explicit source")
	}
}

func TestResolveSourcesRejectsEmptyDirectorySelection(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	if err := os.WriteFile(configPath, []byte("config\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(planPath, []byte("plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	if _, err := resolveSources(dir, nil, configPath, planPath); err == nil {
		t.Fatal("expected resolveSources to reject a directory with no usable regular files")
	}
}
