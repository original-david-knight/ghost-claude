package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"vibedrive/internal/config"
	createpkg "vibedrive/internal/create"
)

func TestResolveInitSourceArgsFromFlags(t *testing.T) {
	got, err := resolveInitSourceArgs([]string{"DESIGN.md", "docs"}, nil)
	if err != nil {
		t.Fatalf("resolveInitSourceArgs returned error: %v", err)
	}
	want := []string{"DESIGN.md", "docs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestResolveInitSourceArgsFromPositionalArg(t *testing.T) {
	got, err := resolveInitSourceArgs(nil, []string{"docs"})
	if err != nil {
		t.Fatalf("resolveInitSourceArgs returned error: %v", err)
	}
	want := []string{"docs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestResolveInitSourceArgsIncludesPositionalAlias(t *testing.T) {
	got, err := resolveInitSourceArgs([]string{"DESIGN.md"}, []string{"docs"})
	if err != nil {
		t.Fatalf("resolveInitSourceArgs returned error: %v", err)
	}
	want := []string{"DESIGN.md", "docs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestResolveInitSourceArgsRejectsMultiplePositionals(t *testing.T) {
	if _, err := resolveInitSourceArgs(nil, []string{"one", "two"}); err == nil {
		t.Fatal("expected resolveInitSourceArgs to reject multiple positional sources")
	}
}

func TestResolveInitSourceArgsRejectsEmptyFlag(t *testing.T) {
	if _, err := resolveInitSourceArgs([]string{"  "}, nil); err == nil {
		t.Fatal("expected resolveInitSourceArgs to reject an empty source flag")
	}
}

func TestResolveConfigPathWithoutWorkspace(t *testing.T) {
	got, err := resolveConfigPath("vibedrive.yaml", "")
	if err != nil {
		t.Fatalf("resolveConfigPath returned error: %v", err)
	}

	want, err := filepath.Abs("vibedrive.yaml")
	if err != nil {
		t.Fatalf("filepath.Abs returned error: %v", err)
	}

	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestResolveConfigPathWithWorkspace(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveConfigPath("vibedrive.yaml", dir)
	if err != nil {
		t.Fatalf("resolveConfigPath returned error: %v", err)
	}

	want := filepath.Join(dir, "vibedrive.yaml")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestResolveConfigPathKeepsAbsoluteConfigPath(t *testing.T) {
	absConfig := filepath.Join(t.TempDir(), "vibedrive.yaml")

	got, err := resolveConfigPath(absConfig, t.TempDir())
	if err != nil {
		t.Fatalf("resolveConfigPath returned error: %v", err)
	}

	if got != absConfig {
		t.Fatalf("expected %q, got %q", absConfig, got)
	}
}

func TestApplyRuntimeAgentRolesUsesDefaults(t *testing.T) {
	cfg := newRuntimeRoleConfig()

	if err := applyRuntimeAgentRoles(cfg, "", ""); err != nil {
		t.Fatalf("applyRuntimeAgentRoles returned error: %v", err)
	}

	if cfg.CoderAgent() != config.AgentCodex {
		t.Fatalf("expected default coder %q, got %q", config.AgentCodex, cfg.CoderAgent())
	}
	if cfg.ReviewerAgent() != config.AgentClaude {
		t.Fatalf("expected default reviewer %q, got %q", config.AgentClaude, cfg.ReviewerAgent())
	}
}

func TestApplyRuntimeAgentRolesOverridesDefaults(t *testing.T) {
	cfg := newRuntimeRoleConfig()

	if err := applyRuntimeAgentRoles(cfg, config.AgentClaude, config.AgentCodex); err != nil {
		t.Fatalf("applyRuntimeAgentRoles returned error: %v", err)
	}

	if cfg.CoderAgent() != config.AgentClaude {
		t.Fatalf("expected coder %q, got %q", config.AgentClaude, cfg.CoderAgent())
	}
	if cfg.ReviewerAgent() != config.AgentCodex {
		t.Fatalf("expected reviewer %q, got %q", config.AgentCodex, cfg.ReviewerAgent())
	}
}

func TestResolveInitAuthorUsesCodexDefault(t *testing.T) {
	got, err := resolveInitAuthor("")
	if err != nil {
		t.Fatalf("resolveInitAuthor returned error: %v", err)
	}
	if got != config.AgentCodex {
		t.Fatalf("expected default author %q, got %q", config.AgentCodex, got)
	}
}

func TestResolveInitAuthorNormalizesClaude(t *testing.T) {
	got, err := resolveInitAuthor(" ClAuDe ")
	if err != nil {
		t.Fatalf("resolveInitAuthor returned error: %v", err)
	}
	if got != config.AgentClaude {
		t.Fatalf("expected author %q, got %q", config.AgentClaude, got)
	}
}

func TestResolveInitAuthorRejectsInvalidValue(t *testing.T) {
	_, err := resolveInitAuthor("cursor")
	if err == nil {
		t.Fatal("expected resolveInitAuthor to reject an unsupported author")
	}
	if !strings.Contains(err.Error(), `author "cursor" is not supported; expected claude or codex`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveInitCriticUsesClaudeDefault(t *testing.T) {
	got, err := resolveInitCritic("")
	if err != nil {
		t.Fatalf("resolveInitCritic returned error: %v", err)
	}
	if got != config.AgentClaude {
		t.Fatalf("expected default critic %q, got %q", config.AgentClaude, got)
	}
}

func TestResolveInitCriticNormalizesCodex(t *testing.T) {
	got, err := resolveInitCritic(" CoDeX ")
	if err != nil {
		t.Fatalf("resolveInitCritic returned error: %v", err)
	}
	if got != config.AgentCodex {
		t.Fatalf("expected critic %q, got %q", config.AgentCodex, got)
	}
}

func TestResolveInitCriticRejectsInvalidValue(t *testing.T) {
	_, err := resolveInitCritic("cursor")
	if err == nil {
		t.Fatal("expected resolveInitCritic to reject an unsupported critic")
	}
	if !strings.Contains(err.Error(), `critic "cursor" is not supported; expected claude or codex`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitCommandAcceptsAuthorAndCriticFlagsWithPrintSources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DESIGN.md"), []byte("design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	for _, author := range []string{config.AgentClaude, config.AgentCodex} {
		for _, critic := range []string{config.AgentClaude, config.AgentCodex} {
			err := initCommand(context.Background(), []string{
				"--workspace", dir,
				"--source", "DESIGN.md",
				"--author", author,
				"--critic", critic,
				"--print-sources",
			})
			if err != nil {
				t.Fatalf("initCommand rejected --author %s --critic %s: %v", author, critic, err)
			}
		}
	}
}

func TestInitCommandRejectsInvalidCriticFlag(t *testing.T) {
	err := initCommand(context.Background(), []string{"--critic", "cursor", "--print-sources"})
	if err == nil {
		t.Fatal("expected initCommand to reject invalid --critic")
	}
	if !strings.Contains(err.Error(), `critic "cursor" is not supported; expected claude or codex`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitCommandRejectsPlannerFlag(t *testing.T) {
	err := initCommand(context.Background(), []string{"--planner", config.AgentCodex, "--print-sources"})
	if err == nil {
		t.Fatal("expected initCommand to reject --planner")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateCommandRunsSelectedStagesAndReturnsToMenu(t *testing.T) {
	dir := t.TempDir()
	menu := &fakeCreateMenu{
		choices: []string{"Product Definition", "UX Review", "Technical Review", "Stop"},
	}
	stage := &fakeCreateStageRunner{}

	err := runCreateCommand(context.Background(), []string{"--workspace", dir}, testCreateDeps(menu, stage, nil))
	if err != nil {
		t.Fatalf("runCreateCommand returned error: %v", err)
	}

	wantStages := []createpkg.Stage{
		createpkg.StageProductDefinition,
		createpkg.StageUXReview,
		createpkg.StageTechnicalReview,
	}
	if len(stage.calls) != len(wantStages) {
		t.Fatalf("expected %d stage calls, got %d: %#v", len(wantStages), len(stage.calls), stage.calls)
	}
	for idx, wantStage := range wantStages {
		call := stage.calls[idx]
		if call.Stage != wantStage {
			t.Fatalf("call %d: expected stage %q, got %q", idx, wantStage, call.Stage)
		}
		if call.Author != config.AgentCodex || call.Critic != config.AgentClaude {
			t.Fatalf("call %d: expected default author/critic codex/claude, got %s/%s", idx, call.Author, call.Critic)
		}
	}
	if len(menu.seenEntries) != 4 {
		t.Fatalf("expected menu after each successful stage plus stop, got %d", len(menu.seenEntries))
	}
}

func TestCreateCommandPlanningHiddenWithoutDesign(t *testing.T) {
	dir := t.TempDir()
	menu := &fakeCreateMenu{choices: []string{"Stop"}}
	stateSeen := false
	menu.onChoose = func(state createpkg.State, entries []createMenuEntry) {
		stateSeen = true
		if state != (createpkg.State{}) {
			t.Fatalf("expected missing state to be treated as empty, got %#v", state)
		}
		if entryLabels(entries).Contains("Planning") {
			t.Fatalf("expected Planning to be hidden without DESIGN.md, got %v", entryLabels(entries))
		}
	}

	err := runCreateCommand(context.Background(), []string{"--workspace", dir}, testCreateDeps(menu, nil, nil))
	if err != nil {
		t.Fatalf("runCreateCommand returned error: %v", err)
	}
	if !stateSeen {
		t.Fatal("expected menu to observe startup state")
	}
	if _, err := os.Stat(createpkg.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("expected Stop not to create state, stat err=%v", err)
	}
}

func TestCreateCommandPlanningRoutesToInitWithDesignAndRoles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, createpkg.DesignFileName()), []byte("design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := createpkg.Write(dir, createpkg.State{LastStage: createpkg.StageProductDefinition}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	menu := &fakeCreateMenu{choices: []string{"Planning"}}
	menu.onChoose = func(_ createpkg.State, entries []createMenuEntry) {
		if !entryLabels(entries).Contains("Planning") {
			t.Fatalf("expected Planning to be visible when DESIGN.md exists, got %v", entryLabels(entries))
		}
	}
	planning := &fakeCreatePlanningRunner{}

	err := runCreateCommand(context.Background(), []string{
		"--workspace", dir,
		"--author", config.AgentClaude,
		"--critic", config.AgentCodex,
	}, testCreateDeps(menu, nil, planning))
	if err != nil {
		t.Fatalf("runCreateCommand returned error: %v", err)
	}

	if len(planning.calls) != 1 {
		t.Fatalf("expected one planning call, got %d", len(planning.calls))
	}
	call := planning.calls[0]
	if call.ConfigPath != filepath.Join(dir, "vibedrive.yaml") {
		t.Fatalf("expected config path under workspace, got %q", call.ConfigPath)
	}
	if !slices.Equal(call.SourceArgs, []string{createpkg.DesignFileName()}) {
		t.Fatalf("expected DESIGN.md as only source, got %v", call.SourceArgs)
	}
	if call.Force {
		t.Fatal("expected planning handoff not to force init")
	}
	if call.Author != config.AgentClaude || call.Critic != config.AgentCodex {
		t.Fatalf("expected author/critic claude/codex, got %s/%s", call.Author, call.Critic)
	}
}

func TestCreateCommandDefaultsAuthorCodexAndCriticClaude(t *testing.T) {
	dir := t.TempDir()
	menu := &fakeCreateMenu{choices: []string{"Product Definition", "Stop"}}
	stage := &fakeCreateStageRunner{}

	err := runCreateCommand(context.Background(), []string{"--workspace", dir}, testCreateDeps(menu, stage, nil))
	if err != nil {
		t.Fatalf("runCreateCommand returned error: %v", err)
	}
	if len(stage.calls) != 1 {
		t.Fatalf("expected one stage call, got %d", len(stage.calls))
	}
	if stage.calls[0].Author != config.AgentCodex || stage.calls[0].Critic != config.AgentClaude {
		t.Fatalf("expected default author/critic codex/claude, got %s/%s", stage.calls[0].Author, stage.calls[0].Critic)
	}
}

func TestCreateCommandRejectsUnsupportedFlagsAndPositionalIdea(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "dry run", args: []string{"--dry-run"}, want: "flag provided but not defined"},
		{name: "resume", args: []string{"--resume"}, want: "flag provided but not defined"},
		{name: "positional idea", args: []string{"build a thing"}, want: "create does not accept positional arguments"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runCreateCommand(context.Background(), tt.args, testCreateDeps(&fakeCreateMenu{}, nil, nil))
			if err == nil {
				t.Fatal("expected runCreateCommand to reject args")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestCreateCommandCancellationDuringMenuPreservesDesignAndState(t *testing.T) {
	dir := t.TempDir()
	designPath := filepath.Join(dir, createpkg.DesignFileName())
	if err := os.WriteFile(designPath, []byte("existing design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := createpkg.Write(dir, createpkg.State{LastStage: createpkg.StageUXReview}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	beforeDesign := mustReadFile(t, designPath)
	beforeState := mustReadFile(t, createpkg.Path(dir))

	menu := &fakeCreateMenu{err: context.Canceled}
	err := runCreateCommand(context.Background(), []string{"--workspace", dir}, testCreateDeps(menu, nil, nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	assertFileBytes(t, designPath, beforeDesign)
	assertFileBytes(t, createpkg.Path(dir), beforeState)
}

func TestCreateCommandCancellationDuringStagePreservesExistingDesignAndState(t *testing.T) {
	dir := t.TempDir()
	designPath := filepath.Join(dir, createpkg.DesignFileName())
	if err := os.WriteFile(designPath, []byte("existing design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := createpkg.Write(dir, createpkg.State{LastStage: createpkg.StageProductDefinition}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	beforeDesign := mustReadFile(t, designPath)
	beforeState := mustReadFile(t, createpkg.Path(dir))

	menu := &fakeCreateMenu{choices: []string{"Technical Review"}}
	stage := &fakeCreateStageRunner{err: context.Canceled}

	err := runCreateCommand(context.Background(), []string{"--workspace", dir}, testCreateDeps(menu, stage, nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	assertFileBytes(t, designPath, beforeDesign)
	assertFileBytes(t, createpkg.Path(dir), beforeState)
}

func TestCreateCommandStopExitsWithoutModifyingDesignOrState(t *testing.T) {
	dir := t.TempDir()
	designPath := filepath.Join(dir, createpkg.DesignFileName())
	if err := os.WriteFile(designPath, []byte("existing design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := createpkg.Write(dir, createpkg.State{LastStage: createpkg.StageTechnicalReview}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	beforeDesign := mustReadFile(t, designPath)
	beforeState := mustReadFile(t, createpkg.Path(dir))

	menu := &fakeCreateMenu{choices: []string{"Stop"}}
	stage := &fakeCreateStageRunner{}
	planning := &fakeCreatePlanningRunner{}

	err := runCreateCommand(context.Background(), []string{"--workspace", dir}, testCreateDeps(menu, stage, planning))
	if err != nil {
		t.Fatalf("runCreateCommand returned error: %v", err)
	}
	if len(stage.calls) != 0 {
		t.Fatalf("expected no stage calls, got %#v", stage.calls)
	}
	if len(planning.calls) != 0 {
		t.Fatalf("expected no planning calls, got %#v", planning.calls)
	}
	assertFileBytes(t, designPath, beforeDesign)
	assertFileBytes(t, createpkg.Path(dir), beforeState)
}

type fakeCreateMenu struct {
	choices     []string
	err         error
	onChoose    func(state createpkg.State, entries []createMenuEntry)
	seenEntries [][]createMenuEntry
	seenStates  []createpkg.State
}

func (f *fakeCreateMenu) Choose(ctx context.Context, state createpkg.State, entries []createMenuEntry) (createMenuEntry, error) {
	f.seenStates = append(f.seenStates, state)
	f.seenEntries = append(f.seenEntries, append([]createMenuEntry{}, entries...))
	if f.onChoose != nil {
		f.onChoose(state, entries)
	}
	if f.err != nil {
		return createMenuEntry{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return createMenuEntry{}, err
	}
	if len(f.choices) == 0 {
		return createMenuEntry{}, io.EOF
	}

	label := f.choices[0]
	f.choices = f.choices[1:]
	for _, entry := range entries {
		if entry.Label == label {
			return entry, nil
		}
	}
	return createMenuEntry{}, fmt.Errorf("menu entry %q not visible; visible entries: %v", label, entryLabels(entries))
}

func (f *fakeCreateMenu) Confirm(ctx context.Context, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
}

type createStageCall struct {
	Stage  createpkg.Stage
	Author string
	Critic string
}

type fakeCreateStageRunner struct {
	calls []createStageCall
	err   error
}

func (f *fakeCreateStageRunner) Run(ctx context.Context, stage createpkg.Stage, authorAgent, criticAgent string) error {
	f.calls = append(f.calls, createStageCall{Stage: stage, Author: authorAgent, Critic: criticAgent})
	if f.err != nil {
		return f.err
	}
	return ctx.Err()
}

type fakeCreatePlanningCall struct {
	ConfigPath string
	SourceArgs []string
	Force      bool
	Author     string
	Critic     string
}

type fakeCreatePlanningRunner struct {
	calls []fakeCreatePlanningCall
	err   error
}

func (f *fakeCreatePlanningRunner) run(ctx context.Context, configPath string, sourceArgs []string, force bool, author, critic string) error {
	f.calls = append(f.calls, fakeCreatePlanningCall{
		ConfigPath: configPath,
		SourceArgs: append([]string{}, sourceArgs...),
		Force:      force,
		Author:     author,
		Critic:     critic,
	})
	if f.err != nil {
		return f.err
	}
	return ctx.Err()
}

func testCreateDeps(menu *fakeCreateMenu, stage *fakeCreateStageRunner, planning *fakeCreatePlanningRunner) createCommandDeps {
	if menu == nil {
		menu = &fakeCreateMenu{choices: []string{"Stop"}}
	}
	if stage == nil {
		stage = &fakeCreateStageRunner{}
	}
	deps := createCommandDeps{
		input: menu,
		stageRunnerFactory: func(_ string, _ string, _ io.Writer, _ io.Writer, _ createpkg.ConfirmFunc) (createStageRunner, error) {
			return stage, nil
		},
		stdout: io.Discard,
		stderr: io.Discard,
	}
	if planning != nil {
		deps.planningRunner = planning.run
	}
	return deps
}

type labelList []string

func (l labelList) Contains(label string) bool {
	return slices.Contains(l, label)
}

func entryLabels(entries []createMenuEntry) labelList {
	labels := make(labelList, 0, len(entries))
	for _, entry := range entries {
		labels = append(labels, entry.Label)
	}
	return labels
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned error: %v", path, err)
	}
	return data
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got := mustReadFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("expected %s to remain %q, got %q", path, string(want), string(got))
	}
}

func newRuntimeRoleConfig() *config.Config {
	return &config.Config{
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			Command:         "claude",
			Transport:       config.ClaudeTransportTUI,
			StartupTimeout:  "30s",
			SessionStrategy: config.SessionStrategySessionID,
		},
		Steps: []config.Step{
			{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "execute"},
			{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review"},
		},
	}
}
