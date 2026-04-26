package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"vibedrive/internal/config"
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
