package view

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"vibedrive/internal/config"
	"vibedrive/internal/plan"
)

func Render(w io.Writer, file *plan.File, cfg *config.Config) error {
	if file == nil {
		return fmt.Errorf("plan file is nil")
	}
	if w == nil {
		return fmt.Errorf("writer is nil")
	}

	counts := countStatuses(file.Tasks)
	total := len(file.Tasks)

	fmt.Fprintf(w, "Project: %s\n", valueOr(file.Project.Name, "(unnamed)"))
	if strings.TrimSpace(file.Project.Objective) != "" {
		fmt.Fprintf(w, "Objective: %s\n", strings.TrimSpace(file.Project.Objective))
	}
	fmt.Fprintf(w, "Plan: %s\n", file.Path)
	if cfg != nil {
		fmt.Fprintf(w, "Workspace: %s\n", cfg.Workspace)
		fmt.Fprintf(w, "Parallelism: %s\n", parallelSummary(cfg))
	}
	fmt.Fprintf(w, "Progress: %d/%d tasks done (%d%%)\n", counts[plan.StatusDone], total, progressPercent(counts[plan.StatusDone], total))
	fmt.Fprintf(w, "Statuses: done=%d in_progress=%d blocked=%d manual=%d todo=%d\n",
		counts[plan.StatusDone],
		counts[plan.StatusInProgress],
		counts[plan.StatusBlocked],
		counts[plan.StatusManual],
		counts[plan.StatusTodo],
	)
	fmt.Fprintf(w, "Next: %s\n", nextSummary(file))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Legend: [x]=done [~]=in_progress [!]=blocked [?]=manual [ ]=todo")
	fmt.Fprintln(w, "Note: step completion is inferred from the enclosing task status; per-step history is not persisted.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Task Graph:")

	graph := newTaskGraph(file.Tasks)
	graph.render(w, cfg)
	return nil
}

type statusCounts map[string]int

func countStatuses(tasks []plan.Task) statusCounts {
	counts := statusCounts{
		plan.StatusDone:       0,
		plan.StatusInProgress: 0,
		plan.StatusBlocked:    0,
		plan.StatusManual:     0,
		plan.StatusTodo:       0,
	}
	for _, task := range tasks {
		counts[task.Status]++
	}
	return counts
}

func progressPercent(done, total int) int {
	if total == 0 {
		return 0
	}
	return (done * 100) / total
}

func parallelSummary(cfg *config.Config) string {
	if cfg.EffectiveParallelism() <= 1 {
		return "serial"
	}
	return fmt.Sprintf("enabled (max %d)", cfg.EffectiveParallelism())
}

func nextSummary(file *plan.File) string {
	task, err := file.FindNextReady()
	switch {
	case err == nil:
		return fmt.Sprintf("%s - %s", task.ID, task.Title)
	case errors.Is(err, plan.ErrAllTasksDone):
		return "all tasks complete"
	case errors.Is(err, plan.ErrNoReadyTasks):
		return fmt.Sprintf("none ready; unfinished tasks: %s", summarizeUnfinished(file.UnfinishedTasks()))
	default:
		return err.Error()
	}
}

func summarizeUnfinished(tasks []plan.Task) string {
	if len(tasks) == 0 {
		return "none"
	}

	parts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		parts = append(parts, fmt.Sprintf("%s(%s)", task.ID, task.Status))
	}
	return strings.Join(parts, ", ")
}

type taskGraph struct {
	tasks    []plan.Task
	children map[string][]plan.Task
	roots    []plan.Task
}

func newTaskGraph(tasks []plan.Task) taskGraph {
	graph := taskGraph{
		tasks:    append([]plan.Task(nil), tasks...),
		children: make(map[string][]plan.Task, len(tasks)),
	}

	for _, task := range tasks {
		for _, depID := range task.Deps {
			graph.children[depID] = append(graph.children[depID], task)
		}
	}

	for _, task := range tasks {
		if len(task.Deps) == 0 {
			graph.roots = append(graph.roots, task)
		}
	}
	if len(graph.roots) == 0 {
		graph.roots = append(graph.roots, tasks...)
		return graph
	}

	return graph
}

func (g taskGraph) render(w io.Writer, cfg *config.Config) {
	if len(g.tasks) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}

	seen := make(map[string]bool, len(g.tasks))
	for i, task := range g.roots {
		g.renderTask(w, cfg, task, "", i == len(g.roots)-1, seen, nil)
	}
	for _, task := range g.tasks {
		if seen[task.ID] {
			continue
		}
		g.renderTask(w, cfg, task, "", true, seen, nil)
	}
}

func (g taskGraph) renderTask(w io.Writer, cfg *config.Config, task plan.Task, prefix string, last bool, seen map[string]bool, stack map[string]bool) {
	connector := "|-- "
	childPrefix := prefix + "|   "
	if last {
		connector = "`-- "
		childPrefix = prefix + "    "
	}

	shown := seen[task.ID]
	fmt.Fprintf(w, "%s%s%s %s (%s) - %s\n", prefix, connector, statusMarker(task.Status), task.ID, task.Status, task.Title)
	if shown {
		fmt.Fprintf(w, "%s    steps: shown above\n", childPrefix)
		return
	}
	seen[task.ID] = true

	fmt.Fprintf(w, "%s    %s\n", childPrefix, stepSummary(cfg, task))

	if stack == nil {
		stack = make(map[string]bool)
	}
	if stack[task.ID] {
		fmt.Fprintf(w, "%s    cycle detected at %s\n", childPrefix, task.ID)
		return
	}

	nextStack := copyStack(stack)
	nextStack[task.ID] = true

	children := g.children[task.ID]
	for i, child := range children {
		g.renderTask(w, cfg, child, childPrefix, i == len(children)-1, seen, nextStack)
	}
}

func copyStack(stack map[string]bool) map[string]bool {
	next := make(map[string]bool, len(stack))
	for id, ok := range stack {
		next[id] = ok
	}
	return next
}

func statusMarker(status string) string {
	switch status {
	case plan.StatusDone:
		return "[x]"
	case plan.StatusInProgress:
		return "[~]"
	case plan.StatusBlocked:
		return "[!]"
	case plan.StatusManual:
		return "[?]"
	default:
		return "[ ]"
	}
}

func stepSummary(cfg *config.Config, task plan.Task) string {
	if cfg == nil {
		return "steps: unavailable (config not loaded)"
	}

	steps, workflowName, err := stepsForTask(cfg, task)
	if err != nil {
		return fmt.Sprintf("steps: unavailable (%v)", err)
	}
	if len(steps) == 0 {
		return fmt.Sprintf("steps (%s): none", workflowName)
	}

	labels := make([]string, 0, len(steps))
	for _, step := range steps {
		labels = append(labels, fmt.Sprintf("%s %s", statusMarker(task.Status), stepLabel(step)))
	}
	return fmt.Sprintf("steps (%s): %s", workflowName, strings.Join(labels, " -> "))
}

func stepsForTask(cfg *config.Config, task plan.Task) ([]config.Step, string, error) {
	if len(cfg.Workflows) == 0 {
		if len(cfg.Steps) == 0 {
			return nil, "", fmt.Errorf("no steps configured")
		}
		return cfg.Steps, "default", nil
	}

	workflowName := strings.TrimSpace(task.Workflow)
	if workflowName == "" {
		workflowName = strings.TrimSpace(cfg.DefaultWorkflow)
	}
	if workflowName == "" && len(cfg.Workflows) == 1 {
		names := make([]string, 0, len(cfg.Workflows))
		for name := range cfg.Workflows {
			names = append(names, name)
		}
		sort.Strings(names)
		workflowName = names[0]
	}
	if workflowName == "" {
		return nil, "", fmt.Errorf("task %q does not declare a workflow and no default_workflow is configured", task.ID)
	}

	workflow, ok := cfg.Workflows[workflowName]
	if !ok {
		return nil, "", fmt.Errorf("task %q references unknown workflow %q", task.ID, workflowName)
	}
	return workflow.Steps, workflowName, nil
}

func stepLabel(step config.Step) string {
	name := strings.TrimSpace(step.Name)
	if name == "" {
		name = "(unnamed)"
	}

	stepType := strings.TrimSpace(strings.ToLower(step.Type))
	switch stepType {
	case config.StepTypeAgent:
		actor := strings.TrimSpace(strings.ToLower(step.Actor))
		if actor == "" {
			actor = "agent"
		}
		return fmt.Sprintf("%s(agent:%s)", name, actor)
	case config.StepTypeClaude, config.StepTypeCodex, config.StepTypeExec:
		return fmt.Sprintf("%s(%s)", name, stepType)
	default:
		return fmt.Sprintf("%s(%s)", name, valueOr(stepType, "step"))
	}
}

func valueOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
