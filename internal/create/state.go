package create

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const statePath = ".vibedrive/create-state.json"

type Stage string

const (
	StageProductDefinition Stage = "product_definition"
	StageFeatureRefactor   Stage = "feature_refactor"
	StageUXReview          Stage = "ux_review"
	StageTechnicalReview   Stage = "technical_review"
)

type State struct {
	LastStage Stage `json:"last_stage"`
}

type stateFile struct {
	LastStage Stage `json:"last_stage"`
}

func Path(workspace string) string {
	return filepath.Join(workspace, statePath)
}

func Read(workspace string) (State, error) {
	path := Path(workspace)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, err
	}

	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return State{}, fmt.Errorf("parse create state %s: %w", path, err)
	}

	return State{LastStage: file.LastStage}, nil
}

func Write(workspace string, state State) error {
	path := Path(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(stateFile{LastStage: state.LastStage}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return os.WriteFile(path, data, 0o644)
}
