package bootstrap

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ghost_claude/internal/claude"
	"ghost_claude/internal/config"
)

type fakeClient struct {
	prompts []string
	closed  bool
}

func (f *fakeClient) RunPrompt(_ context.Context, _ *claude.Session, prompt string) error {
	f.prompts = append(f.prompts, prompt)
	return nil
}

func (f *fakeClient) Close(_ *claude.Session) error {
	f.closed = true
	return nil
}

func TestInitializerRunWritesConfigAndBootstrapsPlan(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")
	designPath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(designPath, []byte("# Design\n\nproject constraints\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	client := &fakeClient{}
	init := New(io.Discard, io.Discard)
	init.newClient = func(cfg *config.Config, stdout, stderr io.Writer) (promptClient, error) {
		if cfg.PlanFile != filepath.Join(dir, "ghost-plan.yaml") {
			t.Fatalf("expected plan path to resolve under workspace, got %q", cfg.PlanFile)
		}
		return client, nil
	}
	init.newSession = func(strategy string) (*claude.Session, error) {
		return &claude.Session{Strategy: strategy, ID: "session-1"}, nil
	}

	if err := init.Run(context.Background(), configPath, designPath, false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(client.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(client.prompts))
	}
	if !strings.Contains(client.prompts[0], "Create ghost-plan.yaml") {
		t.Fatalf("expected first prompt to create the plan file, got %q", client.prompts[0])
	}
	if strings.Contains(client.prompts[0], "Replace the file if it already exists.") {
		t.Fatalf("expected first prompt to omit replace instructions, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "DESIGN.md") {
		t.Fatalf("expected first prompt to reference DESIGN.md, got %q", client.prompts[0])
	}
	if strings.Contains(client.prompts[0], "TODO.md") {
		t.Fatalf("expected first prompt to stop assuming TODO.md, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[1], "Perform a critical review of the plan") {
		t.Fatalf("expected second prompt to request a critical plan review, got %q", client.prompts[1])
	}
	if strings.Contains(client.prompts[1], "/codex") {
		t.Fatalf("expected second prompt to stop requiring /codex, got %q", client.prompts[1])
	}
	if !client.closed {
		t.Fatal("expected client to be closed")
	}
}

func TestInitializerRunSkipsExistingPlanWithoutForce(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")
	planPath := filepath.Join(dir, "ghost-plan.yaml")
	sourcePath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(planPath, []byte("existing plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("existing source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	client := &fakeClient{}
	init := New(io.Discard, io.Discard)
	init.newClient = func(cfg *config.Config, stdout, stderr io.Writer) (promptClient, error) {
		return client, nil
	}

	if err := init.Run(context.Background(), configPath, sourcePath, false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(client.prompts) != 0 {
		t.Fatalf("expected no prompts when plan already exists, got %d", len(client.prompts))
	}
}

func TestInitializerRunRegeneratesPlanWithForce(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")
	planPath := filepath.Join(dir, "ghost-plan.yaml")
	sourcePath := filepath.Join(dir, "DESIGN.md")

	if err := os.WriteFile(planPath, []byte("existing plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("existing source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	client := &fakeClient{}
	init := New(io.Discard, io.Discard)
	init.newClient = func(cfg *config.Config, stdout, stderr io.Writer) (promptClient, error) {
		return client, nil
	}
	init.newSession = func(strategy string) (*claude.Session, error) {
		return &claude.Session{Strategy: strategy, ID: "session-1"}, nil
	}

	if err := init.Run(context.Background(), configPath, sourcePath, true); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(client.prompts) != 2 {
		t.Fatalf("expected forced init to regenerate the plan, got %d prompts", len(client.prompts))
	}
	if !strings.Contains(client.prompts[0], "ghost-plan.yaml") {
		t.Fatalf("expected first prompt to mention the plan path, got %q", client.prompts[0])
	}
	if _, err := os.Stat(planPath); !os.IsNotExist(err) {
		t.Fatalf("expected existing plan file to be removed before prompting, stat err=%v", err)
	}
}

func TestInitializerRunUsesWorkspaceFilesWhenSourceOmitted(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ghost-claude.yaml")

	if err := os.WriteFile(filepath.Join(dir, "DESIGN.md"), []byte("design\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "TEST_PLAN.md"), []byte("tests\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	client := &fakeClient{}
	init := New(io.Discard, io.Discard)
	init.newClient = func(cfg *config.Config, stdout, stderr io.Writer) (promptClient, error) {
		return client, nil
	}
	init.newSession = func(strategy string) (*claude.Session, error) {
		return &claude.Session{Strategy: strategy, ID: "session-1"}, nil
	}

	if err := init.Run(context.Background(), configPath, "", false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(client.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(client.prompts))
	}
	if !strings.Contains(client.prompts[0], "- DESIGN.md") {
		t.Fatalf("expected first prompt to include DESIGN.md as a source, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "- TEST_PLAN.md") {
		t.Fatalf("expected first prompt to include TEST_PLAN.md as a source, got %q", client.prompts[0])
	}
	if strings.Contains(client.prompts[0], "- ghost-claude.yaml") {
		t.Fatalf("expected generated config to be excluded from default sources, got %q", client.prompts[0])
	}
}
