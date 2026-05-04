package config

import (
	"fmt"
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

func TestLoadDefaultsParallelExecutionToSerial(t *testing.T) {
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

	if cfg.Parallel.Enabled {
		t.Fatal("expected parallel execution to be disabled by default")
	}
	if cfg.Parallel.MaxParallelism != DefaultParallelMaxParallelism {
		t.Fatalf("expected default max parallelism %d, got %d", DefaultParallelMaxParallelism, cfg.Parallel.MaxParallelism)
	}
	if cfg.EffectiveParallelism() != 1 {
		t.Fatalf("expected effective parallelism 1, got %d", cfg.EffectiveParallelism())
	}

	wantWorktreeRoot := filepath.Join(dir, DefaultParallelWorktreeRoot)
	if cfg.Parallel.WorktreeRoot != wantWorktreeRoot {
		t.Fatalf("expected worktree root %q, got %q", wantWorktreeRoot, cfg.Parallel.WorktreeRoot)
	}
	wantArtifactRoot := filepath.Join(dir, DefaultParallelArtifactRoot)
	if cfg.Parallel.ArtifactRoot != wantArtifactRoot {
		t.Fatalf("expected artifact root %q, got %q", wantArtifactRoot, cfg.Parallel.ArtifactRoot)
	}
}

func TestLoadRequiresParallelismOptIn(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `parallel:
  max_parallelism: 4
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

	if cfg.Parallel.MaxParallelism != 4 {
		t.Fatalf("expected configured max parallelism 4, got %d", cfg.Parallel.MaxParallelism)
	}
	if cfg.EffectiveParallelism() != 1 {
		t.Fatalf("expected effective parallelism to stay serial until enabled, got %d", cfg.EffectiveParallelism())
	}
}

func TestLoadAcceptsOptInParallelExecution(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `parallel:
  enabled: true
  max_parallelism: 3
  worktree_root: .vibedrive/custom-worktrees
  artifact_root: .vibedrive/custom-task-runs
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

	if cfg.EffectiveParallelism() != 3 {
		t.Fatalf("expected effective parallelism 3, got %d", cfg.EffectiveParallelism())
	}
	wantWorktreeRoot := filepath.Join(dir, ".vibedrive/custom-worktrees")
	if cfg.Parallel.WorktreeRoot != wantWorktreeRoot {
		t.Fatalf("expected worktree root %q, got %q", wantWorktreeRoot, cfg.Parallel.WorktreeRoot)
	}
	wantArtifactRoot := filepath.Join(dir, ".vibedrive/custom-task-runs")
	if cfg.Parallel.ArtifactRoot != wantArtifactRoot {
		t.Fatalf("expected artifact root %q, got %q", wantArtifactRoot, cfg.Parallel.ArtifactRoot)
	}
}

func TestLoadRejectsInvalidParallelismValues(t *testing.T) {
	for _, value := range []int{0, -1} {
		t.Run(fmt.Sprintf("max_parallelism=%d", value), func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "vibedrive.yaml")

			content := fmt.Sprintf(`parallel:
  enabled: true
  max_parallelism: %d
steps:
  - name: inspect
    prompt: inspect
`, value)
			if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}

			_, err := Load(configPath)
			if err == nil {
				t.Fatal("expected Load to reject invalid parallelism")
			}
			if !strings.Contains(err.Error(), "parallel.max_parallelism must be >= 1") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
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
    - --profile
    - vibedrive
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

	if cfg.Codex.Transport != CodexTransportTUI {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportTUI, cfg.Codex.Transport)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "--profile", "vibedrive", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadPreservesExplicitCodexReasoning(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `codex:
  args:
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

	if cfg.Codex.Transport != CodexTransportTUI {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportTUI, cfg.Codex.Transport)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="high"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadStripsConflictingCodexPermissionFlags(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `codex:
  args:
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

	if cfg.Codex.Transport != CodexTransportTUI {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportTUI, cfg.Codex.Transport)
	}

	want := []string{"--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="xhigh"`}
	if !slices.Equal(cfg.Codex.Args, want) {
		t.Fatalf("expected codex args %v, got %v", want, cfg.Codex.Args)
	}
}

func TestLoadRejectsNonInteractiveCodexSubcommands(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `codex:
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
		t.Fatal("expected Load to reject exec subcommand")
	}
	if !strings.Contains(err.Error(), `codex.args subcommand "exec" is non-interactive`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsNonTUITransportSettings(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "claude print",
			content: `claude:
  transport: print
steps:
  - name: inspect
    prompt: inspect
`,
			want: `claude.transport "print" is no longer supported`,
		},
		{
			name: "codex exec",
			content: `codex:
  transport: exec
steps:
  - name: inspect
    type: codex
    prompt: inspect
`,
			want: `codex.transport "exec" is no longer supported`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "vibedrive.yaml")
			if err := os.WriteFile(configPath, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}

			_, err := Load(configPath)
			if err == nil {
				t.Fatal("expected Load to reject non-TUI transport")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
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
		Parallel: ParallelConfig{
			MaxParallelism: DefaultParallelMaxParallelism,
			WorktreeRoot:   DefaultParallelWorktreeRoot,
			ArtifactRoot:   DefaultParallelArtifactRoot,
		},
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
