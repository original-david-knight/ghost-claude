package create

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if err := Write(dir, State{LastStage: StageUXReview}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	state, err := Read(dir)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}

	if state.LastStage != StageUXReview {
		t.Fatalf("expected last stage %q, got %q", StageUXReview, state.LastStage)
	}
}

func TestReadMissingFileReturnsEmptyState(t *testing.T) {
	state, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}

	if state != (State{}) {
		t.Fatalf("expected empty state, got %#v", state)
	}
}

func TestWritePersistsOnlyLastStage(t *testing.T) {
	dir := t.TempDir()

	if err := Write(dir, State{LastStage: StageTechnicalReview}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected only last_stage in state JSON, got %v", got)
	}
	if got["last_stage"] != string(StageTechnicalReview) {
		t.Fatalf("expected last_stage %q, got %v", StageTechnicalReview, got["last_stage"])
	}
}

func TestPathUsesHiddenWorkspaceFile(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, ".vibedrive", "create-state.json")

	if got := Path(dir); got != want {
		t.Fatalf("expected path %q, got %q", want, got)
	}

	if err := Write(dir, State{LastStage: StageProductDefinition}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(want)); err != nil {
		t.Fatalf("expected state parent directory to exist: %v", err)
	}
}
