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

func TestLoadAcceptsConfiguredAgentTransports(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `claude:
  transport: print
codex:
  transport: exec
steps:
  - name: implement
    type: codex
    prompt: implement
  - name: review
    type: claude
    prompt: review
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Claude.Transport != ClaudeTransportPrint {
		t.Fatalf("expected claude transport %q, got %q", ClaudeTransportPrint, cfg.Claude.Transport)
	}
	if cfg.Codex.Transport != CodexTransportExec {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportExec, cfg.Codex.Transport)
	}
}

func TestLoadAcceptsPinnedAgentVersions(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	claudeCommand := writeVersionCommand(t, dir, "fake-claude", "claude 1.2.3")
	codexCommand := writeVersionCommand(t, dir, "fake-codex", "codex-cli 4.5.6")

	content := fmt.Sprintf(`claude:
  command: %q
  version: "claude 1.2.3"
codex:
  command: %q
  version: "codex-cli 4.5.6"
steps:
  - name: inspect
    prompt: inspect
`, claudeCommand, codexCommand)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Claude.Version != "claude 1.2.3" {
		t.Fatalf("expected claude version pin, got %q", cfg.Claude.Version)
	}
	if cfg.Codex.Version != "codex-cli 4.5.6" {
		t.Fatalf("expected codex version pin, got %q", cfg.Codex.Version)
	}
}

func TestLoadDoesNotResolvePinnedAgentVersions(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := fmt.Sprintf(`codex:
  command: %q
  version: "codex-cli 4.5.6"
steps:
  - name: inspect
    type: codex
    prompt: inspect
`, filepath.Join(dir, "missing-codex"))
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Codex.Version != "codex-cli 4.5.6" {
		t.Fatalf("expected codex version pin to load, got %q", cfg.Codex.Version)
	}
}

func TestCheckPinnedAgentVersionsRejectsMismatchedAgentVersions(t *testing.T) {
	tests := []struct {
		name        string
		cfg         func(string) *Config
		wantMessage string
	}{
		{
			name: "claude",
			cfg: func(dir string) *Config {
				return &Config{
					Workspace: dir,
					Claude: ClaudeConfig{
						Command: writeVersionCommand(t, dir, "fake-claude", "claude 1.2.4"),
						Version: "claude 1.2.3",
					},
				}
			},
			wantMessage: `claude.version "claude 1.2.3" does not match live claude CLI version "claude 1.2.4"`,
		},
		{
			name: "codex",
			cfg: func(dir string) *Config {
				return &Config{
					Workspace: dir,
					Codex: CodexConfig{
						Command: writeVersionCommand(t, dir, "fake-codex", "codex-cli 4.5.7"),
						Version: "codex-cli 4.5.6",
					},
				}
			},
			wantMessage: `codex.version "codex-cli 4.5.6" does not match live codex CLI version "codex-cli 4.5.7"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			err := tt.cfg(dir).CheckPinnedAgentVersions()
			if err == nil {
				t.Fatalf("expected CheckPinnedAgentVersions to reject mismatched %s.version", tt.name)
			}
			for _, want := range []string{
				tt.wantMessage,
				"update " + tt.name + ".version only after intentionally upgrading the CLI",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("expected error to contain %q, got %v", want, err)
				}
			}
		})
	}
}

func TestCheckPinnedAgentVersionsReportsVersionReadFailures(t *testing.T) {
	dir := t.TempDir()
	missingCommand := filepath.Join(dir, "missing-codex")

	cfg := &Config{
		Workspace: dir,
		Codex: CodexConfig{
			Command: missingCommand,
			Version: "codex-cli 4.5.6",
		},
	}

	err := cfg.CheckPinnedAgentVersions()
	if err == nil {
		t.Fatal("expected CheckPinnedAgentVersions to reject unreadable codex command")
	}
	for _, want := range []string{
		`codex.version is pinned to "codex-cli 4.5.6" but failed to read the live CLI version`,
		missingCommand,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %v", want, err)
		}
	}
}

func TestCheckPinnedAgentVersionsUsesWorkspaceForRelativeCommands(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	writeVersionCommand(t, dir, filepath.Join("bin", "fake-codex"), "codex-cli 4.5.6")

	content := `codex:
  command: ./bin/fake-codex
  version: "codex-cli 4.5.6"
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
	if err := cfg.CheckPinnedAgentVersions(); err != nil {
		t.Fatalf("CheckPinnedAgentVersions returned error: %v", err)
	}
}

func TestLoadNormalizesConfiguredAgentTransports(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `claude:
  transport: " PRINT "
codex:
  transport: " EXEC "
steps:
  - name: implement
    type: codex
    prompt: implement
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Claude.Transport != ClaudeTransportPrint {
		t.Fatalf("expected claude transport %q, got %q", ClaudeTransportPrint, cfg.Claude.Transport)
	}
	if cfg.Codex.Transport != CodexTransportExec {
		t.Fatalf("expected codex transport %q, got %q", CodexTransportExec, cfg.Codex.Transport)
	}
}

func TestLoadRejectsUnknownTransportSettings(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "claude stream",
			content: `claude:
  transport: stream
steps:
  - name: inspect
    prompt: inspect
`,
			want: `unsupported claude.transport "stream"`,
		},
		{
			name: "codex pty",
			content: `codex:
  transport: pty
steps:
  - name: inspect
    type: codex
    prompt: inspect
`,
			want: `unsupported codex.transport "pty"`,
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
				t.Fatal("expected Load to reject unknown transport")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadRejectsCodexSubcommandWithExecTransport(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	content := `codex:
  transport: exec
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
		t.Fatal("expected Load to reject redundant exec subcommand")
	}
	if !strings.Contains(err.Error(), `codex.transport "exec" selects non-interactive exec mode; remove codex.args subcommand "exec"`) {
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

	if cfg.CoderAgent() != AgentClaude {
		t.Fatalf("expected default coder %q, got %q", AgentClaude, cfg.CoderAgent())
	}
	if cfg.ReviewerAgent() != AgentCodex {
		t.Fatalf("expected default reviewer %q, got %q", AgentCodex, cfg.ReviewerAgent())
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

	if cfg.CoderAgent() != AgentClaude {
		t.Fatalf("expected default coder %q, got %q", AgentClaude, cfg.CoderAgent())
	}
	if cfg.ReviewerAgent() != AgentCodex {
		t.Fatalf("expected default reviewer %q, got %q", AgentCodex, cfg.ReviewerAgent())
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

func writeVersionCommand(t *testing.T, dir, name, version string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat <<'VIBEDRIVE_VERSION'\n%s\nVIBEDRIVE_VERSION\n", version)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	return path
}
