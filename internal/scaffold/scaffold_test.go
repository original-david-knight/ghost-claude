package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePreservesExistingTODO(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")
	todoPath := filepath.Join(dir, "TODO.md")

	if err := os.WriteFile(todoPath, []byte("existing todo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

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
	if !strings.Contains(string(configContent), "plan_file: ghost-plan.yaml") {
		t.Fatalf("expected plan mode sample config, got %q", string(configContent))
	}
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
	if strings.Contains(string(configContent), "fresh_session: true") {
		t.Fatalf("expected scaffolded config to avoid extra Claude sessions in the default workflow, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "task\n          - finalize") && !strings.Contains(string(configContent), "- task\n          - finalize") {
		t.Fatalf("expected scaffolded config to use the task finalize helper, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "todo_file:") {
		t.Fatalf("expected scaffolded config to stop pinning todo_file, got %q", string(configContent))
	}

	content, err := os.ReadFile(todoPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(content) != "existing todo\n" {
		t.Fatalf("expected existing TODO to be preserved, got %q", string(content))
	}
}

func TestWriteFailsWhenConfigExists(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	if err := os.WriteFile(configPath, []byte("old config\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := Write(configPath, false)
	if err == nil {
		t.Fatal("expected Write to fail when config already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got %v", err)
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
	configPath := filepath.Join(dir, "ghost-claude.yaml")
	todoPath := filepath.Join(dir, "TODO.md")

	if err := os.WriteFile(configPath, []byte("old config\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(todoPath, []byte("old todo\n"), 0o644); err != nil {
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
	if !strings.Contains(string(configContent), "type: agent") || !strings.Contains(string(configContent), "actor: coder") {
		t.Fatalf("expected scaffolded config to use runtime-resolved coder steps, got %q", string(configContent))
	}
	if !strings.Contains(string(configContent), "address-peer-review") {
		t.Fatalf("expected scaffolded config to hand peer-review findings back to the coder, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "\ncoder:") || strings.Contains(string(configContent), "\nreviewer:") {
		t.Fatalf("expected scaffolded config to leave runtime role selection to CLI flags, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "fresh_session: true") {
		t.Fatalf("expected scaffolded config to avoid extra Claude sessions in the default workflow, got %q", string(configContent))
	}
	if strings.Contains(string(configContent), "todo_file:") {
		t.Fatalf("expected scaffolded config to stop pinning todo_file, got %q", string(configContent))
	}

	todoContent, err := os.ReadFile(todoPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(todoContent) != "old todo\n" {
		t.Fatalf("expected existing TODO to be preserved even with force, got %q", string(todoContent))
	}
}

func TestWriteDoesNotCreateTODOByDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	if err := Write(configPath, false); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "TODO.md")); !os.IsNotExist(err) {
		t.Fatalf("expected TODO.md to be left alone, stat err=%v", err)
	}
}
