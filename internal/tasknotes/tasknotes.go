package tasknotes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultRelativePath = ".vibedrive/task-notes.yaml"

type File struct {
	Path  string `yaml:"-"`
	Tasks []Task `yaml:"tasks,omitempty"`
}

type Task struct {
	ID     string `yaml:"id"`
	Status string `yaml:"status,omitempty"`
	Notes  string `yaml:"notes,omitempty"`
}

func Path(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = "."
	}
	return filepath.Join(workspace, DefaultRelativePath)
}

func Load(path string) (*File, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{Path: absPath}, nil
		}
		return nil, err
	}

	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse task notes %s: %w", absPath, err)
	}
	file.Path = absPath

	for i := range file.Tasks {
		file.Tasks[i].ID = strings.TrimSpace(file.Tasks[i].ID)
		file.Tasks[i].Status = strings.TrimSpace(file.Tasks[i].Status)
		file.Tasks[i].Notes = strings.TrimSpace(file.Tasks[i].Notes)
	}

	return &file, nil
}

func (f *File) Save() error {
	if f == nil {
		return fmt.Errorf("task notes file is nil")
	}
	if strings.TrimSpace(f.Path) == "" {
		return fmt.Errorf("task notes file path is required")
	}

	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}

	data, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(f.Path, data, 0o644)
}

func (f *File) Upsert(taskID, status, notes string) error {
	if f == nil {
		return fmt.Errorf("task notes file is nil")
	}

	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task id is required")
	}

	entry := Task{
		ID:     taskID,
		Status: strings.TrimSpace(status),
		Notes:  strings.TrimSpace(notes),
	}
	for i := range f.Tasks {
		if f.Tasks[i].ID != taskID {
			continue
		}
		f.Tasks[i] = entry
		return nil
	}

	f.Tasks = append(f.Tasks, entry)
	return nil
}

func (f *File) Find(taskID string) (Task, bool) {
	if f == nil {
		return Task{}, false
	}

	taskID = strings.TrimSpace(taskID)
	for _, task := range f.Tasks {
		if task.ID == taskID {
			return task, true
		}
	}
	return Task{}, false
}

func Remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
