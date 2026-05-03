package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"vibedrive/internal/plan"
	"vibedrive/internal/tasknotes"
)

const resultDir = ".vibedrive/task-results"
const reviewDir = ".vibedrive/reviews"

type TaskResult struct {
	Status string `json:"status"`
	Notes  string `json:"notes,omitempty"`
}

type TaskArtifactPaths struct {
	RootDir       string
	ResultPath    string
	ReviewPath    string
	TaskNotesPath string
}

type FinalizeOptions struct {
	Workspace     string
	PlanFile      string
	TaskID        string
	ResultPath    string
	CommitMessage string
}

func ResultPath(workspace, taskID string) string {
	return artifactPath(workspace, resultDir, taskID, ".json")
}

func ReviewPath(workspace, taskID string) string {
	return artifactPath(workspace, reviewDir, taskID, ".json")
}

func WorkspaceArtifactPaths(workspace, taskID string) TaskArtifactPaths {
	workspace = cleanPathDefault(workspace, ".")
	return TaskArtifactPaths{
		RootDir:       filepath.Join(workspace, ".vibedrive"),
		ResultPath:    ResultPath(workspace, taskID),
		ReviewPath:    ReviewPath(workspace, taskID),
		TaskNotesPath: tasknotes.Path(workspace),
	}
}

func IsolatedArtifactPaths(rootDir, taskID string) TaskArtifactPaths {
	rootDir = cleanPathDefault(rootDir, ".")
	return TaskArtifactPaths{
		RootDir:       rootDir,
		ResultPath:    artifactPath(rootDir, "task-results", taskID, ".json"),
		ReviewPath:    artifactPath(rootDir, "reviews", taskID, ".json"),
		TaskNotesPath: filepath.Join(rootDir, "task-notes.yaml"),
	}
}

func artifactPath(workspace, dir, taskID, ext string) string {
	workspace = cleanPathDefault(workspace, ".")
	fileName := strings.NewReplacer("/", "_", "\\", "_").Replace(strings.TrimSpace(taskID))
	if fileName == "" {
		fileName = "task"
	}
	return filepath.Join(workspace, dir, fileName+ext)
}

func cleanPathDefault(path, fallback string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		path = fallback
	}
	return filepath.Clean(path)
}

func Finalize(ctx context.Context, opts FinalizeOptions, stdout, stderr io.Writer) error {
	file, task, err := loadTask(opts.PlanFile, opts.TaskID)
	if err != nil {
		return err
	}

	result, err := loadResult(opts.ResultPath)
	if err != nil {
		return err
	}

	status := normalizeStatus(result.Status)
	if status == "" {
		return fmt.Errorf("task result %s has unsupported status %q", opts.ResultPath, result.Status)
	}
	result.Status = status

	notesPath := taskNotesPathForResult(opts.Workspace, opts.ResultPath)
	reviewPath := reviewPathForResult(opts.Workspace, opts.ResultPath, opts.TaskID)

	notesFile, err := loadTaskNotes(notesPath, file)
	if err != nil {
		return err
	}

	switch status {
	case plan.StatusDone:
		if failedCommand, verifyErr := runVerifyCommands(ctx, opts.Workspace, task.VerifyCommands, stdout, stderr); verifyErr != nil {
			result.Status = plan.StatusInProgress
			result.Notes = appendFailureNote(result.Notes, failedCommand)
			if err := applyResult(file, opts.TaskID, result); err != nil {
				return err
			}
			if err := notesFile.Upsert(opts.TaskID, result.Status, result.Notes); err != nil {
				return err
			}
			if err := removeResultFile(opts.ResultPath); err != nil {
				return err
			}
			if err := removeArtifactFile(reviewPath); err != nil {
				return err
			}
			if err := notesFile.Save(); err != nil {
				return err
			}
			if err := file.Save(); err != nil {
				return err
			}
			return fmt.Errorf("verify task %q with %q: %w", opts.TaskID, failedCommand, verifyErr)
		}
	case plan.StatusInProgress, plan.StatusBlocked, plan.StatusManual:
	default:
		return fmt.Errorf("task result %s has unsupported status %q", opts.ResultPath, result.Status)
	}

	if err := applyResult(file, opts.TaskID, result); err != nil {
		return err
	}
	if err := notesFile.Upsert(opts.TaskID, result.Status, result.Notes); err != nil {
		return err
	}
	if err := removeResultFile(opts.ResultPath); err != nil {
		return err
	}
	if err := removeArtifactFile(reviewPath); err != nil {
		return err
	}
	if err := notesFile.Save(); err != nil {
		return err
	}
	if err := file.Save(); err != nil {
		return err
	}

	if status == plan.StatusBlocked || status == plan.StatusManual || status == plan.StatusInProgress || status == plan.StatusDone {
		return commitIfNeeded(ctx, opts.Workspace, opts.CommitMessage, stdout, stderr)
	}

	return nil
}

func loadTask(planPath, taskID string) (*plan.File, plan.Task, error) {
	file, err := plan.Load(planPath)
	if err != nil {
		return nil, plan.Task{}, err
	}

	task, ok := file.FindTask(taskID)
	if !ok {
		return nil, plan.Task{}, fmt.Errorf("task %q not found in %s", taskID, planPath)
	}

	return file, task, nil
}

func loadResult(path string) (TaskResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskResult{}, err
	}

	var result TaskResult
	if err := json.Unmarshal(data, &result); err != nil {
		return TaskResult{}, fmt.Errorf("parse task result %s: %w", path, err)
	}

	return result, nil
}

func applyResult(file *plan.File, taskID string, result TaskResult) error {
	for i := range file.Tasks {
		if file.Tasks[i].ID != taskID {
			continue
		}
		file.Tasks[i].Status = normalizeStatus(result.Status)
		file.Tasks[i].Notes = ""
		return nil
	}

	return fmt.Errorf("task %q not found in %s", taskID, file.Path)
}

func loadTaskNotes(path string, file *plan.File) (*tasknotes.File, error) {
	notesFile, err := tasknotes.Load(path)
	if err != nil {
		return nil, err
	}

	for _, task := range file.Tasks {
		note := strings.TrimSpace(task.Notes)
		if note == "" {
			continue
		}
		if existing, ok := notesFile.Find(task.ID); ok && strings.TrimSpace(existing.Notes) != "" {
			continue
		}
		if err := notesFile.Upsert(task.ID, task.Status, note); err != nil {
			return nil, err
		}
	}

	return notesFile, nil
}

func taskNotesPathForResult(workspace, resultPath string) string {
	return filepath.Join(artifactRootForResult(workspace, resultPath), "task-notes.yaml")
}

func reviewPathForResult(workspace, resultPath, taskID string) string {
	return artifactPath(artifactRootForResult(workspace, resultPath), "reviews", taskID, ".json")
}

func artifactRootForResult(workspace, resultPath string) string {
	resultPath = strings.TrimSpace(resultPath)
	if resultPath == "" {
		return filepath.Join(cleanPathDefault(workspace, "."), ".vibedrive")
	}
	return filepath.Clean(filepath.Dir(filepath.Dir(resultPath)))
}

func runVerifyCommands(ctx context.Context, workspace string, commands []string, stdout, stderr io.Writer) (string, error) {
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}

		cmd := shellCommand(ctx, command)
		cmd.Dir = workspace
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return command, err
		}
	}

	return "", nil
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "sh", "-lc", command)
}

func commitIfNeeded(ctx context.Context, workspace, message string, stdout, stderr io.Writer) error {
	return CommitIfNeeded(ctx, workspace, message, stdout, stderr)
}

func CommitIfNeeded(ctx context.Context, workspace, message string, stdout, stderr io.Writer) error {
	args := []string{"add", "-A", "--", "."}
	args = append(args, transientArtifactExcludes()...)
	if err := runGit(ctx, workspace, stdout, stderr, args...); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "--cached", "--quiet")
	if err := cmd.Run(); err == nil {
		return nil
	} else if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return runGit(ctx, workspace, stdout, stderr, "commit", "-m", message)
	} else {
		return fmt.Errorf("git diff --cached --quiet: %w", err)
	}
}

func transientArtifactExcludes() []string {
	return []string{
		":(exclude).vibedrive/task-results/**",
		":(exclude).vibedrive/reviews/**",
		":(exclude).vibedrive/task-runs/**",
		":(exclude).vibedrive/worktrees/**",
	}
}

func runGit(ctx context.Context, workspace string, stdout, stderr io.Writer, args ...string) error {
	cmdArgs := append([]string{"-C", workspace}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func removeResultFile(path string) error {
	return removeArtifactFile(path)
}

func removeArtifactFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func normalizeStatus(status string) string {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case plan.StatusDone:
		return plan.StatusDone
	case plan.StatusInProgress:
		return plan.StatusInProgress
	case plan.StatusBlocked:
		return plan.StatusBlocked
	case plan.StatusManual:
		return plan.StatusManual
	default:
		return ""
	}
}

func appendFailureNote(notes, command string) string {
	notes = strings.TrimSpace(notes)
	suffix := fmt.Sprintf("Verification failed while running %q.", command)
	if notes == "" {
		return suffix
	}
	return notes + " " + suffix
}
