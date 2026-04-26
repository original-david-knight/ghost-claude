package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadAddsMaxEffortWhenClaudeArgsOmitted(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

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

	want := []string{"--effort", "max", "--permission-mode", "bypassPermissions"}
	if !slices.Equal(cfg.Claude.Args, want) {
		t.Fatalf("expected default claude args %v, got %v", want, cfg.Claude.Args)
	}
}

func TestLoadAppendsMaxEffortWhenClaudeArgsDoNotSetIt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

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
	configPath := filepath.Join(dir, "vibedrive.yaml")

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

	want := []string{"--effort", "high", "--permission-mode", "bypassPermissions"}
	if !slices.Equal(cfg.Claude.Args, want) {
		t.Fatalf("expected claude args %v, got %v", want, cfg.Claude.Args)
	}
}

func TestLoadPreservesExplicitClaudePermissionFlag(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `claude:
  args:
    - --dangerously-skip-permissions
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

	want := []string{"--dangerously-skip-permissions", "--effort", "max"}
	if !slices.Equal(cfg.Claude.Args, want) {
		t.Fatalf("expected claude args %v, got %v", want, cfg.Claude.Args)
	}
}

func TestLoadSetsDefaultCodexTUIArgs(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

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

	if cfg.Codex.Transport != CodexTransportTUI {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportTUI, cfg.Codex.Transport)
	}
	if cfg.Codex.StartupTimeout != defaultStartupTimeout {
		t.Fatalf("expected codex startup timeout %q, got %q", defaultStartupTimeout, cfg.Codex.StartupTimeout)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestDefaultAgentLaunchConfigDoesNotRequireConfigFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	cfg, err := DefaultAgentLaunchConfig(configPath)
	if err != nil {
		t.Fatalf("DefaultAgentLaunchConfig returned error: %v", err)
	}

	if cfg.Path != configPath {
		t.Fatalf("expected config path %q, got %q", configPath, cfg.Path)
	}
	if cfg.Workspace != dir {
		t.Fatalf("expected workspace %q, got %q", dir, cfg.Workspace)
	}
	if cfg.Claude.Command != "claude" || cfg.Codex.Command != "codex" {
		t.Fatalf("expected default agent commands, got claude=%q codex=%q", cfg.Claude.Command, cfg.Codex.Command)
	}
	if cfg.Codex.Transport != CodexTransportTUI {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportTUI, cfg.Codex.Transport)
	}
	if len(cfg.Steps) != 0 || len(cfg.Workflows) != 0 {
		t.Fatalf("expected launch-only config not to require workflow steps, got steps=%d workflows=%d", len(cfg.Steps), len(cfg.Workflows))
	}
}

func TestLoadAppendsDefaultCodexReasoningWhenMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

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

	if cfg.Codex.Transport != CodexTransportExec {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportExec, cfg.Codex.Transport)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "review", "--uncommitted", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadPreservesExplicitCodexReasoning(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

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

	if cfg.Codex.Transport != CodexTransportExec {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportExec, cfg.Codex.Transport)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "exec", "-c", `model_reasoning_effort="high"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadStripsConflictingCodexPermissionFlags(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

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

	if cfg.Codex.Transport != CodexTransportExec {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportExec, cfg.Codex.Transport)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "exec", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadRejectsExecSubcommandForCodexTUITransport(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `codex:
  transport: tui
  args:
    - exec
steps:
  - name: inspect
    type: codex
    prompt: inspect
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected Load to reject exec subcommand for codex tui transport")
	}
	if !strings.Contains(err.Error(), `codex.transport "tui" does not support subcommand "exec"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadDefaultsRolesForAgentSteps(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

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
	configPath := filepath.Join(dir, "vibedrive.yaml")

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
	configPath := filepath.Join(dir, "vibedrive.yaml")

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
