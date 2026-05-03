package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/config"
)

func TestWriteWritesSampleConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	if err := Write(configPath, false); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	configContent, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(configContent), "workspace: .") {
		t.Fatalf("expected sample config content, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "plan_file: vibedrive-plan.yaml") {
		t.Fatalf("expected plan mode sample config, got %q", string(configContent))
	}
	assertScaffoldedParallelDefaults(t, configPath, string(configContent))
	if !strings.Contains(string(configContent), "codex:") {
		t.Fatalf("expected scaffolded config to define codex, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "\ncoder:") || strings.Contains(string(configContent), "\nreviewer:") {
		t.Fatalf("expected scaffolded config to leave runtime role selection to CLI flags, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "type: agent") || !strings.Contains(string(configContent), "actor: coder") {
		t.Fatalf("expected scaffolded config to use runtime-resolved coder steps, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "peer-review") || !strings.Contains(string(configContent), "actor: reviewer") {
		t.Fatalf("expected scaffolded config to use runtime-resolved reviewer steps, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "address-peer-review") {
		t.Fatalf("expected scaffolded config to hand peer-review findings back to the coder, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "{{ .ReviewPath }}") {
		t.Fatalf("expected scaffolded config to use the review artifact path, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "required_outputs:") || !strings.Contains(string(configContent), "{{ .TaskResultPath }}") {
		t.Fatalf("expected scaffolded config to declare required outputs for task artifacts, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "what was learned in this phase") {
		t.Fatalf("expected scaffolded config to request phase-learnings notes, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "missing self-verification instrumentation") {
		t.Fatalf("expected scaffolded config to review self-verification instrumentation, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "scripted screenshots") {
		t.Fatalf("expected scaffolded config to mention screenshot-based verification artifacts, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "Boundary metadata:") {
		t.Fatalf("expected scaffolded config to render task boundary metadata when present, got %q", string(configContent))
	}
	for _, want := range []string{"owns_paths:", "reads_contracts:", "provides_contracts:", "conflicts_with:"} {
		if !strings.Contains(string(configContent), want) {
			t.Fatalf("expected scaffolded config to include boundary field %q, got %q", want, string(configContent))
		}
	}
	if !strings.Contains(string(configContent), "Treat owns_paths as explicit edit authority") {
		t.Fatalf("expected scaffolded config to enforce owns_paths edit authority, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "contract files according to reads_contracts and provides_contracts") {
		t.Fatalf("expected scaffolded config to enforce contract metadata, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "violations of declared owns_paths or contract metadata") {
		t.Fatalf("expected scaffolded review to check ownership and contract violations, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "{{ .TaskNotesPath }}") {
		t.Fatalf("expected scaffolded config to explain external notes path, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "fresh_session: true") {
		t.Fatalf("expected scaffolded config to avoid extra Claude sessions in the default workflow, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "task\n          - finalize") && !strings.Contains(string(configContent), "- task\n          - finalize") {
		t.Fatalf("expected scaffolded config to use the task finalize helper, got %q", string(configContent))
	}
}

func TestWriteSkipsExistingConfigWithoutForce(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	if err := os.WriteFile(configPath, []byte("old config\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	if err := Write(configPath, false); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(content) != "old config\n" {
		t.Fatalf("expected existing config to be preserved, got %q", string(content))
	}
}

func TestWriteOverwritesWhenForceIsSet(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")

	if err := os.WriteFile(configPath, []byte("old config\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	if err := Write(configPath, true); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	configContent, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(configContent), "workspace: .") {
		t.Fatalf("expected sample config content, got %q", string(configContent))
	}
	assertScaffoldedParallelDefaults(t, configPath, string(configContent))
	if !strings.Contains(string(configContent), "type: agent") || !strings.Contains(string(configContent), "actor: coder") {
		t.Fatalf("expected scaffolded config to use runtime-resolved coder steps, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "address-peer-review") {
		t.Fatalf("expected scaffolded config to hand peer-review findings back to the coder, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "required_outputs:") || !strings.Contains(string(configContent), "{{ .TaskResultPath }}") {
		t.Fatalf("expected scaffolded config to declare required outputs for task artifacts, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "what was learned in this phase") {
		t.Fatalf("expected scaffolded config to request phase-learnings notes, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "missing self-verification instrumentation") {
		t.Fatalf("expected scaffolded config to review self-verification instrumentation, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "scripted screenshots") {
		t.Fatalf("expected scaffolded config to mention screenshot-based verification artifacts, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "Boundary metadata:") {
		t.Fatalf("expected scaffolded config to render task boundary metadata when present, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "Treat owns_paths as explicit edit authority") {
		t.Fatalf("expected scaffolded config to enforce owns_paths edit authority, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "contract files according to reads_contracts and provides_contracts") {
		t.Fatalf("expected scaffolded config to enforce contract metadata, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "violations of declared owns_paths or contract metadata") {
		t.Fatalf("expected scaffolded review to check ownership and contract violations, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "{{ .TaskNotesPath }}") {
		t.Fatalf("expected scaffolded config to explain external notes path, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "\ncoder:") || strings.Contains(string(configContent), "\nreviewer:") {
		t.Fatalf("expected scaffolded config to leave runtime role selection to CLI flags, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "fresh_session: true") {
		t.Fatalf("expected scaffolded config to avoid extra Claude sessions in the default workflow, got %q", string(configContent))
	}
}

func assertScaffoldedParallelDefaults(t *testing.T, configPath, content string) {
	t.Helper()

	for _, want := range []string{
		"parallel:",
		"enabled: false",
		"max_parallelism: 1",
		"worktree_root: .vibedrive/worktrees",
		"artifact_root: .vibedrive/task-runs",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("expected scaffolded config to include parallel default %q, got %q", want, content)
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load scaffolded config returned error: %v", err)
	}
	if cfg.Parallel.Enabled {
		t.Fatal("expected scaffolded config to keep parallel execution disabled")
	}
	if cfg.EffectiveParallelism() != 1 {
		t.Fatalf("expected scaffolded config to stay serial by default, got effective parallelism %d", cfg.EffectiveParallelism())
	}
}
