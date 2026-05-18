package view

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"vibedrive/internal/config"
	"vibedrive/internal/plan"
	"vibedrive/internal/runstate"
)

type RenderOptions struct {
	ActiveOnly bool
}

func Render(w io.Writer, file *plan.File, cfg *config.Config) error {
	return RenderWithOptions(w, file, cfg, RenderOptions{})
}

func RenderWithOptions(w io.Writer, file *plan.File, cfg *config.Config, opts RenderOptions) error {
	if file == nil {
		return fmt.Errorf("plan file is nil")
	}
	if w == nil {
		return fmt.Errorf("writer is nil")
	}

	activeTasks := loadActiveTasks(file, cfg)
	displayFile := fileWithActiveTasks(file, activeTasks)
	counts := countStatuses(displayFile.Tasks)
	total := len(displayFile.Tasks)

	if opts.ActiveOnly {
		renderActiveOnly(w, displayFile, cfg, activeTasks)
		return nil
	}

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
	fmt.Fprintf(w, "Next: %s\n", nextSummary(displayFile))
	if summary := activeSummary(displayFile.Tasks, activeTasks); summary != "" {
		fmt.Fprintf(w, "Active: %s\n", summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Legend: [x]=done [~]=in_progress [!]=blocked [?]=manual [ ]=todo")
	fmt.Fprintln(w, "Note: active step state is shown while vibedrive run is executing; otherwise step completion is inferred from the enclosing task status.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Task Graph:")

	graph := newTaskGraph(displayFile.Tasks)
	graph.render(w, cfg, activeTasks)
	fmt.Fprintf(w, "\n%d/%d tasks completed\n", counts[plan.StatusDone], total)
	return nil
}

func renderActiveOnly(w io.Writer, file *plan.File, cfg *config.Config, activeTasks map[string]runstate.Task) {
	counts := countStatuses(file.Tasks)
	fmt.Fprintf(w, "%d/%d tasks completed\n", counts[plan.StatusDone], len(file.Tasks))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Currently Executing:")
	if len(activeTasks) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}

	for _, item := range orderedActiveTasks(file.Tasks, activeTasks) {
		title := strings.TrimSpace(item.planTask.Title)
		if title == "" {
			title = "(task not in plan)"
		}
		fmt.Fprintf(w, "  [~] %s - %s\n", valueOr(item.active.ID, "(unknown)"), title)
		fmt.Fprintf(w, "      %s\n", activeStepSummary(cfg, item.planTask, item.active))
	}
}

func loadActiveTasks(file *plan.File, cfg *config.Config) map[string]runstate.Task {
	active := map[string]runstate.Task{}
	if file == nil || cfg == nil {
		return active
	}

	state, err := runstate.Load(runstate.Path(cfg.Workspace))
	if err != nil {
		return active
	}
	return runstate.ActiveTasksForPlan(state, file.Path)
}

func fileWithActiveTasks(file *plan.File, activeTasks map[string]runstate.Task) *plan.File {
	if file == nil || len(activeTasks) == 0 {
		return file
	}

	displayFile := *file
	displayFile.Tasks = append([]plan.Task(nil), file.Tasks...)
	for i := range displayFile.Tasks {
		if _, ok := activeTasks[displayFile.Tasks[i].ID]; ok {
			displayFile.Tasks[i].Status = plan.StatusInProgress
		}
	}
	return &displayFile
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
		return "all runnable tasks complete"
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

func activeSummary(tasks []plan.Task, activeTasks map[string]runstate.Task) string {
	if len(activeTasks) == 0 {
		return ""
	}

	parts := make([]string, 0, len(activeTasks))
	for _, item := range orderedActiveTasks(tasks, activeTasks) {
		parts = append(parts, formatActiveTask(item.active))
	}

	return strings.Join(parts, ", ")
}

type activeTaskView struct {
	active   runstate.Task
	planTask plan.Task
}

func orderedActiveTasks(tasks []plan.Task, activeTasks map[string]runstate.Task) []activeTaskView {
	items := make([]activeTaskView, 0, len(activeTasks))
	seen := make(map[string]bool, len(activeTasks))
	for _, task := range tasks {
		active, ok := activeTasks[task.ID]
		if !ok {
			continue
		}
		items = append(items, activeTaskView{
			active:   active,
			planTask: task,
		})
		seen[task.ID] = true
	}

	ids := make([]string, 0, len(activeTasks)-len(seen))
	for id := range activeTasks {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		items = append(items, activeTaskView{
			active: activeTasks[id],
		})
	}
	return items
}

func formatActiveTask(active runstate.Task) string {
	taskID := strings.TrimSpace(active.ID)
	if taskID == "" {
		taskID = "(unknown)"
	}
	stepName := strings.TrimSpace(active.StepName)
	if stepName == "" {
		return taskID
	}
	if active.StepTotal > 0 && active.StepIndex >= 0 {
		return fmt.Sprintf("%s step %d/%d %s", taskID, active.StepIndex+1, active.StepTotal, stepName)
	}
	return fmt.Sprintf("%s step %s", taskID, stepName)
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

func (g taskGraph) render(w io.Writer, cfg *config.Config, activeTasks map[string]runstate.Task) {
	if len(g.tasks) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}

	seen := make(map[string]bool, len(g.tasks))
	for i, task := range g.roots {
		g.renderTask(w, cfg, task, "", i == len(g.roots)-1, seen, nil, activeTasks)
	}
	for _, task := range g.tasks {
		if seen[task.ID] {
			continue
		}
		g.renderTask(w, cfg, task, "", true, seen, nil, activeTasks)
	}
}

func (g taskGraph) renderTask(w io.Writer, cfg *config.Config, task plan.Task, prefix string, last bool, seen map[string]bool, stack map[string]bool, activeTasks map[string]runstate.Task) {
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

	fmt.Fprintf(w, "%s    %s\n", childPrefix, stepSummary(cfg, task, activeTasks[task.ID]))

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
		g.renderTask(w, cfg, child, childPrefix, i == len(children)-1, seen, nextStack, activeTasks)
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

func stepSummary(cfg *config.Config, task plan.Task, active runstate.Task) string {
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
	for i, step := range steps {
		labels = append(labels, fmt.Sprintf("%s %s", stepStatusMarker(task, active, i), stepLabel(step)))
	}
	return fmt.Sprintf("steps (%s): %s", workflowName, strings.Join(labels, " -> "))
}

func activeStepSummary(cfg *config.Config, task plan.Task, active runstate.Task) string {
	label := activeStepLabel(cfg, task, active)
	if active.StepTotal > 0 && active.StepIndex >= 0 {
		return fmt.Sprintf("step %d/%d: %s", active.StepIndex+1, active.StepTotal, label)
	}
	return fmt.Sprintf("step: %s", label)
}

func activeStepLabel(cfg *config.Config, task plan.Task, active runstate.Task) string {
	if cfg != nil && strings.TrimSpace(task.ID) != "" {
		steps, _, err := stepsForTask(cfg, task)
		if err == nil && active.StepIndex >= 0 && active.StepIndex < len(steps) {
			return stepLabel(steps[active.StepIndex])
		}
	}

	name := strings.TrimSpace(active.StepName)
	if name == "" {
		name = "(unknown)"
	}
	stepType := strings.TrimSpace(strings.ToLower(active.StepType))
	actor := strings.TrimSpace(strings.ToLower(active.StepActor))
	if stepType == config.StepTypeAgent && actor != "" {
		return fmt.Sprintf("%s(agent:%s)", name, actor)
	}
	if stepType != "" {
		return fmt.Sprintf("%s(%s)", name, stepType)
	}
	return name
}

func stepStatusMarker(task plan.Task, active runstate.Task, stepIndex int) string {
	if active.ID == task.ID {
		switch {
		case stepIndex < active.StepIndex:
			return statusMarker(plan.StatusDone)
		case stepIndex == active.StepIndex:
			return statusMarker(plan.StatusInProgress)
		default:
			return statusMarker(plan.StatusTodo)
		}
	}
	return statusMarker(task.Status)
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
