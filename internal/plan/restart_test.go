package plan

import "testing"

func TestResetProgressResetsStatusesAndClearsNotes(t *testing.T) {
	file := &File{
		Project: Project{
			Components: []Component{
				{
					ID:                "api",
					OwnedPaths:        StringList{"internal/api/**"},
					ReadsContracts:    StringList{"docs/schema.md"},
					ProvidesContracts: StringList{"docs/api.md"},
				},
			},
		},
		Tasks: []Task{
			{
				ID:                "done-task",
				Title:             "Done task",
				Status:            StatusDone,
				Component:         "api",
				OwnsPaths:         StringList{"internal/api/handler.go"},
				ReadsContracts:    StringList{"docs/schema.md"},
				ProvidesContracts: StringList{"docs/api.md"},
				ConflictsWith:     StringList{"manual-task"},
				Notes:             "finished cleanly",
			},
			{ID: "blocked-task", Title: "Blocked task", Status: StatusBlocked, Notes: "missing seed data"},
			{ID: "manual-task", Title: "Manual task", Status: StatusManual, Notes: "needs human review"},
			{ID: "in-progress-task", Title: "In progress task", Status: StatusInProgress, Notes: "split the migration first"},
		},
	}

	file.ResetProgress()

	for _, task := range file.Tasks {
		if task.Status != StatusTodo {
			t.Fatalf("expected task %q status %q, got %q", task.ID, StatusTodo, task.Status)
		}
		if task.Notes != "" {
			t.Fatalf("expected task %q notes to be cleared, got %q", task.ID, task.Notes)
		}
	}

	if len(file.Project.Components) != 1 || file.Project.Components[0].ID != "api" {
		t.Fatalf("expected component metadata to be preserved, got %#v", file.Project.Components)
	}
	task := file.Tasks[0]
	if task.Component != "api" || task.OwnsPaths[0] != "internal/api/handler.go" || task.ReadsContracts[0] != "docs/schema.md" || task.ProvidesContracts[0] != "docs/api.md" || task.ConflictsWith[0] != "manual-task" {
		t.Fatalf("expected task boundary metadata to be preserved, got %#v", task)
	}
}
