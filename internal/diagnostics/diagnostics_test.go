package diagnostics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCaptureExecWritesContractLayout(t *testing.T) {
	workspace := t.TempDir()
	now := time.Date(2026, 5, 7, 16, 0, 0, 0, time.UTC)
	capturer := NewWithOptions(Options{
		Workspace: workspace,
		Now: func() time.Time {
			return now
		},
	})
	exitCode := 7

	result, err := capturer.CaptureExec(ExecCapture{
		Identity: Identity{
			RunID:    "run/one",
			TaskID:   "task alpha",
			StepName: "..",
		},
		Failure: Failure{
			Path:    "exec_step_non_zero_exit",
			Message: "exit status 7",
		},
		Command: ExecCommand{
			Argv:       []string{"sh", "-c", "echo fail"},
			WorkingDir: workspace,
			ExitCode:   &exitCode,
			Env: ExecEnvironment{
				Step: map[string]string{
					"API_TOKEN": "secret",
					"MODE":      "test",
				},
				InheritedKeys: []string{"PATH", "HOME"},
			},
		},
		Stdout:       Bytes([]byte("child stdout\n")),
		Stderr:       Bytes([]byte("child stderr\n")),
		Combined:     Bytes([]byte("child stdout\nchild stderr\n")),
		ParentStdout: Bytes([]byte("parent stdout\n")),
		ParentStderr: Bytes([]byte("parent stderr\n")),
	})
	if err != nil {
		t.Fatalf("CaptureExec returned error: %v", err)
	}

	if !strings.HasPrefix(result.Dir, filepath.Join(workspace, ".vibedrive", "debug")) {
		t.Fatalf("diagnostics dir %q is outside debug root", result.Dir)
	}
	if strings.Contains(result.Dir, "..") {
		t.Fatalf("diagnostics dir contains unsafe segment: %q", result.Dir)
	}

	for _, rel := range []string{
		"manifest.json",
		"parent/stdout-tail.txt",
		"parent/stderr-tail.txt",
		"exec/command.json",
		"exec/stdout-tail.txt",
		"exec/stderr-tail.txt",
		"exec/combined-tail.txt",
	} {
		if _, err := os.Stat(filepath.Join(result.Dir, rel)); err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(result.Dir, "prompt", "raw.bin")); !os.IsNotExist(err) {
		t.Fatalf("plain exec prompt artifact should not be written, stat error: %v", err)
	}

	manifest := readManifest(t, result.ManifestPath)
	if manifest.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", manifest.SchemaVersion, SchemaVersion)
	}
	if manifest.RunID != "run/one" || manifest.TaskID != "task alpha" || manifest.StepName != ".." {
		t.Fatalf("manifest did not preserve raw identity: %#v", manifest)
	}
	if manifest.PathSegments.Run == "run/one" || manifest.PathSegments.Step == ".." {
		t.Fatalf("manifest used unsafe path segments: %#v", manifest.PathSegments)
	}
	if manifest.Transport.Kind != "exec" || manifest.Transport.Interactive {
		t.Fatalf("unexpected transport: %#v", manifest.Transport)
	}
	if entry := artifactEntry(t, manifest, ArtifactPromptRaw); entry.Status != ArtifactStatusNotApplicable {
		t.Fatalf("prompt raw status = %q, want not_applicable", entry.Status)
	}
	if entry := artifactEntry(t, manifest, ArtifactExecCombined); entry.Status != ArtifactStatusWritten || entry.Bytes == nil || *entry.Bytes == 0 {
		t.Fatalf("exec combined entry not written with bytes: %#v", entry)
	}
	if entry := artifactEntry(t, manifest, ArtifactManifest); entry.Status != ArtifactStatusWritten || entry.Bytes == nil || *entry.Bytes == 0 {
		t.Fatalf("manifest entry not written with bytes: %#v", entry)
	}

	var command ExecCommand
	readJSONFile(t, filepath.Join(result.Dir, "exec", "command.json"), &command)
	if got := command.Env.Step["API_TOKEN"]; got != "[REDACTED]" {
		t.Fatalf("API_TOKEN = %q, want redacted", got)
	}
	if got := command.Env.Step["MODE"]; got != "test" {
		t.Fatalf("MODE = %q, want test", got)
	}
	if len(command.Env.InheritedKeys) != 2 || command.Env.InheritedKeys[0] != "HOME" || command.Env.InheritedKeys[1] != "PATH" {
		t.Fatalf("inherited keys were not sorted/bounded metadata: %#v", command.Env.InheritedKeys)
	}
}

func TestCaptureExecBoundsOutputTails(t *testing.T) {
	workspace := t.TempDir()
	capturer := New(workspace)
	stdout := append([]byte("BEGIN"), bytes.Repeat([]byte("x"), ExecOutputLimit+128)...)
	stdout = append(stdout, []byte("TAIL")...)
	parent := append([]byte("PARENT-BEGIN"), bytes.Repeat([]byte("p"), ParentOutputLimit+64)...)
	parent = append(parent, []byte("PARENT-TAIL")...)

	result, err := capturer.CaptureExec(ExecCapture{
		Identity:     Identity{RunID: "run", TaskID: "task", StepName: "exec"},
		Failure:      Failure{Path: "exec_step_non_zero_exit", Message: "failed"},
		Command:      ExecCommand{Argv: []string{"false"}},
		Stdout:       Bytes(stdout),
		Stderr:       Bytes([]byte("stderr")),
		Combined:     Bytes(stdout),
		ParentStdout: Bytes(parent),
		ParentStderr: Bytes([]byte("parent stderr")),
	})
	if err != nil {
		t.Fatalf("CaptureExec returned error: %v", err)
	}

	got := readFile(t, filepath.Join(result.Dir, "exec", "stdout-tail.txt"))
	if len(got) != ExecOutputLimit {
		t.Fatalf("stdout tail length = %d, want %d", len(got), ExecOutputLimit)
	}
	if bytes.Contains(got, []byte("BEGIN")) {
		t.Fatalf("stdout tail preserved prefix instead of tail")
	}
	if !bytes.HasSuffix(got, []byte("TAIL")) {
		t.Fatalf("stdout tail does not end with suffix")
	}

	parentGot := readFile(t, filepath.Join(result.Dir, "parent", "stdout-tail.txt"))
	if len(parentGot) != ParentOutputLimit {
		t.Fatalf("parent tail length = %d, want %d", len(parentGot), ParentOutputLimit)
	}
	if bytes.Contains(parentGot, []byte("PARENT-BEGIN")) || !bytes.HasSuffix(parentGot, []byte("PARENT-TAIL")) {
		t.Fatalf("parent output was not tail truncated")
	}

	manifest := readManifest(t, result.ManifestPath)
	entry := artifactEntry(t, manifest, ArtifactExecStdout)
	if !entry.Truncated || entry.LimitBytes != ExecOutputLimit || entry.OriginalBytes != int64(len(stdout)) || entry.SHA256 == "" {
		t.Fatalf("stdout manifest entry missing truncation metadata: %#v", entry)
	}
}

func TestCaptureTmuxBoundsPanePromptAndTitleHistory(t *testing.T) {
	workspace := t.TempDir()
	capturer := New(workspace)

	var pane bytes.Buffer
	for i := range PaneLineLimit + 5 {
		pane.WriteString("line-")
		pane.WriteString(strings.Repeat("0", 4-len(intString(i))))
		pane.WriteString(intString(i))
		pane.WriteByte('\n')
	}
	rawPrompt := append([]byte("PROMPT-BEGIN"), bytes.Repeat([]byte("r"), PromptByteLimit+200)...)
	rawPrompt = append(rawPrompt, []byte("PROMPT-END")...)

	events := make([]TitleEvent, TitleEventLimit+8)
	for i := range events {
		events[i] = TitleEvent{
			TS:              time.Date(2026, 5, 7, 16, 0, i%60, 0, time.UTC),
			Source:          "title",
			Title:           "title-" + intString(i),
			State:           "busy",
			IdleTransitions: i,
			BusyTransitions: i + 1,
		}
	}
	events[len(events)-1].Title = strings.Repeat("z", TitleBytesLimit+50)

	result, err := capturer.CaptureTmux(TmuxCapture{
		Identity:          Identity{RunID: "run", TaskID: "task", StepName: "tmux"},
		Failure:           Failure{Path: "tmux_submit_prompt_timeout", Message: "timeout"},
		Transport:         Transport{Agent: "codex"},
		Pane:              Bytes(pane.Bytes()),
		TitleHistory:      events,
		TitleHistoryKnown: true,
		Metadata:          TmuxMetadata{PaneTarget: "%1", Agent: "codex", Command: "codex", FinalState: "busy"},
		Prompt: PromptPayload{
			Raw:               Bytes(rawPrompt),
			Normalized:        Bytes([]byte("line1\r\nline2\rline3")),
			NormalizationMode: "test",
			BracketedPaste:    true,
		},
		ParentStdout: Bytes([]byte("parent out")),
		ParentStderr: Bytes([]byte("parent err")),
	})
	if err != nil {
		t.Fatalf("CaptureTmux returned error: %v", err)
	}

	paneGot := string(readFile(t, filepath.Join(result.Dir, "tmux", "pane.txt")))
	if strings.Contains(paneGot, "line-0000") || !strings.Contains(paneGot, "line-3004") {
		t.Fatalf("pane did not preserve the last %d lines", PaneLineLimit)
	}

	rawGot := readFile(t, filepath.Join(result.Dir, "prompt", "raw.bin"))
	if len(rawGot) != PromptByteLimit {
		t.Fatalf("raw prompt length = %d, want %d", len(rawGot), PromptByteLimit)
	}
	if !bytes.HasPrefix(rawGot, []byte("PROMPT-BEGIN")) || !bytes.Contains(rawGot, []byte(promptElisionMarker)) || !bytes.HasSuffix(rawGot, []byte("PROMPT-END")) {
		t.Fatalf("raw prompt did not preserve first and last bytes with marker")
	}
	normalizedGot := string(readFile(t, filepath.Join(result.Dir, "prompt", "normalized.txt")))
	if strings.Contains(normalizedGot, "\r") || !strings.Contains(normalizedGot, "line1\nline2\nline3") {
		t.Fatalf("normalized prompt was not line-ending normalized: %q", normalizedGot)
	}

	titleLines := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(result.Dir, "tmux", "title-history.jsonl")))), "\n")
	if len(titleLines) != TitleEventLimit {
		t.Fatalf("title history line count = %d, want %d", len(titleLines), TitleEventLimit)
	}
	var first TitleEvent
	if err := json.Unmarshal([]byte(titleLines[0]), &first); err != nil {
		t.Fatalf("parse first title event: %v", err)
	}
	if first.Title != "title-8" {
		t.Fatalf("first retained event = %q, want title-8", first.Title)
	}
	var last TitleEvent
	if err := json.Unmarshal([]byte(titleLines[len(titleLines)-1]), &last); err != nil {
		t.Fatalf("parse last title event: %v", err)
	}
	if len([]byte(last.Title)) > TitleBytesLimit || !strings.HasSuffix(last.Title, titleElisionMarker) {
		t.Fatalf("long title was not bounded with marker: len=%d title=%q", len([]byte(last.Title)), last.Title)
	}

	manifest := readManifest(t, result.ManifestPath)
	if entry := artifactEntry(t, manifest, ArtifactTmuxTitles); !entry.Truncated || entry.OriginalEvents != len(events) || entry.LimitEvents != TitleEventLimit {
		t.Fatalf("title manifest entry missing truncation metadata: %#v", entry)
	}
	if entry := artifactEntry(t, manifest, ArtifactPromptRaw); !entry.Truncated || entry.LimitBytes != PromptByteLimit {
		t.Fatalf("prompt manifest entry missing truncation metadata: %#v", entry)
	}
	if entry := artifactEntry(t, manifest, ArtifactExecCommand); entry.Status != ArtifactStatusNotApplicable {
		t.Fatalf("tmux capture should mark exec command not_applicable: %#v", entry)
	}
}

func TestCaptureExecConcurrentSameStep(t *testing.T) {
	workspace := t.TempDir()
	capturer := New(workspace)
	identity := Identity{RunID: "run", TaskID: "task", StepName: "same-step"}

	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := range 24 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := capturer.CaptureExec(ExecCapture{
				Identity:     identity,
				Failure:      Failure{Path: "exec_step_non_zero_exit", Message: "failed " + intString(i)},
				Command:      ExecCommand{Argv: []string{"sh", "-c", "exit " + intString(i)}},
				Stdout:       Bytes([]byte("stdout " + intString(i))),
				Stderr:       Bytes([]byte("stderr " + intString(i))),
				Combined:     Bytes([]byte("combined " + intString(i))),
				ParentStdout: Bytes([]byte("parent stdout")),
				ParentStderr: Bytes([]byte("parent stderr")),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent CaptureExec returned error: %v", err)
		}
	}

	stepDir, err := capturer.StepDir(identity)
	if err != nil {
		t.Fatalf("StepDir returned error: %v", err)
	}
	manifest := readManifest(t, filepath.Join(stepDir, "manifest.json"))
	if manifest.SchemaVersion != SchemaVersion || artifactEntry(t, manifest, ArtifactExecCombined).Status != ArtifactStatusWritten {
		t.Fatalf("manifest after concurrent capture is invalid: %#v", manifest)
	}
	if leftovers := tempFiles(t, stepDir); len(leftovers) > 0 {
		t.Fatalf("temporary files left after concurrent capture: %v", leftovers)
	}
}

func TestTailBufferKeepsTailAndDigest(t *testing.T) {
	buffer := NewTailBuffer(8)
	if _, err := buffer.Write([]byte("hello")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if _, err := buffer.Write([]byte(" world")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	snapshot := buffer.Snapshot()
	if string(snapshot.Data) != "lo world" {
		t.Fatalf("tail data = %q, want lo world", snapshot.Data)
	}
	if snapshot.OriginalBytes != int64(len("hello world")) {
		t.Fatalf("original bytes = %d", snapshot.OriginalBytes)
	}
	if snapshot.SHA256 == "" {
		t.Fatalf("snapshot digest was empty")
	}
}

func readManifest(t *testing.T, path string) Manifest {
	t.Helper()
	var manifest Manifest
	readJSONFile(t, path, &manifest)
	return manifest
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	data := readFile(t, path)
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s returned error: %v", path, err)
	}
	return data
}

func artifactEntry(t *testing.T, manifest Manifest, kind string) ArtifactEntry {
	t.Helper()
	for _, entry := range manifest.Artifacts {
		if entry.Kind == kind {
			return entry
		}
	}
	t.Fatalf("artifact %s not found in manifest: %#v", kind, manifest.Artifacts)
	return ArtifactEntry{}
}

func tempFiles(t *testing.T, root string) []string {
	t.Helper()
	var matches []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(filepath.Base(path), ".tmp") {
			matches = append(matches, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir returned error: %v", err)
	}
	return matches
}

func intString(i int) string {
	if i == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for i > 0 {
		pos--
		digits[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(digits[pos:])
}
