package plan

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadDefaultsEmptyStatusToTodo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: first
    title: First task
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := file.Tasks[0].Status; got != StatusTodo {
		t.Fatalf("expected default status %q, got %q", StatusTodo, got)
	}
}

func TestLoadSupportsComponentAndContractMetadata(t *testing.T) {
	path := writePlanFile(t, `project:
  name: demo
  components:
    - id: plan
      name: Plan model
      owned_paths:
        - internal/plan/**
      reads_contracts:
        - README.md
      provides_contracts:
        - internal/plan/plan.go
    - id: docs
      owned_paths:
        - README.md
tasks:
  - id: schema
    title: Add schema metadata
    status: todo
    component: plan
    owns_paths:
      - internal/plan/**
    reads_contracts:
      - README.md
    provides_contracts:
      - internal/plan/plan.go
    conflicts_with:
      - docs
  - id: docs
    title: Document schema metadata
    status: todo
`)

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := len(file.Project.Components); got != 2 {
		t.Fatalf("expected 2 components, got %d", got)
	}
	if got := file.Project.Components[0].OwnedPaths[0]; got != "internal/plan/**" {
		t.Fatalf("expected component owned path to load, got %q", got)
	}
	if got := file.Project.Components[0].ReadsContracts[0]; got != "README.md" {
		t.Fatalf("expected component read contract to load, got %q", got)
	}
	if got := file.Tasks[0].Component; got != "plan" {
		t.Fatalf("expected task component %q, got %q", "plan", got)
	}
	if got := file.Tasks[0].OwnsPaths[0]; got != "internal/plan/**" {
		t.Fatalf("expected task owned path to load, got %q", got)
	}
	if got := file.Tasks[0].ProvidesContracts[0]; got != "internal/plan/plan.go" {
		t.Fatalf("expected task provided contract to load, got %q", got)
	}
	if got := file.Tasks[0].ConflictsWith[0]; got != "docs" {
		t.Fatalf("expected task conflict to load, got %q", got)
	}

	ready, err := file.FindNextReady()
	if err != nil {
		t.Fatalf("FindNextReady returned error: %v", err)
	}
	if ready.ID != "schema" {
		t.Fatalf("expected serial ready selection to ignore metadata, got %q", ready.ID)
	}

	if err := file.Save(); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if got := reloaded.Tasks[0].ReadsContracts[0]; got != "README.md" {
		t.Fatalf("expected task contract metadata to survive save, got %q", got)
	}
}

func TestLoadAllowsOmittedComponentCatalogAndUnknownYAMLFields(t *testing.T) {
	path := writePlanFile(t, `project:
  name: demo
  future_metadata:
    generated_by: newer-vibedrive
tasks:
  - id: first
    title: First task
    component: loose-component
    owns_paths:
      - internal/plan/**
    future_task_metadata: ignored
`)

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := file.Tasks[0].Status; got != StatusTodo {
		t.Fatalf("expected default status %q, got %q", StatusTodo, got)
	}
	if got := file.Tasks[0].Component; got != "loose-component" {
		t.Fatalf("expected task-only component metadata to load, got %q", got)
	}
}

func TestLoadRejectsDuplicateComponentIDs(t *testing.T) {
	path := writePlanFile(t, `project:
  name: demo
  components:
    - id: plan
    - id: plan
tasks:
  - id: first
    title: First task
    status: todo
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected Load to reject duplicate component IDs")
	}
	if !strings.Contains(err.Error(), `duplicate component id "plan"`) {
		t.Fatalf("expected duplicate component ID error, got %v", err)
	}
}

func TestValidateRejectsUnknownTaskDependency(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusTodo, Deps: StringList{"missing"}},
		},
	}

	err := file.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject an unknown dependency")
	}
	if !strings.Contains(err.Error(), `deps references unknown task "missing"`) {
		t.Fatalf("expected unknown dependency error, got %v", err)
	}
}

func TestValidateRejectsMalformedOwnershipMetadata(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusTodo, OwnsPaths: StringList{"../outside"}},
		},
	}

	err := file.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject parent path ownership metadata")
	}
	if !strings.Contains(err.Error(), "tasks[0].owns_paths") {
		t.Fatalf("expected owns_paths validation error, got %v", err)
	}
}

func TestExamplePlanLoadsValidatesAndSaves(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "vibedrive-plan.example.yaml"))
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}

	path := writePlanFile(t, string(content))
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := len(file.Project.Components); got < 4 {
		t.Fatalf("expected example plan to document component boundaries, got %d components", got)
	}
	service, ok := file.FindTask("implement-service-contract")
	if !ok {
		t.Fatal("expected example plan to include service implementation task")
	}
	if service.Component != "service" {
		t.Fatalf("expected service task component metadata, got %q", service.Component)
	}
	if got, want := []string(service.OwnsPaths), []string{"internal/service/**"}; !slices.Equal(got, want) {
		t.Fatalf("expected service owned paths %v, got %v", want, got)
	}
	if got, want := []string(service.ReadsContracts), []string{"internal/contracts/public-api.md"}; !slices.Equal(got, want) {
		t.Fatalf("expected service contract reads %v, got %v", want, got)
	}
	checkpoint, ok := file.FindTask("integration-checkpoint")
	if !ok {
		t.Fatal("expected example plan to include an integration checkpoint")
	}
	if checkpoint.Workflow != "checkpoint" {
		t.Fatalf("expected integration checkpoint workflow, got %q", checkpoint.Workflow)
	}
	if got, want := []string(checkpoint.Deps), []string{"implement-service-contract", "implement-web-contract"}; !slices.Equal(got, want) {
		t.Fatalf("expected integration checkpoint deps %v, got %v", want, got)
	}
	for i := range file.Tasks {
		if file.Tasks[i].ID == "define-public-contract" {
			file.Tasks[i].Status = StatusDone
		}
	}
	analysis, err := file.AnalyzeReadyBatch(3)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}
	if got, want := taskIDs(analysis.Selected), []string{"implement-service-contract", "implement-web-contract"}; !slices.Equal(got, want) {
		t.Fatalf("expected example plan to expose a safe parallel batch %v, got %v", want, got)
	}
	exclusion := requireReadyBatchExclusion(t, analysis, "integration-checkpoint")
	if exclusion.Reason != ReadyBatchReasonUnmetDependencies {
		t.Fatalf("expected checkpoint to wait for integrated work, got %#v", exclusion)
	}
	if err := file.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if err := file.Save(); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
}

func TestFindNextReadyPrefersInProgressTask(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusTodo},
			{ID: "second", Title: "second", Status: StatusInProgress},
		},
	}

	task, err := file.FindNextReady()
	if err != nil {
		t.Fatalf("FindNextReady returned error: %v", err)
	}

	if task.ID != "second" {
		t.Fatalf("expected in-progress task, got %q", task.ID)
	}
}

func TestFindNextReadyHonorsDependencies(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusTodo},
			{ID: "second", Title: "second", Status: StatusTodo, Deps: StringList{"first"}},
		},
	}

	task, err := file.FindNextReady()
	if err != nil {
		t.Fatalf("FindNextReady returned error: %v", err)
	}

	if task.ID != "first" {
		t.Fatalf("expected dependency-free task, got %q", task.ID)
	}
}

func TestFindNextReadyReturnsNoReadyWhenBlockedByDeps(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "first", Title: "first", Status: StatusBlocked},
			{ID: "second", Title: "second", Status: StatusTodo, Deps: StringList{"first"}},
		},
	}

	_, err := file.FindNextReady()
	if !errors.Is(err, ErrNoReadyTasks) {
		t.Fatalf("expected ErrNoReadyTasks, got %v", err)
	}
}

func TestAnalyzeReadyBatchSelectsIndependentTasksUpToLimit(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "api", Title: "API", Status: StatusTodo, OwnsPaths: StringList{"internal/api/**"}},
			{ID: "ui", Title: "UI", Status: StatusTodo, OwnsPaths: StringList{"web/ui/**"}},
			{ID: "docs", Title: "Docs", Status: StatusTodo, OwnsPaths: StringList{"docs/**"}},
		},
	}

	analysis, err := file.AnalyzeReadyBatch(2)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(analysis.Selected), []string{"api", "ui"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
	exclusion := requireReadyBatchExclusion(t, analysis, "docs")
	if exclusion.Reason != ReadyBatchReasonLimitReached {
		t.Fatalf("expected docs reason %q, got %#v", ReadyBatchReasonLimitReached, exclusion)
	}
}

func TestSelectReadyBatchReturnsSelectedTasks(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "api", Title: "API", Status: StatusTodo, OwnsPaths: StringList{"internal/api/**"}},
			{ID: "ui", Title: "UI", Status: StatusTodo, OwnsPaths: StringList{"web/ui/**"}},
		},
	}

	selected, err := file.SelectReadyBatch(4)
	if err != nil {
		t.Fatalf("SelectReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(selected), []string{"api", "ui"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
}

func TestAnalyzeReadyBatchExplainsDependencyBlockedTasks(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "setup", Title: "Setup", Status: StatusTodo, OwnsPaths: StringList{"internal/setup/**"}},
			{ID: "feature", Title: "Feature", Status: StatusTodo, Deps: StringList{"setup"}, OwnsPaths: StringList{"internal/feature/**"}},
		},
	}

	analysis, err := file.AnalyzeReadyBatch(2)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(analysis.Selected), []string{"setup"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
	exclusion := requireReadyBatchExclusion(t, analysis, "feature")
	if exclusion.Reason != ReadyBatchReasonUnmetDependencies {
		t.Fatalf("expected feature reason %q, got %#v", ReadyBatchReasonUnmetDependencies, exclusion)
	}
	if got, want := exclusion.UnmetDependencies, []string{"setup"}; !slices.Equal(got, want) {
		t.Fatalf("expected unmet dependencies %v, got %v", want, got)
	}
}

func TestAnalyzeReadyBatchRejectsOverlappingOwnership(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "api", Title: "API", Status: StatusTodo, OwnsPaths: StringList{"internal/api/**"}},
			{ID: "handler", Title: "Handler", Status: StatusTodo, OwnsPaths: StringList{"internal/api/handler.go"}},
		},
	}

	analysis, err := file.AnalyzeReadyBatch(2)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(analysis.Selected), []string{"api"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
	exclusion := requireReadyBatchExclusion(t, analysis, "handler")
	if exclusion.Reason != ReadyBatchReasonOwnershipConflict || exclusion.ConflictsWith != "api" {
		t.Fatalf("expected ownership conflict with api, got %#v", exclusion)
	}
}

func TestAnalyzeReadyBatchRejectsExplicitConflicts(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "api", Title: "API", Status: StatusTodo, OwnsPaths: StringList{"internal/api/**"}, ConflictsWith: StringList{"docs"}},
			{ID: "docs", Title: "Docs", Status: StatusTodo, OwnsPaths: StringList{"docs/**"}},
		},
	}

	analysis, err := file.AnalyzeReadyBatch(2)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(analysis.Selected), []string{"api"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
	exclusion := requireReadyBatchExclusion(t, analysis, "docs")
	if exclusion.Reason != ReadyBatchReasonExplicitConflict || exclusion.ConflictsWith != "api" {
		t.Fatalf("expected explicit conflict with api, got %#v", exclusion)
	}
}

func TestAnalyzeReadyBatchRejectsSharedContractWriters(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "server", Title: "Server", Status: StatusTodo, OwnsPaths: StringList{"internal/server/**"}, ProvidesContracts: StringList{"docs/api.md"}},
			{ID: "client", Title: "Client", Status: StatusTodo, OwnsPaths: StringList{"internal/client/**"}, ProvidesContracts: StringList{"docs/api.md"}},
		},
	}

	analysis, err := file.AnalyzeReadyBatch(2)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(analysis.Selected), []string{"server"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
	exclusion := requireReadyBatchExclusion(t, analysis, "client")
	if exclusion.Reason != ReadyBatchReasonContractWriterConflict || exclusion.ConflictsWith != "server" {
		t.Fatalf("expected contract writer conflict with server, got %#v", exclusion)
	}
}

func TestAnalyzeReadyBatchKeepsMissingOwnershipMetadataSerial(t *testing.T) {
	file := &File{
		Tasks: []Task{
			{ID: "legacy", Title: "Legacy", Status: StatusTodo},
			{ID: "bounded", Title: "Bounded", Status: StatusTodo, OwnsPaths: StringList{"internal/bounded/**"}},
		},
	}

	analysis, err := file.AnalyzeReadyBatch(2)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(analysis.Selected), []string{"legacy"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
	exclusion := requireReadyBatchExclusion(t, analysis, "bounded")
	if exclusion.Reason != ReadyBatchReasonMissingOwnershipMetadata || exclusion.ConflictsWith != "legacy" {
		t.Fatalf("expected missing metadata conflict with legacy, got %#v", exclusion)
	}
}

func TestAnalyzeReadyBatchUsesComponentOwnershipFallback(t *testing.T) {
	file := &File{
		Project: Project{
			Components: []Component{
				{ID: "api", OwnedPaths: StringList{"internal/api/**"}},
				{ID: "docs", OwnedPaths: StringList{"docs/**"}},
			},
		},
		Tasks: []Task{
			{ID: "api-task", Title: "API", Status: StatusTodo, Component: "api"},
			{ID: "docs-task", Title: "Docs", Status: StatusTodo, Component: "docs"},
		},
	}

	analysis, err := file.AnalyzeReadyBatch(2)
	if err != nil {
		t.Fatalf("AnalyzeReadyBatch returned error: %v", err)
	}

	if got, want := taskIDs(analysis.Selected), []string{"api-task", "docs-task"}; !slices.Equal(got, want) {
		t.Fatalf("expected selected tasks %v, got %v", want, got)
	}
}

func TestSavePersistsUpdatedStatusWithoutTaskNotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	file := &File{
		Path:    path,
		Project: Project{Name: "demo"},
		Tasks: []Task{
			{ID: "first", Title: "First task", Status: StatusDone, Notes: "finished"},
		},
	}

	if err := file.Save(); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := loaded.Tasks[0].Status; got != StatusDone {
		t.Fatalf("expected status %q, got %q", StatusDone, got)
	}
	if got := loaded.Tasks[0].Notes; got != "" {
		t.Fatalf("expected notes to stay out of the plan file, got %q", got)
	}
}

func TestLoadReadsLegacyTaskNotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: first
    title: First task
    status: done
    notes: finished
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := loaded.Tasks[0].Notes; got != "finished" {
		t.Fatalf("expected legacy notes to load, got %q", got)
	}
}

func TestLoadFlattensColonPrefixedAcceptanceItem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: demo-task
    title: Demo task
    status: todo
    acceptance:
      - demo.mp4 exists
      - Recording review: no tile pops, smooth descent, recognizable imagery throughout
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := "Recording review: no tile pops, smooth descent, recognizable imagery throughout"
	if got := file.Tasks[0].Acceptance[1]; got != want {
		t.Fatalf("expected acceptance item %q, got %q", want, got)
	}
}

func taskIDs(tasks []Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func requireReadyBatchExclusion(t *testing.T, analysis ReadyBatchAnalysis, taskID string) ReadyBatchExclusion {
	t.Helper()
	for _, exclusion := range analysis.NotSelected {
		if exclusion.Task.ID == taskID {
			return exclusion
		}
	}
	t.Fatalf("expected exclusion for task %q, got %#v", taskID, analysis.NotSelected)
	return ReadyBatchExclusion{}
}

func writePlanFile(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "vibedrive-plan.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	return path
}
