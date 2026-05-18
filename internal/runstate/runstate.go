package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const RelativePath = ".vibedrive/run-state.json"

var mu sync.Mutex

var processAlive = func(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

type File struct {
	Runs []Run `json:"runs,omitempty"`
}

type Run struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	PlanFile  string    `json:"plan_file,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Tasks     []Task    `json:"tasks,omitempty"`
}

type Task struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Workflow  string    `json:"workflow,omitempty"`
	StepName  string    `json:"step_name,omitempty"`
	StepType  string    `json:"step_type,omitempty"`
	StepActor string    `json:"step_actor,omitempty"`
	StepIndex int       `json:"step_index"`
	StepTotal int       `json:"step_total"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Workspace string    `json:"workspace,omitempty"`
	PlanFile  string    `json:"plan_file,omitempty"`
}

func Path(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = "."
	}
	return filepath.Join(workspace, RelativePath)
}

func Load(path string) (*File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return &File{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, err
	}

	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse run state %s: %w", path, err)
	}
	normalize(&file)
	return &file, nil
}

func UpsertTask(path string, run Run, task Task) error {
	mu.Lock()
	defer mu.Unlock()

	file, err := Load(path)
	if err != nil {
		return err
	}

	now := time.Now()
	run.ID = strings.TrimSpace(run.ID)
	if run.ID == "" {
		return fmt.Errorf("run id is required")
	}
	run.UpdatedAt = now

	task.ID = strings.TrimSpace(task.ID)
	if task.ID == "" {
		return fmt.Errorf("task id is required")
	}
	task.Status = strings.TrimSpace(task.Status)
	task.Workflow = strings.TrimSpace(task.Workflow)
	task.StepName = strings.TrimSpace(task.StepName)
	task.StepType = strings.TrimSpace(task.StepType)
	task.StepActor = strings.TrimSpace(task.StepActor)
	task.Workspace = strings.TrimSpace(task.Workspace)
	task.PlanFile = cleanOptionalPath(task.PlanFile)
	if task.StartedAt.IsZero() {
		task.StartedAt = now
	}
	task.UpdatedAt = now

	runIndex := -1
	for i := range file.Runs {
		if file.Runs[i].ID == run.ID {
			runIndex = i
			break
		}
	}
	if runIndex == -1 {
		run.Tasks = []Task{task}
		file.Runs = append(file.Runs, run)
		return save(path, file)
	}

	existing := &file.Runs[runIndex]
	existing.PID = run.PID
	existing.Workspace = strings.TrimSpace(run.Workspace)
	existing.PlanFile = cleanOptionalPath(run.PlanFile)
	existing.UpdatedAt = now
	taskIndex := -1
	for i := range existing.Tasks {
		if existing.Tasks[i].ID == task.ID {
			taskIndex = i
			break
		}
	}
	if taskIndex == -1 {
		existing.Tasks = append(existing.Tasks, task)
	} else {
		if !existing.Tasks[taskIndex].StartedAt.IsZero() {
			task.StartedAt = existing.Tasks[taskIndex].StartedAt
		}
		existing.Tasks[taskIndex] = task
	}

	return save(path, file)
}

func ClearTask(path, runID, taskID string) error {
	mu.Lock()
	defer mu.Unlock()

	file, err := Load(path)
	if err != nil {
		return err
	}

	runID = strings.TrimSpace(runID)
	taskID = strings.TrimSpace(taskID)
	for runIndex := range file.Runs {
		if file.Runs[runIndex].ID != runID {
			continue
		}
		tasks := file.Runs[runIndex].Tasks[:0]
		for _, task := range file.Runs[runIndex].Tasks {
			if task.ID != taskID {
				tasks = append(tasks, task)
			}
		}
		file.Runs[runIndex].Tasks = tasks
		if len(file.Runs[runIndex].Tasks) == 0 {
			file.Runs = append(file.Runs[:runIndex], file.Runs[runIndex+1:]...)
		} else {
			file.Runs[runIndex].UpdatedAt = time.Now()
		}
		break
	}

	return save(path, file)
}

func ClearRun(path, runID string) error {
	mu.Lock()
	defer mu.Unlock()

	file, err := Load(path)
	if err != nil {
		return err
	}

	runID = strings.TrimSpace(runID)
	runs := file.Runs[:0]
	for _, run := range file.Runs {
		if run.ID != runID {
			runs = append(runs, run)
		}
	}
	file.Runs = runs
	return save(path, file)
}

func ActiveTasksForPlan(file *File, planPath string) map[string]Task {
	active := make(map[string]Task)
	if file == nil {
		return active
	}

	planPath = cleanOptionalPath(planPath)
	for _, run := range file.Runs {
		if !processAlive(run.PID) {
			continue
		}
		if planPath != "" && run.PlanFile != "" && run.PlanFile != planPath {
			continue
		}
		for _, task := range run.Tasks {
			if planPath != "" && task.PlanFile != "" && task.PlanFile != planPath {
				continue
			}
			existing, ok := active[task.ID]
			if !ok || task.UpdatedAt.After(existing.UpdatedAt) {
				active[task.ID] = task
			}
		}
	}
	return active
}

func save(path string, file *File) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("run state path is required")
	}
	normalize(file)
	if file == nil || len(file.Runs) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".run-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func normalize(file *File) {
	if file == nil {
		return
	}
	runs := file.Runs[:0]
	for _, run := range file.Runs {
		run.ID = strings.TrimSpace(run.ID)
		if run.ID == "" {
			continue
		}
		run.Workspace = strings.TrimSpace(run.Workspace)
		run.PlanFile = cleanOptionalPath(run.PlanFile)
		tasks := run.Tasks[:0]
		for _, task := range run.Tasks {
			task.ID = strings.TrimSpace(task.ID)
			if task.ID == "" {
				continue
			}
			task.Status = strings.TrimSpace(task.Status)
			task.Workflow = strings.TrimSpace(task.Workflow)
			task.StepName = strings.TrimSpace(task.StepName)
			task.StepType = strings.TrimSpace(task.StepType)
			task.StepActor = strings.TrimSpace(task.StepActor)
			task.Workspace = strings.TrimSpace(task.Workspace)
			task.PlanFile = cleanOptionalPath(task.PlanFile)
			tasks = append(tasks, task)
		}
		run.Tasks = tasks
		runs = append(runs, run)
	}
	file.Runs = runs
}

func cleanOptionalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}
