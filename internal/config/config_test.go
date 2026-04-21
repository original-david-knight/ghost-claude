package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadAddsMaxEffortWhenClaudeArgsOmitted(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `steps:
  - name: inspect
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{"--effort", "max"}
	if !slices.Equal(cfg.Claude.Args, want) {
		t.Fatalf("expected default claude args %v, got %v", want, cfg.Claude.Args)
	}
}

func TestLoadAppendsMaxEffortWhenClaudeArgsDoNotSetIt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `claude:
  args:
    - --permission-mode
    - bypassPermissions
steps:
  - name: inspect
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{"--permission-mode", "bypassPermissions", "--effort", "max"}
	if !slices.Equal(cfg.Claude.Args, want) {
		t.Fatalf("expected claude args %v, got %v", want, cfg.Claude.Args)
	}
}

func TestLoadPreservesExplicitClaudeEffort(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `claude:
  args:
    - --effort
    - high
steps:
  - name: inspect
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{"--effort", "high"}
	if !slices.Equal(cfg.Claude.Args, want) {
		t.Fatalf("expected claude args %v, got %v", want, cfg.Claude.Args)
	}
}

func TestLoadSetsDefaultCodexExecArgs(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `steps:
  - name: inspect
    type: codex
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "exec", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadAppendsDefaultCodexReasoningWhenMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `codex:
  args:
    - review
    - --uncommitted
steps:
  - name: inspect
    type: codex
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "review", "--uncommitted", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadPreservesExplicitCodexReasoning(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `codex:
  args:
    - exec
    - -c
    - model_reasoning_effort="high"
steps:
  - name: inspect
    type: codex
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "exec", "-c", `model_reasoning_effort="high"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadStripsConflictingCodexPermissionFlags(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `codex:
  args:
    - exec
    - --sandbox
    - read-only
    - --ask-for-approval
    - on-request
    - --full-auto
steps:
  - name: inspect
    type: codex
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "exec", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadDefaultsRolesForAgentSteps(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `steps:
  - name: inspect
    type: agent
    actor: reviewer
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.CoderAgent() != AgentCodex {
		t.Fatalf("expected default coder %q, got %q", AgentCodex, cfg.CoderAgent())
	}
	if cfg.ReviewerAgent() != AgentClaude {
		t.Fatalf("expected default reviewer %q, got %q", AgentClaude, cfg.ReviewerAgent())
	}
}

func TestLoadIgnoresConfiguredRuntimeRoles(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `coder: invalid-coder
reviewer: invalid-reviewer
steps:
  - name: execute
    type: agent
    actor: coder
    prompt: execute
  - name: review
    type: agent
    actor: reviewer
    prompt: review
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.CoderAgent() != AgentCodex {
		t.Fatalf("expected default coder %q, got %q", AgentCodex, cfg.CoderAgent())
	}
	if cfg.ReviewerAgent() != AgentClaude {
		t.Fatalf("expected default reviewer %q, got %q", AgentClaude, cfg.ReviewerAgent())
	}
}

func TestValidateAllowsSameAgentForCoderAndReviewer(t *testing.T) {
	cfg := &Config{
		MaxStalledIterations: 1,
		Coder:                AgentCodex,
		Reviewer:             AgentCodex,
		Claude: ClaudeConfig{
			Command:         "claude",
			Transport:       ClaudeTransportTUI,
			StartupTimeout:  "30s",
			SessionStrategy: SessionStrategySessionID,
		},
		Steps: []Step{
			{Name: "execute", Type: StepTypeAgent, Actor: StepActorCoder, Prompt: "execute"},
			{Name: "review", Type: StepTypeAgent, Actor: StepActorReviewer, Prompt: "review"},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestLoadRejectsPrimaryActorAlias(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	content := `steps:
  - name: execute
    type: agent
    actor: primary
    prompt: execute
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("expected Load to reject the primary actor alias")
	}
}
