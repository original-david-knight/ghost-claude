package tasknotes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileReturnsEmptyNotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".vibedrive", "task-notes.yaml")

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if file.Path == "" {
		t.Fatal("expected notes path to be set")
	}
	if len(file.Tasks) != 0 {
		t.Fatalf("expected no note entries, got %d", len(file.Tasks))
	}
}

func TestSaveAndLoadTaskNotes(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if err := file.Upsert("scaffold", "done", "finished work"); err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}
	if err := file.Save(); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	note, ok := loaded.Find("scaffold")
	if !ok {
		t.Fatal("expected scaffold note")
	}
	if note.Status != "done" {
		t.Fatalf("expected status %q, got %q", "done", note.Status)
	}
	if note.Notes != "finished work" {
		t.Fatalf("expected notes %q, got %q", "finished work", note.Notes)
	}
}

func TestRemoveIgnoresMissingTaskNotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".vibedrive", "task-notes.yaml")

	if err := Remove(path); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte("tasks: []\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected notes file to be removed, stat err=%v", err)
	}
}
