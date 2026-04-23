package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ghost_claude/internal/config"
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
	got, err := resolveConfigPath("ghost-claude.yaml", "")
	if err != nil {
		t.Fatalf("resolveConfigPath returned error: %v", err)
	}

	want, err := filepath.Abs("ghost-claude.yaml")
	if err != nil {
		t.Fatalf("filepath.Abs returned error: %v", err)
	}

	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestResolveConfigPathWithWorkspace(t *testing.T) {
	dir := t.TempDir()

	got, err := resolveConfigPath("ghost-claude.yaml", dir)
	if err != nil {
		t.Fatalf("resolveConfigPath returned error: %v", err)
	}

	want := filepath.Join(dir, "ghost-claude.yaml")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestResolveConfigPathKeepsAbsoluteConfigPath(t *testing.T) {
	absConfig := filepath.Join(t.TempDir(), "ghost-claude.yaml")

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

func TestResolveInitPlannerUsesClaudeDefault(t *testing.T) {
	got, err := resolveInitPlanner("")
	if err != nil {
		t.Fatalf("resolveInitPlanner returned error: %v", err)
	}
	if got != config.AgentClaude {
		t.Fatalf("expected default planner %q, got %q", config.AgentClaude, got)
	}
}

func TestResolveInitPlannerNormalizesCodex(t *testing.T) {
	got, err := resolveInitPlanner(" CoDeX ")
	if err != nil {
		t.Fatalf("resolveInitPlanner returned error: %v", err)
	}
	if got != config.AgentCodex {
		t.Fatalf("expected planner %q, got %q", config.AgentCodex, got)
	}
}

func TestResolveInitPlannerRejectsInvalidValue(t *testing.T) {
	_, err := resolveInitPlanner("cursor")
	if err == nil {
		t.Fatal("expected resolveInitPlanner to reject an unsupported planner")
	}
	if !strings.Contains(err.Error(), `planner "cursor" is not supported; expected claude or codex`) {
		t.Fatalf("unexpected error: %v", err)
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
