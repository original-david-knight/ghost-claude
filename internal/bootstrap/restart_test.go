package bootstrap

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/config"
	"vibedrive/internal/plan"
	"vibedrive/internal/scaffold"
	"vibedrive/internal/tasknotes"
	"vibedrive/pkg/agentcli/claude"
)

func TestInitializerRestartReplansFromNotesAndResetsProgress(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vibedrive.yaml")
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	designPath := filepath.Join(dir, "DESIGN.md")

	if err := scaffold.Write(configPath, false); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := os.WriteFile(designPath, []byte("# Design\n\nShip the project cleanly.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(planPath, []byte(`project:
  name: demo
  objective: Ship the project.
  source_docs:
    - DESIGN.md
  constraint_files:
    - DESIGN.md
tasks:
  - id: seed-fixtures
    title: Seed fixtures
    status: done
  - id: checkpoint-e2e
    title: End-to-end checkpoint
    status: blocked
`), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	notesPath := tasknotes.Path(dir)
	notesFile, err := tasknotes.Load(notesPath)
	if err != nil {
		t.Fatalf("Load task notes returned error: %v", err)
	}
	if err := notesFile.Upsert("seed-fixtures", plan.StatusDone, "Tests were flaky because fixture setup happened too late."); err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}
	if err := notesFile.Upsert("checkpoint-e2e", plan.StatusBlocked, "Split browser verification into a dedicated checkpoint before packaging."); err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}
	if err := notesFile.Save(); err != nil {
		t.Fatalf("Save task notes returned error: %v", err)
	}

	client := &fakeClient{}
	init := New(io.Discard, io.Discard)
	init.newClient = func(cfg *config.Config, stdout, stderr io.Writer) (promptClient, error) {
		if cfg.PlanFile != planPath {
			t.Fatalf("expected plan path %q, got %q", planPath, cfg.PlanFile)
		}
		return client, nil
	}
	init.newSession = func(strategy string) (*claude.Session, error) {
		return &claude.Session{Strategy: strategy, ID: "session-1"}, nil
	}

	if err := init.Restart(context.Background(), configPath); err != nil {
		t.Fatalf("Restart returned error: %v", err)
	}

	if len(client.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(client.prompts))
	}
	if !strings.Contains(client.prompts[0], "Tests were flaky because fixture setup happened too late.") {
		t.Fatalf("expected first prompt to include prior notes, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "DESIGN.md") {
		t.Fatalf("expected first prompt to include DESIGN.md, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "keep testing, verification, and cleanup work attached to the implementation task") {
		t.Fatalf("expected first prompt to keep routine testing and cleanup inline, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "self-verification path agents can run without manual help") {
		t.Fatalf("expected first prompt to require self-verifying tasks, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "preserve useful prior task notes") {
		t.Fatalf("expected first prompt to preserve useful prior notes as plan changes, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "regenerate component, ownership, contract, and integration-boundary metadata from the current source docs") {
		t.Fatalf("expected first prompt to regenerate boundaries from current docs and learnings, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "before finalizing tasks, identify the repository components") {
		t.Fatalf("expected first prompt to require boundary analysis before restarted task generation, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "context reduction, not merely speed") {
		t.Fatalf("expected first prompt to optimize restarted tasks for context reduction, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "explicit edit authority") {
		t.Fatalf("expected first prompt to require explicit edit authority, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "cross-cutting implementation task must depend on a preceding contract or foundation task") {
		t.Fatalf("expected first prompt to require foundation tasks before cross-cutting work, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "screenshot instrumentation or seeded test data") {
		t.Fatalf("expected first prompt to include preparatory verification tooling, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "prior-run notes or planned work show unresolved follow-up") {
		t.Fatalf("expected first prompt to preserve trigger-based debt follow-up from prior notes, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "not as proof that replanning can observe actual changed-file counts or other finalize-time facts") {
		t.Fatalf("expected first prompt to distinguish replanning heuristics from finalize-time facts, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "do not restore or add standalone tech-debt tasks on a fixed schedule") {
		t.Fatalf("expected first prompt to reject fixed tech-debt cadence, got %q", client.prompts[0])
	}
	if strings.Contains(client.prompts[0], "after every block of 5 significant dev steps") {
		t.Fatalf("expected first prompt to remove the old tech-debt cadence, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "what it learned in that phase") {
		t.Fatalf("expected first prompt to require phase-learning notes for future reruns, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[0], "reset every task status to todo") {
		t.Fatalf("expected first prompt to require todo reset, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[1], "missing trigger-justified standalone tech-debt tasks") {
		t.Fatalf("expected second prompt to review missing trigger-based tech-debt follow-up, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "self-verification path agents can run without manual help") {
		t.Fatalf("expected second prompt to review self-verification paths, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "missing or stale component, ownership, contract, or integration-boundary analysis") {
		t.Fatalf("expected second prompt to review stale boundary analysis, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "excessive context") {
		t.Fatalf("expected second prompt to review excessive context requirements, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "missing interfaces, shared contracts") {
		t.Fatalf("expected second prompt to review missing interfaces and contracts, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "ambiguous ownership or unsafe parallel assumptions") {
		t.Fatalf("expected second prompt to review ambiguous ownership and unsafe parallel assumptions, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "reject tasks that are cross-cutting without a preceding contract or foundation task") {
		t.Fatalf("expected second prompt to reject cross-cutting tasks without foundation tasks, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "screenshot capture") {
		t.Fatalf("expected second prompt to review screenshot instrumentation, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "replanning can observe actual changed-file counts or other finalize-time facts") {
		t.Fatalf("expected second prompt to review replanning-boundary violations, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "standalone cleanup or test-coverage tasks") {
		t.Fatalf("expected second prompt to review unnecessary standalone cleanup tasks, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "capturing phase learnings") {
		t.Fatalf("expected second prompt to review phase-learning coverage, got %q", client.prompts[1])
	}
	if strings.Contains(client.prompts[1], "after each block of 5 significant dev steps") {
		t.Fatalf("expected second prompt to remove the old tech-debt cadence review, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "leftover task notes copied into the plan") {
		t.Fatalf("expected second prompt to review stale notes, got %q", client.prompts[1])
	}
	if !client.closed {
		t.Fatal("expected client to be closed")
	}

	updatedPlan, err := plan.Load(planPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	for _, task := range updatedPlan.Tasks {
		if task.Status != plan.StatusTodo {
			t.Fatalf("expected task %q status %q, got %q", task.ID, plan.StatusTodo, task.Status)
		}
		if task.Notes != "" {
			t.Fatalf("expected task %q notes to be cleared, got %q", task.ID, task.Notes)
		}
	}
	if _, err := os.Stat(notesPath); !os.IsNotExist(err) {
		t.Fatalf("expected task notes file to be cleared after restart, stat err=%v", err)
	}
}
