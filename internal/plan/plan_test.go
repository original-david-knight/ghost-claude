package plan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsEmptyStatusToTodo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: first
    title: First task
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := file.Tasks[0].Status; got != StatusTodo {
		t.Fatalf("expected default status %q, got %q", StatusTodo, got)
	}
}

func TestFindNextReadyPrefersInProgressTask(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusTodo},
			{ID: "second", Title: "second", Status: StatusInProgress},
		},
	}

	task, err := file.FindNextReady()
	if err != nil {
		t.Fatalf("FindNextReady returned error: %v", err)
	}

	if task.ID != "second" {
		t.Fatalf("expected in-progress task, got %q", task.ID)
	}
}

func TestFindNextReadyHonorsDependencies(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusTodo},
			{ID: "second", Title: "second", Status: StatusTodo, Deps: StringList{"first"}},
		},
	}

	task, err := file.FindNextReady()
	if err != nil {
		t.Fatalf("FindNextReady returned error: %v", err)
	}

	if task.ID != "first" {
		t.Fatalf("expected dependency-free task, got %q", task.ID)
	}
}

func TestFindNextReadyReturnsNoReadyWhenBlockedByDeps(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusBlocked},
			{ID: "second", Title: "second", Status: StatusTodo, Deps: StringList{"first"}},
		},
	}

	_, err := file.FindNextReady()
	if !errors.Is(err, ErrNoReadyTasks) {
		t.Fatalf("expected ErrNoReadyTasks, got %v", err)
	}
}

func TestSavePersistsUpdatedStatusWithoutTaskNotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	file := &File{
		Path:    path,
		Project: Project{Name: "demo"},
		Tasks: []Task{
			{ID: "first", Title: "First task", Status: StatusDone, Notes: "finished"},
		},
	}

	if err := file.Save(); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := loaded.Tasks[0].Status; got != StatusDone {
		t.Fatalf("expected status %q, got %q", StatusDone, got)
	}
	if got := loaded.Tasks[0].Notes; got != "" {
		t.Fatalf("expected notes to stay out of the plan file, got %q", got)
	}
}

func TestLoadReadsLegacyTaskNotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: first
    title: First task
    status: done
    notes: finished
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := loaded.Tasks[0].Notes; got != "finished" {
		t.Fatalf("expected legacy notes to load, got %q", got)
	}
}

func TestLoadFlattensColonPrefixedAcceptanceItem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: demo-task
    title: Demo task
    status: todo
    acceptance:
      - demo.mp4 exists
      - Recording review: no tile pops, smooth descent, recognizable imagery throughout
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := "Recording review: no tile pops, smooth descent, recognizable imagery throughout"
	if got := file.Tasks[0].Acceptance[1]; got != want {
		t.Fatalf("expected acceptance item %q, got %q", want, got)
	}
}
