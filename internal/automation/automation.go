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

	"ghost_claude/internal/plan"
)

const resultDir = ".ghost-claude/task-results"
const reviewDir = ".ghost-claude/reviews"

type TaskResult struct {
	Status string `json:"status"`
	Notes  string `json:"notes,omitempty"`
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

func artifactPath(workspace, dir, taskID, ext string) string {
	fileName := strings.NewReplacer("/", "_", "\\", "_").Replace(strings.TrimSpace(taskID))
	if fileName == "" {
		fileName = "task"
	}
	return filepath.Join(workspace, dir, fileName+ext)
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

	switch status {
	case plan.StatusDone:
		if failedCommand, verifyErr := runVerifyCommands(ctx, opts.Workspace, task.VerifyCommands, stdout, stderr); verifyErr != nil {
			result.Status = plan.StatusInProgress
			result.Notes = appendFailureNote(result.Notes, failedCommand)
			if err := applyResult(file, opts.TaskID, result); err != nil {
				return err
			}
			if err := removeResultFile(opts.ResultPath); err != nil {
				return err
			}
			if err := removeArtifactFile(ReviewPath(opts.Workspace, opts.TaskID)); err != nil {
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
	if err := removeResultFile(opts.ResultPath); err != nil {
		return err
	}
	if err := removeArtifactFile(ReviewPath(opts.Workspace, opts.TaskID)); err != nil {
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
		file.Tasks[i].Notes = strings.TrimSpace(result.Notes)
		return nil
	}

	return fmt.Errorf("task %q not found in %s", taskID, file.Path)
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
	if err := runGit(ctx, workspace, stdout, stderr, "add", "-A"); err != nil {
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
