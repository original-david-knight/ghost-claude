package runstate

import (
	"testing"
	"time"
)

func TestActiveTasksForPlanSkipsDeadRunPID(t *testing.T) {
	oldProcessAlive := processAlive
	processAlive = func(pid int) bool {
		return pid == 100
	}
	defer func() {
		processAlive = oldProcessAlive
	}()

	state := &File{
		Runs: []Run{
			{
				ID:       "dead",
				PID:      200,
				PlanFile: "/repo/vibedrive-plan.yaml",
				Tasks: []Task{{
					ID:        "old-task",
					PlanFile:  "/repo/vibedrive-plan.yaml",
					UpdatedAt: time.Now(),
				}},
			},
			{
				ID:       "alive",
				PID:      100,
				PlanFile: "/repo/vibedrive-plan.yaml",
				Tasks: []Task{{
					ID:        "current-task",
					PlanFile:  "/repo/vibedrive-plan.yaml",
					UpdatedAt: time.Now(),
				}},
			},
		},
	}

	active := ActiveTasksForPlan(state, "/repo/vibedrive-plan.yaml")
	if _, ok := active["old-task"]; ok {
		t.Fatalf("expected old-task from dead run to be ignored: %#v", active)
	}
	if _, ok := active["current-task"]; !ok {
		t.Fatalf("expected current-task from live run to be active: %#v", active)
	}
}
