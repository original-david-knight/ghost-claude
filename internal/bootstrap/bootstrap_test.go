package bootstrap

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/claude"
	"vibedrive/internal/config"
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

type fakeBootstrapPlanner struct {
	prompts []string
	closed  bool
}

func (f *fakeBootstrapPlanner) RunPrompt(_ context.Context, prompt string) error {
	f.prompts = append(f.prompts, prompt)
	return nil
}

func (f *fakeBootstrapPlanner) Close() error {
	f.closed = true
	return nil
}

func TestInitializerRunWritesConfigAndBootstrapsPlan(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	designPath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(designPath, []byte("# Design\n\nproject constraints\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	plannerClient := &fakeBootstrapPlanner{}
	init := New(io.Discard, io.Discard)
	init.newPlanner = func(cfg *config.Config, planner string, stdout, stderr io.Writer) (bootstrapPlanner, error) {
		if cfg.PlanFile != filepath.Join(dir, "vibedrive-plan.yaml") {
			t.Fatalf("expected plan path to resolve under workspace, got %q", cfg.PlanFile)
		}
		if planner != config.AgentClaude {
			t.Fatalf("expected planner %q, got %q", config.AgentClaude, planner)
		}
		return plannerClient, nil
	}

	if err := init.Run(context.Background(), configPath, []string{designPath}, false, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(plannerClient.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(plannerClient.prompts))
	}
	if !strings.Contains(plannerClient.prompts[0], "Create vibedrive-plan.yaml") {
		t.Fatalf("expected first prompt to create the plan file, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[0], "what it learned in that phase") {
		t.Fatalf("expected first prompt to require per-task phase notes, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[0], "keep testing, verification, and cleanup work attached to the implementation task") {
		t.Fatalf("expected first prompt to keep testing and cleanup inline by default, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[0], "expected to introduce a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface") {
		t.Fatalf("expected first prompt to describe trigger-based tech-debt rules, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[0], "do not claim the plan can know actual changed-file counts or other finalize-time facts before execution") {
		t.Fatalf("expected first prompt to distinguish planning heuristics from finalize-time facts, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[0], "do not add standalone tech-debt tasks on a fixed schedule") {
		t.Fatalf("expected first prompt to reject fixed tech-debt cadence, got %q", plannerClient.prompts[0])
	}
	if strings.Contains(plannerClient.prompts[0], "after every 5 significant dev steps") {
		t.Fatalf("expected first prompt to remove the old tech-debt cadence, got %q", plannerClient.prompts[0])
	}
	if strings.Contains(plannerClient.prompts[0], "Replace the file if it already exists.") {
		t.Fatalf("expected first prompt to omit replace instructions, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[0], "DESIGN.md") {
		t.Fatalf("expected first prompt to reference DESIGN.md, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[1], "Perform a critical review of the plan") {
		t.Fatalf("expected second prompt to request a critical plan review, got %q", plannerClient.prompts[1])
	}
	if !strings.Contains(plannerClient.prompts[1], "capturing phase learnings") {
		t.Fatalf("expected second prompt to review note-capture coverage, got %q", plannerClient.prompts[1])
	}
	if !strings.Contains(plannerClient.prompts[1], "missing trigger-justified standalone tech-debt tasks") {
		t.Fatalf("expected second prompt to review trigger-based tech-debt gaps, got %q", plannerClient.prompts[1])
	}
	if !strings.Contains(plannerClient.prompts[1], "plan-time knowledge of actual changed-file counts or other finalize-time facts") {
		t.Fatalf("expected second prompt to review planning-boundary violations, got %q", plannerClient.prompts[1])
	}
	if !strings.Contains(plannerClient.prompts[1], "defer routine testing, verification, or cleanup work that should stay attached to implementation") {
		t.Fatalf("expected second prompt to keep routine testing and cleanup inline, got %q", plannerClient.prompts[1])
	}
	if strings.Contains(plannerClient.prompts[1], "required 2 tech-debt tasks after each block of 5 significant dev steps") {
		t.Fatalf("expected second prompt to remove the old tech-debt cadence review, got %q", plannerClient.prompts[1])
	}
	if strings.Contains(plannerClient.prompts[1], "/codex") {
		t.Fatalf("expected second prompt to stop requiring /codex, got %q", plannerClient.prompts[1])
	}
	if !plannerClient.closed {
		t.Fatal("expected planner client to be closed")
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

	plannerClient := &fakeBootstrapPlanner{}
	init := New(io.Discard, io.Discard)
	init.newPlanner = func(cfg *config.Config, planner string, stdout, stderr io.Writer) (bootstrapPlanner, error) {
		if cfg.PlanFile != filepath.Join(dir, "vibedrive-plan.yaml") {
			t.Fatalf("expected plan path to resolve under existing config workspace, got %q", cfg.PlanFile)
		}
		return plannerClient, nil
	}

	if err := init.Run(context.Background(), configPath, []string{sourcePath}, false, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(plannerClient.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(plannerClient.prompts))
	}
	if !strings.Contains(plannerClient.prompts[0], "DESIGN.md") {
		t.Fatalf("expected first prompt to reference DESIGN.md, got %q", plannerClient.prompts[0])
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

	plannerClient := &fakeBootstrapPlanner{}
	init := New(io.Discard, io.Discard)
	init.newPlanner = func(cfg *config.Config, planner string, stdout, stderr io.Writer) (bootstrapPlanner, error) {
		return plannerClient, nil
	}

	if err := init.Run(context.Background(), configPath, []string{sourcePath}, false, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(plannerClient.prompts) != 0 {
		t.Fatalf("expected no prompts when plan already exists, got %d", len(plannerClient.prompts))
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

	plannerClient := &fakeBootstrapPlanner{}
	init := New(io.Discard, io.Discard)
	init.newPlanner = func(cfg *config.Config, planner string, stdout, stderr io.Writer) (bootstrapPlanner, error) {
		return plannerClient, nil
	}

	if err := init.Run(context.Background(), configPath, []string{sourcePath}, true, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(plannerClient.prompts) != 2 {
		t.Fatalf("expected forced init to regenerate the plan, got %d prompts", len(plannerClient.prompts))
	}
	if !strings.Contains(plannerClient.prompts[0], "vibedrive-plan.yaml") {
		t.Fatalf("expected first prompt to mention the plan path, got %q", plannerClient.prompts[0])
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

	plannerClient := &fakeBootstrapPlanner{}
	init := New(io.Discard, io.Discard)
	init.newPlanner = func(cfg *config.Config, planner string, stdout, stderr io.Writer) (bootstrapPlanner, error) {
		return plannerClient, nil
	}

	if err := init.Run(context.Background(), configPath, nil, false, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(plannerClient.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(plannerClient.prompts))
	}
	if !strings.Contains(plannerClient.prompts[0], "- DESIGN.md") {
		t.Fatalf("expected first prompt to include DESIGN.md as a source, got %q", plannerClient.prompts[0])
	}
	if !strings.Contains(plannerClient.prompts[0], "- TEST_PLAN.md") {
		t.Fatalf("expected first prompt to include TEST_PLAN.md as a source, got %q", plannerClient.prompts[0])
	}
	if strings.Contains(plannerClient.prompts[0], "- vibedrive.yaml") {
		t.Fatalf("expected generated config to be excluded from default sources, got %q", plannerClient.prompts[0])
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

	plannerClient := &fakeBootstrapPlanner{}
	init := New(io.Discard, io.Discard)
	init.newPlanner = func(cfg *config.Config, planner string, stdout, stderr io.Writer) (bootstrapPlanner, error) {
		return plannerClient, nil
	}

	if err := init.Run(context.Background(), configPath, []string{"docs/zeta.md", "docs"}, false, config.AgentClaude); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(plannerClient.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(plannerClient.prompts))
	}

	alphaIndex := strings.Index(plannerClient.prompts[0], "- docs/alpha.md")
	zetaIndex := strings.Index(plannerClient.prompts[0], "- docs/zeta.md")
	if alphaIndex == -1 || zetaIndex == -1 {
		t.Fatalf("expected prompt to include both resolved sources, got %q", plannerClient.prompts[0])
	}
	if alphaIndex > zetaIndex {
		t.Fatalf("expected prompt to render sources in sorted order, got %q", plannerClient.prompts[0])
	}
}

func TestNewBootstrapPlannerUsesSelectedClient(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Workspace: dir,
		Claude: config.ClaudeConfig{
			Command:         "claude",
			Transport:       config.ClaudeTransportPrint,
			StartupTimeout:  "30s",
			SessionStrategy: config.SessionStrategySessionID,
		},
		Codex: config.CodexConfig{
			Command:        "codex",
			Transport:      config.CodexTransportExec,
			StartupTimeout: "30s",
			Args:           []string{"exec"},
		},
	}

	planner, err := newBootstrapPlanner(cfg, config.AgentCodex, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("newBootstrapPlanner returned error: %v", err)
	}
	defer func() {
		_ = planner.Close()
	}()

	if _, ok := planner.(*codexBootstrapPlanner); !ok {
		t.Fatalf("expected codex bootstrap planner, got %T", planner)
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
