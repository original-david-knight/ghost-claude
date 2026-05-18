package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedrive/internal/diagnostics"
)

type failingOutputWriter struct {
	writes int
}

func (w *failingOutputWriter) Write(_ []byte) (int, error) {
	w.writes++
	return 0, os.ErrClosed
}

func TestNewAcceptsPrintTransport(t *testing.T) {
	client, err := New("claude", nil, ".", " PRINT ", "1s", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if client.transport != TransportPrint {
		t.Fatalf("expected transport %q, got %q", TransportPrint, client.transport)
	}
	if client.IsFullscreenTUI() {
		t.Fatal("print transport must not report fullscreen TUI")
	}
}

func TestRunPromptPrintPassesRenderedPromptOnStdin(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude.py")
	capturePath := filepath.Join(dir, "capture.json")
	writeClaudePrintScript(t, scriptPath, `#!/usr/bin/env python3
import json
import os
import sys

prompt = sys.stdin.read()
with open(os.environ["CLAUDE_CAPTURE"], "w") as capture:
    json.dump({"args": sys.argv[1:], "prompt": prompt}, capture)
sys.stdout.write("answer:\n" + prompt)
sys.stdout.flush()
sys.stderr.write("diagnostic:" + " ".join(sys.argv[1:]) + "\n")
sys.stderr.flush()
`)
	t.Setenv("CLAUDE_CAPTURE", capturePath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	client, err := New(scriptPath, []string{"--effort", "max"}, dir, TransportPrint, "1s", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	session := &Session{Strategy: SessionStrategySessionID, ID: "session-1"}
	prompt := "rendered task alpha\nquote ' and $HOME remain literal\n"
	if err := client.RunPrompt(context.Background(), session, prompt); err != nil {
		t.Fatalf("RunPrompt returned error: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	var capture struct {
		Args   []string `json:"args"`
		Prompt string   `json:"prompt"`
	}
	readClaudePrintJSON(t, capturePath, &capture)
	if strings.Join(capture.Args, "\x00") != strings.Join([]string{"--effort", "max", "--print"}, "\x00") {
		t.Fatalf("unexpected args: %#v", capture.Args)
	}
	if capture.Prompt != prompt {
		t.Fatalf("prompt was not delivered exactly on stdin:\nwant %q\ngot  %q", prompt, capture.Prompt)
	}
	if got := stdout.String(); got != "answer:\n"+prompt {
		t.Fatalf("stdout was not streamed, got %q", got)
	}
	if got := stderr.String(); got != "diagnostic:--effort max --print\n" {
		t.Fatalf("stderr was not streamed, got %q", got)
	}
	if session.Started || session.tui != nil {
		t.Fatalf("print transport must not start a TUI session: %#v", session)
	}
}

func TestRunPromptPrintIgnoresLiveOutputWriteErrors(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude.py")
	writeClaudePrintScript(t, scriptPath, `#!/usr/bin/env python3
import sys

_ = sys.stdin.read()
sys.stdout.write("stdout-ok\n")
sys.stdout.flush()
sys.stderr.write("stderr-ok\n")
sys.stderr.flush()
`)

	stdout := &failingOutputWriter{}
	stderr := &failingOutputWriter{}
	client, err := New(scriptPath, nil, dir, TransportPrint, "1s", stdout, stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), &Session{}, "prompt"); err != nil {
		t.Fatalf("RunPrompt returned error: %v", err)
	}
	if stdout.writes == 0 {
		t.Fatal("expected stdout display writer to receive output")
	}
	if stderr.writes == 0 {
		t.Fatal("expected stderr display writer to receive output")
	}
}

func TestRunPromptPrintCapturesDiagnosticsOnNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude.py")
	writeClaudePrintScript(t, scriptPath, `#!/usr/bin/env python3
import sys

_ = sys.stdin.read()
sys.stdout.write("partial stdout\n")
sys.stdout.flush()
sys.stderr.write("partial stderr\n")
sys.stderr.flush()
sys.exit(17)
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	client, err := New(scriptPath, []string{"--model", "test"}, dir, TransportPrint, "1s", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := WithDiagnostics(context.Background(), Diagnostics{
		Identity: diagnostics.Identity{
			RunID:    "run-1",
			TaskID:   "task-one",
			StepName: "review",
		},
		ParentStdout: diagnostics.Bytes([]byte("parent stdout\n")),
		ParentStderr: diagnostics.Bytes([]byte("parent stderr\n")),
	})
	prompt := "review rendered prompt\n"
	err = client.RunPrompt(ctx, &Session{}, prompt)
	if err == nil {
		t.Fatal("expected RunPrompt to fail")
	}
	if !strings.Contains(err.Error(), "claude print failed") || !strings.Contains(err.Error(), "exit status 17") {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdout.String() != "partial stdout\n" {
		t.Fatalf("stdout was not streamed before failure, got %q", stdout.String())
	}
	if stderr.String() != "partial stderr\n" {
		t.Fatalf("stderr was not streamed before failure, got %q", stderr.String())
	}

	diagDir := filepath.Join(dir, ".vibedrive", "debug", "run-1", "task-one", "review")
	var manifest diagnostics.Manifest
	readClaudePrintJSON(t, filepath.Join(diagDir, "manifest.json"), &manifest)
	if manifest.Failure.Path != printFailureNonZeroExit {
		t.Fatalf("failure path = %q, want %q", manifest.Failure.Path, printFailureNonZeroExit)
	}
	if manifest.Transport.Kind != "claude_print" || manifest.Transport.Agent != "claude" || manifest.Transport.Interactive {
		t.Fatalf("unexpected transport metadata: %#v", manifest.Transport)
	}
	if entry := claudePrintArtifact(t, manifest, diagnostics.ArtifactPromptRaw); entry.Status != diagnostics.ArtifactStatusWritten || !entry.Required {
		t.Fatalf("prompt raw artifact not required/written: %#v", entry)
	}
	if entry := claudePrintArtifact(t, manifest, diagnostics.ArtifactExecStderr); entry.Status != diagnostics.ArtifactStatusWritten || !entry.Required {
		t.Fatalf("stderr artifact not required/written: %#v", entry)
	}

	if got := string(readClaudePrintFile(t, filepath.Join(diagDir, "prompt", "raw.bin"))); got != prompt {
		t.Fatalf("raw prompt diagnostic = %q, want %q", got, prompt)
	}
	if got := string(readClaudePrintFile(t, filepath.Join(diagDir, "exec", "stdout-tail.txt"))); got != "partial stdout\n" {
		t.Fatalf("stdout diagnostic = %q", got)
	}
	if got := string(readClaudePrintFile(t, filepath.Join(diagDir, "exec", "stderr-tail.txt"))); got != "partial stderr\n" {
		t.Fatalf("stderr diagnostic = %q", got)
	}
	if got := string(readClaudePrintFile(t, filepath.Join(diagDir, "parent", "stdout-tail.txt"))); got != "parent stdout\n" {
		t.Fatalf("parent stdout diagnostic = %q", got)
	}

	var command diagnostics.ExecCommand
	readClaudePrintJSON(t, filepath.Join(diagDir, "exec", "command.json"), &command)
	wantArgv := []string{scriptPath, "--model", "test", "--print"}
	if strings.Join(command.Argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("command argv = %#v, want %#v", command.Argv, wantArgv)
	}
	if command.ExitCode == nil || *command.ExitCode != 17 {
		t.Fatalf("exit_code = %#v, want 17", command.ExitCode)
	}
	if command.TimedOut {
		t.Fatalf("timed_out = true, want false")
	}
}

func TestRunInteractivePromptRejectsPrintTransport(t *testing.T) {
	client, err := New("claude", nil, ".", TransportPrint, "1s", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	err = client.RunInteractivePrompt(context.Background(), &Session{}, "interactive")
	if err == nil {
		t.Fatal("expected RunInteractivePrompt to fail")
	}
	if !strings.Contains(err.Error(), "does not support interactive TUI prompts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeClaudePrintScript(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}

func readClaudePrintJSON(t *testing.T, path string, target any) {
	t.Helper()

	data := readClaudePrintFile(t, path)
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("parse JSON %s: %v\n%s", path, err, data)
	}
}

func readClaudePrintFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func claudePrintArtifact(t *testing.T, manifest diagnostics.Manifest, kind string) diagnostics.ArtifactEntry {
	t.Helper()

	for _, entry := range manifest.Artifacts {
		if entry.Kind == kind {
			return entry
		}
	}
	t.Fatalf("manifest missing artifact %q: %#v", kind, manifest.Artifacts)
	return diagnostics.ArtifactEntry{}
}
