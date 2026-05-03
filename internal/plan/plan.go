package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	StatusTodo       = "todo"
	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusDone       = "done"
	StatusManual     = "manual"
)

const (
	ReadyBatchReasonUnmetDependencies        = "unmet_dependencies"
	ReadyBatchReasonLimitReached             = "limit_reached"
	ReadyBatchReasonExplicitConflict         = "explicit_conflict"
	ReadyBatchReasonMissingOwnershipMetadata = "missing_ownership_metadata"
	ReadyBatchReasonOwnershipConflict        = "ownership_conflict"
	ReadyBatchReasonContractWriterConflict   = "contract_writer_conflict"
)

var (
	ErrAllTasksDone = errors.New("all plan tasks are complete")
	ErrNoReadyTasks = errors.New("no ready plan tasks found")
)

type File struct {
	Path    string  `yaml:"-"`
	Project Project `yaml:"project"`
	Tasks   []Task  `yaml:"tasks"`
}

type Project struct {
	Name            string      `yaml:"name"`
	Objective       string      `yaml:"objective"`
	SourceDocs      StringList  `yaml:"source_docs,omitempty"`
	ConstraintFiles StringList  `yaml:"constraint_files,omitempty"`
	Components      []Component `yaml:"components,omitempty"`
}

type Component struct {
	ID                string     `yaml:"id"`
	Name              string     `yaml:"name,omitempty"`
	OwnedPaths        StringList `yaml:"owned_paths,omitempty"`
	ReadsContracts    StringList `yaml:"reads_contracts,omitempty"`
	ProvidesContracts StringList `yaml:"provides_contracts,omitempty"`
}

type Task struct {
	ID                string     `yaml:"id"`
	Title             string     `yaml:"title"`
	Details           string     `yaml:"details,omitempty"`
	Status            string     `yaml:"status"`
	Workflow          string     `yaml:"workflow,omitempty"`
	Kind              string     `yaml:"kind,omitempty"`
	Deps              StringList `yaml:"deps,omitempty"`
	ContextFiles      StringList `yaml:"context_files,omitempty"`
	Component         string     `yaml:"component,omitempty"`
	OwnsPaths         StringList `yaml:"owns_paths,omitempty"`
	ReadsContracts    StringList `yaml:"reads_contracts,omitempty"`
	ProvidesContracts StringList `yaml:"provides_contracts,omitempty"`
	ConflictsWith     StringList `yaml:"conflicts_with,omitempty"`
	Acceptance        StringList `yaml:"acceptance,omitempty"`
	VerifyCommands    StringList `yaml:"verify_commands,omitempty"`
	CommitMessage     string     `yaml:"commit_message,omitempty"`
	Notes             string     `yaml:"notes,omitempty"`
}

type ReadyBatchAnalysis struct {
	Limit       int
	Selected    []Task
	NotSelected []ReadyBatchExclusion
}

type ReadyBatchExclusion struct {
	Task              Task
	Reason            string
	ConflictsWith     string
	UnmetDependencies []string
	Detail            string
}

func Load(path string) (*File, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	file.Path = absPath

	for i := range file.Tasks {
		if strings.TrimSpace(file.Tasks[i].Status) == "" {
			file.Tasks[i].Status = StatusTodo
		}
		file.Tasks[i].Status = normalizeStatus(file.Tasks[i].Status)
	}

	if err := file.Validate(); err != nil {
		return nil, err
	}

	return &file, nil
}

func (f *File) Save() error {
	if f == nil {
		return fmt.Errorf("plan file is nil")
	}
	if strings.TrimSpace(f.Path) == "" {
		return fmt.Errorf("plan file path is required")
	}
	if err := f.Validate(); err != nil {
		return err
	}

	fileForSave := *f
	fileForSave.Tasks = append([]Task(nil), f.Tasks...)
	for i := range fileForSave.Tasks {
		fileForSave.Tasks[i].Notes = ""
	}

	data, err := yaml.Marshal(&fileForSave)
	if err != nil {
		return err
	}
	return os.WriteFile(f.Path, data, 0o644)
}

func (f *File) Validate() error {
	if len(f.Tasks) == 0 {
		return fmt.Errorf("plan must contain at least one task")
	}

	components, err := validateComponents(f.Project.Components)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(f.Tasks))
	for i, task := range f.Tasks {
		id := strings.TrimSpace(task.ID)
		if id == "" {
			return fmt.Errorf("tasks[%d].id is required", i)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("duplicate task id %q", id)
		}
		seen[id] = struct{}{}

		if strings.TrimSpace(task.Title) == "" {
			return fmt.Errorf("tasks[%d].title is required", i)
		}

		switch normalizeStatus(task.Status) {
		case StatusTodo, StatusInProgress, StatusBlocked, StatusDone, StatusManual:
		default:
			return fmt.Errorf("tasks[%d].status %q is not supported", i, task.Status)
		}

		componentID := strings.TrimSpace(task.Component)
		if componentID != "" && len(components) > 0 {
			if _, ok := components[componentID]; !ok {
				return fmt.Errorf("tasks[%d].component references unknown component %q", i, task.Component)
			}
		}

		if err := validatePathMetadata(fmt.Sprintf("tasks[%d].owns_paths", i), task.OwnsPaths); err != nil {
			return err
		}
		if err := validatePathMetadata(fmt.Sprintf("tasks[%d].reads_contracts", i), task.ReadsContracts); err != nil {
			return err
		}
		if err := validatePathMetadata(fmt.Sprintf("tasks[%d].provides_contracts", i), task.ProvidesContracts); err != nil {
			return err
		}
	}

	for i, task := range f.Tasks {
		taskID := strings.TrimSpace(task.ID)
		for _, dep := range task.Deps {
			if _, ok := seen[dep]; !ok {
				return fmt.Errorf("tasks[%d].deps references unknown task %q", i, dep)
			}
		}
		for _, conflict := range task.ConflictsWith {
			conflictID := strings.TrimSpace(conflict)
			if conflictID == "" {
				return fmt.Errorf("tasks[%d].conflicts_with contains an empty task id", i)
			}
			if conflictID == taskID {
				return fmt.Errorf("tasks[%d].conflicts_with references itself", i)
			}
			if _, ok := seen[conflictID]; !ok {
				return fmt.Errorf("tasks[%d].conflicts_with references unknown task %q", i, conflict)
			}
		}
	}

	return nil
}

func validateComponents(components []Component) (map[string]struct{}, error) {
	seen := make(map[string]struct{}, len(components))
	for i, component := range components {
		id := strings.TrimSpace(component.ID)
		if id == "" {
			return nil, fmt.Errorf("project.components[%d].id is required", i)
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate component id %q", id)
		}
		seen[id] = struct{}{}

		if err := validatePathMetadata(fmt.Sprintf("project.components[%d].owned_paths", i), component.OwnedPaths); err != nil {
			return nil, err
		}
		if err := validatePathMetadata(fmt.Sprintf("project.components[%d].reads_contracts", i), component.ReadsContracts); err != nil {
			return nil, err
		}
		if err := validatePathMetadata(fmt.Sprintf("project.components[%d].provides_contracts", i), component.ProvidesContracts); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

func validatePathMetadata(field string, values StringList) error {
	for i, value := range values {
		item := strings.TrimSpace(value)
		if item == "" {
			return fmt.Errorf("%s[%d] is required", field, i)
		}
		if filepath.IsAbs(item) || strings.HasPrefix(item, "/") {
			return fmt.Errorf("%s[%d] must be repo-relative, got %q", field, i, value)
		}
		if hasParentPathSegment(item) {
			return fmt.Errorf("%s[%d] must not contain '..', got %q", field, i, value)
		}
	}
	return nil
}

func hasParentPathSegment(value string) bool {
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}

func (f *File) FindNextReady() (Task, error) {
	if candidate, ok := f.findReadyTaskByStatus(StatusInProgress); ok {
		return candidate, nil
	}
	if candidate, ok := f.findReadyTaskByStatus(StatusTodo); ok {
		return candidate, nil
	}

	if f.hasUnfinishedTasks() {
		return Task{}, ErrNoReadyTasks
	}
	return Task{}, ErrAllTasksDone
}

func (f *File) SelectReadyBatch(limit int) ([]Task, error) {
	analysis, err := f.AnalyzeReadyBatch(limit)
	if err != nil {
		return nil, err
	}
	return append([]Task(nil), analysis.Selected...), nil
}

func (f *File) AnalyzeReadyBatch(limit int) (ReadyBatchAnalysis, error) {
	if f == nil {
		return ReadyBatchAnalysis{}, fmt.Errorf("plan file is nil")
	}
	if limit < 1 {
		return ReadyBatchAnalysis{}, fmt.Errorf("ready batch limit must be >= 1")
	}

	analysis := ReadyBatchAnalysis{Limit: limit}
	state := newReadyBatchState(f)

	for _, status := range []string{StatusInProgress, StatusTodo} {
		for _, task := range f.Tasks {
			if normalizeStatus(task.Status) != status {
				continue
			}

			unmetDeps := f.unmetDependencyIDs(task)
			if len(unmetDeps) > 0 {
				analysis.NotSelected = append(analysis.NotSelected, ReadyBatchExclusion{
					Task:              task,
					Reason:            ReadyBatchReasonUnmetDependencies,
					UnmetDependencies: unmetDeps,
					Detail:            fmt.Sprintf("waiting for dependencies: %s", strings.Join(unmetDeps, ", ")),
				})
				continue
			}

			if len(analysis.Selected) >= limit {
				analysis.NotSelected = append(analysis.NotSelected, ReadyBatchExclusion{
					Task:   task,
					Reason: ReadyBatchReasonLimitReached,
					Detail: fmt.Sprintf("ready batch limit %d reached", limit),
				})
				continue
			}

			if exclusion, ok := state.exclusion(task, analysis.Selected); ok {
				analysis.NotSelected = append(analysis.NotSelected, exclusion)
				continue
			}

			analysis.Selected = append(analysis.Selected, task)
		}
	}

	return analysis, nil
}

func (f *File) FindTask(id string) (Task, bool) {
	for _, task := range f.Tasks {
		if task.ID == id {
			return task, true
		}
	}
	return Task{}, false
}

func (t Task) ProgressSignature() string {
	return fmt.Sprintf("%s:%s", t.ID, normalizeStatus(t.Status))
}

func (t Task) IsTerminal() bool {
	switch normalizeStatus(t.Status) {
	case StatusDone, StatusBlocked, StatusManual:
		return true
	default:
		return false
	}
}

func (f *File) UnfinishedTasks() []Task {
	var tasks []Task
	for _, task := range f.Tasks {
		if normalizeStatus(task.Status) != StatusDone {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func (f *File) findReadyTaskByStatus(status string) (Task, bool) {
	for _, task := range f.Tasks {
		if normalizeStatus(task.Status) != status {
			continue
		}
		if f.depsDone(task) {
			return task, true
		}
	}
	return Task{}, false
}

func (f *File) hasUnfinishedTasks() bool {
	for _, task := range f.Tasks {
		if normalizeStatus(task.Status) != StatusDone {
			return true
		}
	}
	return false
}

func (f *File) depsDone(task Task) bool {
	return len(f.unmetDependencyIDs(task)) == 0
}

func (f *File) unmetDependencyIDs(task Task) []string {
	var unmet []string
	for _, depID := range task.Deps {
		dep, ok := f.FindTask(depID)
		if !ok || normalizeStatus(dep.Status) != StatusDone {
			unmet = append(unmet, depID)
		}
	}
	return unmet
}

func normalizeStatus(status string) string {
	return strings.TrimSpace(strings.ToLower(status))
}

type readyBatchState struct {
	components map[string]Component
}

type taskBoundaryMetadata struct {
	ownsPaths         []string
	providesContracts []string
	hasOwnership      bool
}

func newReadyBatchState(file *File) readyBatchState {
	components := make(map[string]Component, len(file.Project.Components))
	for _, component := range file.Project.Components {
		id := strings.TrimSpace(component.ID)
		if id != "" {
			components[id] = component
		}
	}
	return readyBatchState{components: components}
}

func (s readyBatchState) exclusion(task Task, selected []Task) (ReadyBatchExclusion, bool) {
	for _, other := range selected {
		if conflictsExplicitly(task, other) {
			return ReadyBatchExclusion{
				Task:          task,
				Reason:        ReadyBatchReasonExplicitConflict,
				ConflictsWith: other.ID,
				Detail:        fmt.Sprintf("explicit conflict with %s", other.ID),
			}, true
		}
	}

	taskMetadata := s.metadataFor(task)
	if len(selected) > 0 && !taskMetadata.hasOwnership {
		return ReadyBatchExclusion{
			Task:          task,
			Reason:        ReadyBatchReasonMissingOwnershipMetadata,
			ConflictsWith: selected[0].ID,
			Detail:        "task has no owns_paths or component owned_paths",
		}, true
	}

	for _, other := range selected {
		otherMetadata := s.metadataFor(other)
		if !otherMetadata.hasOwnership {
			return ReadyBatchExclusion{
				Task:          task,
				Reason:        ReadyBatchReasonMissingOwnershipMetadata,
				ConflictsWith: other.ID,
				Detail:        fmt.Sprintf("selected task %s has no owns_paths or component owned_paths", other.ID),
			}, true
		}
	}

	for _, other := range selected {
		otherMetadata := s.metadataFor(other)
		if left, right, ok := firstOverlappingPattern(taskMetadata.ownsPaths, otherMetadata.ownsPaths); ok {
			return ReadyBatchExclusion{
				Task:          task,
				Reason:        ReadyBatchReasonOwnershipConflict,
				ConflictsWith: other.ID,
				Detail:        fmt.Sprintf("owns_paths overlap: %s and %s", left, right),
			}, true
		}
		if left, right, ok := firstOverlappingPattern(taskMetadata.providesContracts, otherMetadata.providesContracts); ok {
			return ReadyBatchExclusion{
				Task:          task,
				Reason:        ReadyBatchReasonContractWriterConflict,
				ConflictsWith: other.ID,
				Detail:        fmt.Sprintf("provides_contracts overlap: %s and %s", left, right),
			}, true
		}
	}

	return ReadyBatchExclusion{}, false
}

func (s readyBatchState) metadataFor(task Task) taskBoundaryMetadata {
	ownsPaths := normalizedMetadataList(task.OwnsPaths)
	if len(ownsPaths) == 0 {
		if component, ok := s.components[strings.TrimSpace(task.Component)]; ok {
			ownsPaths = normalizedMetadataList(component.OwnedPaths)
		}
	}

	return taskBoundaryMetadata{
		ownsPaths:         ownsPaths,
		providesContracts: normalizedMetadataList(task.ProvidesContracts),
		hasOwnership:      len(ownsPaths) > 0,
	}
}

func conflictsExplicitly(left, right Task) bool {
	return stringListContains(left.ConflictsWith, right.ID) || stringListContains(right.ConflictsWith, left.ID)
}

func stringListContains(values StringList, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func normalizedMetadataList(values StringList) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		item := normalizePathPattern(value)
		if item != "" {
			normalized = append(normalized, item)
		}
	}
	return normalized
}

func firstOverlappingPattern(left, right []string) (string, string, bool) {
	for _, leftPattern := range left {
		for _, rightPattern := range right {
			if pathPatternsOverlap(leftPattern, rightPattern) {
				return leftPattern, rightPattern, true
			}
		}
	}
	return "", "", false
}

func pathPatternsOverlap(left, right string) bool {
	left = normalizePathPattern(left)
	right = normalizePathPattern(right)
	if left == "" || right == "" {
		return false
	}
	if left == right || isUniversalPathPattern(left) || isUniversalPathPattern(right) {
		return true
	}
	if !hasPathGlob(left) && !hasPathGlob(right) {
		return pathContainsOrEquals(left, right) || pathContainsOrEquals(right, left)
	}

	leftPrefix := literalPathPrefix(left)
	rightPrefix := literalPathPrefix(right)
	if leftPrefix == "" || rightPrefix == "" {
		return true
	}
	return strings.HasPrefix(leftPrefix, rightPrefix) || strings.HasPrefix(rightPrefix, leftPrefix)
}

func normalizePathPattern(value string) string {
	item := filepath.ToSlash(strings.TrimSpace(value))
	item = strings.TrimPrefix(item, "./")
	if item == "" {
		return ""
	}
	item = filepath.ToSlash(filepath.Clean(item))
	if item == "." {
		return ""
	}
	return item
}

func isUniversalPathPattern(value string) bool {
	return value == "*" || value == "**" || value == "**/*"
}

func hasPathGlob(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func literalPathPrefix(pattern string) string {
	index := strings.IndexAny(pattern, "*?[")
	if index < 0 {
		return pattern
	}
	return pattern[:index]
}

func pathContainsOrEquals(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}
