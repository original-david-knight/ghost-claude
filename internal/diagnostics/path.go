package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

type pathSegments struct {
	run  string
	task string
	step string
}

func derivePathSegments(id Identity) (pathSegments, error) {
	run, err := safeSegment(id.RunID, "run id")
	if err != nil {
		return pathSegments{}, err
	}
	task, err := safeSegment(id.TaskID, "task id")
	if err != nil {
		return pathSegments{}, err
	}
	step, err := safeSegment(id.StepName, "step name")
	if err != nil {
		return pathSegments{}, err
	}
	return pathSegments{run: run, task: task, step: step}, nil
}

func safeSegment(raw, label string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s is required", label)
	}

	changed := false
	if filepath.IsAbs(raw) || strings.ContainsAny(raw, `/\`) {
		changed = true
	}

	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		allowed := r == '.' || r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
		if allowed && r < utf8RuneLimit {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		changed = true
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	segment := strings.Trim(b.String(), ".-")
	if segment == "" || segment == "." || segment == ".." {
		segment = "segment"
		changed = true
	}
	if segment != raw {
		changed = true
	}
	if changed {
		segment = segment + "-" + shortHash(raw)
	}
	if segment == "" || segment == "." || segment == ".." || filepath.IsAbs(segment) || strings.ContainsAny(segment, `/\`) {
		return "", fmt.Errorf("%s produced unsafe diagnostics path segment", label)
	}
	return segment, nil
}

const utf8RuneLimit = 0x80

func shortHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:10]
}
