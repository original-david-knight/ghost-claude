package main

import (
	"path/filepath"
	"testing"

	"ghost_claude/internal/config"
)

func TestResolveInitSourceArgFromFlag(t *testing.T) {
	got, err := resolveInitSourceArg("DESIGN.md", nil)
	if err != nil {
		t.Fatalf("resolveInitSourceArg returned error: %v", err)
	}
	if got != "DESIGN.md" {
		t.Fatalf("expected DESIGN.md, got %q", got)
	}
}

func TestResolveInitSourceArgFromPositionalArg(t *testing.T) {
	got, err := resolveInitSourceArg("", []string{"docs"})
	if err != nil {
		t.Fatalf("resolveInitSourceArg returned error: %v", err)
	}
	if got != "docs" {
		t.Fatalf("expected docs, got %q", got)
	}
}

func TestResolveInitSourceArgRejectsMultipleSources(t *testing.T) {
	if _, err := resolveInitSourceArg("", []string{"one", "two"}); err == nil {
		t.Fatal("expected resolveInitSourceArg to reject multiple sources")
	}
}

func TestResolveInitSourceArgRejectsMixedFlagAndPositional(t *testing.T) {
	if _, err := resolveInitSourceArg("DESIGN.md", []string{"docs"}); err == nil {
		t.Fatal("expected resolveInitSourceArg to reject mixed flag and positional sources")
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
