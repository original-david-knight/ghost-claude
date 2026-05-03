package automation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/plan"
	"vibedrive/internal/tasknotes"
)

func TestFinalizeMarksTaskDoneAndCommitsChanges(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	writeFile(t, planPath, `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    status: todo
    verify_commands:
      - git rev-parse --is-inside-work-tree
`)

	resultPath := ResultPath(dir, "scaffold")
	reviewPath := ReviewPath(dir, "scaffold")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	writeFile(t, resultPath, `{"status":"done","notes":"finished work"}`)
	writeFile(t, reviewPath, `{"decision":"approved","summary":"looks good","findings":[]}`)

	err := Finalize(context.Background(), FinalizeOptions{
		Workspace:     dir,
		PlanFile:      planPath,
		TaskID:        "scaffold",
		ResultPath:    resultPath,
		CommitMessage: "feat: finish scaffold",
	}, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatalf("Finalize returned error: %v", err)
	}

	loaded, err := plan.Load(planPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	task, ok := loaded.FindTask("scaffold")
	if !ok {
		t.Fatal("expected task scaffold to exist")
	}
	if task.Status != plan.StatusDone {
		t.Fatalf("expected task status %q, got %q", plan.StatusDone, task.Status)
	}
	if task.Notes != "" {
		t.Fatalf("expected task notes to stay out of the plan, got %q", task.Notes)
	}
	notesFile, err := tasknotes.Load(tasknotes.Path(dir))
	if err != nil {
		t.Fatalf("Load task notes returned error: %v", err)
	}
	note, ok := notesFile.Find("scaffold")
	if !ok {
		t.Fatal("expected task notes entry for scaffold")
	}
	if note.Status != plan.StatusDone {
		t.Fatalf("expected task notes status %q, got %q", plan.StatusDone, note.Status)
	}
	if note.Notes != "finished work" {
		t.Fatalf("expected task notes to round-trip, got %q", note.Notes)
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Fatalf("expected result file to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(reviewPath); !os.IsNotExist(err) {
		t.Fatalf("expected review file to be removed, stat err=%v", err)
	}

	commitMessage := runCmd(t, dir, "git", "-C", dir, "log", "-1", "--pretty=%s")
	if strings.TrimSpace(commitMessage) != "feat: finish scaffold" {
		t.Fatalf("expected commit message %q, got %q", "feat: finish scaffold", commitMessage)
	}
}

func TestFinalizeMarksTaskInProgressWhenVerificationFails(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	writeFile(t, planPath, `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    status: todo
    verify_commands:
      - git show definitely-not-a-real-ref
`)

	resultPath := ResultPath(dir, "scaffold")
	reviewPath := ReviewPath(dir, "scaffold")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	writeFile(t, resultPath, `{"status":"done","notes":"implementation complete"}`)
	writeFile(t, reviewPath, `{"decision":"changes_requested","summary":"needs fixes","findings":["add a real verification command"]}`)

	err := Finalize(context.Background(), FinalizeOptions{
		Workspace:     dir,
		PlanFile:      planPath,
		TaskID:        "scaffold",
		ResultPath:    resultPath,
		CommitMessage: "feat: finish scaffold",
	}, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected Finalize to fail when verification fails")
	}

	loaded, loadErr := plan.Load(planPath)
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}

	task, ok := loaded.FindTask("scaffold")
	if !ok {
		t.Fatal("expected task scaffold to exist")
	}
	if task.Status != plan.StatusInProgress {
		t.Fatalf("expected task status %q, got %q", plan.StatusInProgress, task.Status)
	}
	if task.Notes != "" {
		t.Fatalf("expected verification failure notes to stay out of the plan, got %q", task.Notes)
	}
	notesFile, err := tasknotes.Load(tasknotes.Path(dir))
	if err != nil {
		t.Fatalf("Load task notes returned error: %v", err)
	}
	note, ok := notesFile.Find("scaffold")
	if !ok {
		t.Fatal("expected task notes entry for scaffold")
	}
	if note.Status != plan.StatusInProgress {
		t.Fatalf("expected task notes status %q, got %q", plan.StatusInProgress, note.Status)
	}
	if !strings.Contains(note.Notes, "Verification failed while running") {
		t.Fatalf("expected verification failure notes, got %q", note.Notes)
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Fatalf("expected result file to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(reviewPath); !os.IsNotExist(err) {
		t.Fatalf("expected review file to be removed, stat err=%v", err)
	}
	if _, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "HEAD").CombinedOutput(); err == nil {
		t.Fatal("expected no commit to be created when verification fails")
	}
}

func TestFinalizeMigratesLegacyPlanNotesToTaskNotesFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	writeFile(t, planPath, `project:
  name: demo
tasks:
  - id: old-task
    title: Old task
    status: done
    notes: keep this prior note
  - id: current-task
    title: Current task
    status: todo
`)

	resultPath := ResultPath(dir, "current-task")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	writeFile(t, resultPath, `{"status":"in_progress","notes":"current note"}`)

	err := Finalize(context.Background(), FinalizeOptions{
		Workspace:     dir,
		PlanFile:      planPath,
		TaskID:        "current-task",
		ResultPath:    resultPath,
		CommitMessage: "chore: record progress",
	}, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatalf("Finalize returned error: %v", err)
	}

	loaded, err := plan.Load(planPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	for _, task := range loaded.Tasks {
		if task.Notes != "" {
			t.Fatalf("expected task %q notes to stay out of the plan, got %q", task.ID, task.Notes)
		}
	}

	notesFile, err := tasknotes.Load(tasknotes.Path(dir))
	if err != nil {
		t.Fatalf("Load task notes returned error: %v", err)
	}
	oldNote, ok := notesFile.Find("old-task")
	if !ok {
		t.Fatal("expected legacy note entry for old-task")
	}
	if oldNote.Notes != "keep this prior note" {
		t.Fatalf("expected legacy note to be migrated, got %q", oldNote.Notes)
	}
	currentNote, ok := notesFile.Find("current-task")
	if !ok {
		t.Fatal("expected note entry for current-task")
	}
	if currentNote.Notes != "current note" {
		t.Fatalf("expected current note, got %q", currentNote.Notes)
	}
}

func TestWorkspaceArtifactPathsPreserveCurrentLocations(t *testing.T) {
	dir := t.TempDir()

	paths := WorkspaceArtifactPaths(dir, "api/db")

	if paths.RootDir != filepath.Join(dir, ".vibedrive") {
		t.Fatalf("expected root dir under workspace .vibedrive, got %q", paths.RootDir)
	}
	if paths.ResultPath != ResultPath(dir, "api/db") {
		t.Fatalf("expected result path %q, got %q", ResultPath(dir, "api/db"), paths.ResultPath)
	}
	if paths.ReviewPath != ReviewPath(dir, "api/db") {
		t.Fatalf("expected review path %q, got %q", ReviewPath(dir, "api/db"), paths.ReviewPath)
	}
	if paths.TaskNotesPath != tasknotes.Path(dir) {
		t.Fatalf("expected task notes path %q, got %q", tasknotes.Path(dir), paths.TaskNotesPath)
	}
}

func TestIsolatedArtifactPathsUseDedicatedRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".vibedrive", "task-runs", "001-api-db-abcdef123456")

	paths := IsolatedArtifactPaths(root, "api/db")

	if paths.RootDir != root {
		t.Fatalf("expected root dir %q, got %q", root, paths.RootDir)
	}
	wantResult := filepath.Join(root, "task-results", "api_db.json")
	if paths.ResultPath != wantResult {
		t.Fatalf("expected result path %q, got %q", wantResult, paths.ResultPath)
	}
	wantReview := filepath.Join(root, "reviews", "api_db.json")
	if paths.ReviewPath != wantReview {
		t.Fatalf("expected review path %q, got %q", wantReview, paths.ReviewPath)
	}
	wantNotes := filepath.Join(root, "task-notes.yaml")
	if paths.TaskNotesPath != wantNotes {
		t.Fatalf("expected task notes path %q, got %q", wantNotes, paths.TaskNotesPath)
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()

	runCmd(t, dir, "git", "-C", dir, "init")
	runCmd(t, dir, "git", "-C", dir, "config", "user.email", "test@example.com")
	runCmd(t, dir, "git", "-C", dir, "config", "user.name", "Test User")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}

func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}
