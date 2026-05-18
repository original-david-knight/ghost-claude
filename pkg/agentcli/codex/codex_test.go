package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestNewDefaultsToTUIWhenNoSubcommandIsPresent(t *testing.T) {
	client, err := New("codex", []string{"--dangerously-bypass-approvals-and-sandbox"}, ".", "", "", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if client.transport != TransportTUI {
		t.Fatalf("expected default transport %q, got %q", TransportTUI, client.transport)
	}
}

func TestNewAcceptsExecTransport(t *testing.T) {
	client, err := New("codex", []string{"--model", "test"}, ".", " EXEC ", "", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if client.transport != TransportExec {
		t.Fatalf("expected transport %q, got %q", TransportExec, client.transport)
	}
	if client.IsFullscreenTUI() {
		t.Fatal("exec transport must not report fullscreen TUI")
	}
}

func TestNewRejectsNonInteractiveSubcommands(t *testing.T) {
	for _, subcommand := range []string{"exec", "review"} {
		t.Run(subcommand, func(t *testing.T) {
			_, err := New("codex", []string{subcommand}, ".", TransportTUI, "", io.Discard, io.Discard)
			if err == nil {
				t.Fatalf("expected New to reject %s subcommand", subcommand)
			}
			if !strings.Contains(err.Error(), "non-interactive") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewRejectsSubcommandsForExecTransport(t *testing.T) {
	_, err := New("codex", []string{"resume"}, ".", TransportExec, "", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected New to reject subcommand for exec transport")
	}
	if !strings.Contains(err.Error(), `codex transport "exec" selects non-interactive exec mode; remove codex args subcommand "resume"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunPromptExecPassesRenderedPromptOnStdin(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	capturePath := filepath.Join(dir, "capture.json")
	writeExecutable(t, scriptPath, `#!/usr/bin/env python3
import json
import os
import sys

prompt = sys.stdin.read()
with open(os.environ["CODEX_CAPTURE"], "w") as capture:
    json.dump({"args": sys.argv[1:], "prompt": prompt}, capture)
sys.stdout.write("answer:\n" + prompt)
sys.stdout.flush()
sys.stderr.write("diagnostic:" + " ".join(sys.argv[1:]) + "\n")
sys.stderr.flush()
`)
	t.Setenv("CODEX_CAPTURE", capturePath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	client, err := New(scriptPath, []string{"--model", "test"}, dir, TransportExec, "1s", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	session := &Session{}
	prompt := "rendered task alpha\nquote ' and $HOME remain literal\n"
	if err := client.RunPrompt(context.Background(), session, prompt); err != nil {
		t.Fatalf("RunPrompt returned error: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	var capture struct {
		Args   []string `json:"args"`
		Prompt string   `json:"prompt"`
	}
	readCodexJSON(t, capturePath, &capture)
	if strings.Join(capture.Args, "\x00") != strings.Join([]string{"exec", "--model", "test"}, "\x00") {
		t.Fatalf("unexpected args: %#v", capture.Args)
	}
	if capture.Prompt != prompt {
		t.Fatalf("prompt was not delivered exactly on stdin:\nwant %q\ngot  %q", prompt, capture.Prompt)
	}
	if got := stdout.String(); got != "answer:\n"+prompt {
		t.Fatalf("stdout was not streamed, got %q", got)
	}
	if got := stderr.String(); got != "diagnostic:exec --model test\n" {
		t.Fatalf("stderr was not streamed, got %q", got)
	}
	if session.Started || session.tui != nil {
		t.Fatalf("exec transport must not start a TUI session: %#v", session)
	}
}

func TestRunPromptExecIgnoresLiveOutputWriteErrors(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	writeExecutable(t, scriptPath, `#!/usr/bin/env python3
import sys

_ = sys.stdin.read()
sys.stdout.write("stdout-ok\n")
sys.stdout.flush()
sys.stderr.write("stderr-ok\n")
sys.stderr.flush()
`)

	stdout := &failingOutputWriter{}
	stderr := &failingOutputWriter{}
	client, err := New(scriptPath, nil, dir, TransportExec, "1s", stdout, stderr)
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

func TestRunPromptExecCapturesDiagnosticsOnNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	writeExecutable(t, scriptPath, `#!/usr/bin/env python3
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
	client, err := New(scriptPath, []string{"--model", "test"}, dir, TransportExec, "1s", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := WithDiagnostics(context.Background(), Diagnostics{
		Identity: diagnostics.Identity{
			RunID:    "run-1",
			TaskID:   "task-one",
			StepName: "implement",
		},
		ParentStdout: diagnostics.Bytes([]byte("parent stdout\n")),
		ParentStderr: diagnostics.Bytes([]byte("parent stderr\n")),
	})
	prompt := "implement rendered prompt\n"
	err = client.RunPrompt(ctx, &Session{}, prompt)
	if err == nil {
		t.Fatal("expected RunPrompt to fail")
	}
	if !strings.Contains(err.Error(), "codex exec failed") || !strings.Contains(err.Error(), "exit status 17") {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdout.String() != "partial stdout\n" {
		t.Fatalf("stdout was not streamed before failure, got %q", stdout.String())
	}
	if stderr.String() != "partial stderr\n" {
		t.Fatalf("stderr was not streamed before failure, got %q", stderr.String())
	}

	diagDir := filepath.Join(dir, ".vibedrive", "debug", "run-1", "task-one", "implement")
	var manifest diagnostics.Manifest
	readCodexJSON(t, filepath.Join(diagDir, "manifest.json"), &manifest)
	if manifest.Failure.Path != execFailureNonZeroExit {
		t.Fatalf("failure path = %q, want %q", manifest.Failure.Path, execFailureNonZeroExit)
	}
	if manifest.Transport.Kind != "codex_exec" || manifest.Transport.Agent != "codex" || manifest.Transport.Interactive {
		t.Fatalf("unexpected transport metadata: %#v", manifest.Transport)
	}
	if entry := codexArtifact(t, manifest, diagnostics.ArtifactPromptRaw); entry.Status != diagnostics.ArtifactStatusWritten || !entry.Required {
		t.Fatalf("prompt raw artifact not required/written: %#v", entry)
	}
	if entry := codexArtifact(t, manifest, diagnostics.ArtifactExecStderr); entry.Status != diagnostics.ArtifactStatusWritten || !entry.Required {
		t.Fatalf("stderr artifact not required/written: %#v", entry)
	}

	if got := string(readCodexFile(t, filepath.Join(diagDir, "prompt", "raw.bin"))); got != prompt {
		t.Fatalf("raw prompt diagnostic = %q, want %q", got, prompt)
	}
	if got := string(readCodexFile(t, filepath.Join(diagDir, "exec", "stdout-tail.txt"))); got != "partial stdout\n" {
		t.Fatalf("stdout diagnostic = %q", got)
	}
	if got := string(readCodexFile(t, filepath.Join(diagDir, "exec", "stderr-tail.txt"))); got != "partial stderr\n" {
		t.Fatalf("stderr diagnostic = %q", got)
	}
	if got := string(readCodexFile(t, filepath.Join(diagDir, "parent", "stdout-tail.txt"))); got != "parent stdout\n" {
		t.Fatalf("parent stdout diagnostic = %q", got)
	}

	var command diagnostics.ExecCommand
	readCodexJSON(t, filepath.Join(diagDir, "exec", "command.json"), &command)
	wantArgv := []string{scriptPath, "exec", "--model", "test"}
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

func TestRunInteractivePromptRejectsExecTransport(t *testing.T) {
	client, err := New("codex", nil, ".", TransportExec, "1s", io.Discard, io.Discard)
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

func TestTitleMonitorClassifiesIdleAndBusyTitles(t *testing.T) {
	monitor := newTitleMonitor("/tmp/vibedrive")

	if state, ok := monitor.classifyTitle("vibedrive"); !ok || state != "idle" {
		t.Fatalf("expected idle title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("david@host:~/workspace/vibedrive"); !ok || state != "idle" {
		t.Fatalf("expected path-style idle title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("gpt-5.5 xhigh · ~/workspace/vibedrive"); !ok || state != "idle" {
		t.Fatalf("expected Codex status title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("gpt-5.5 xhigh • ~/workspace/vibedrive"); !ok || state != "idle" {
		t.Fatalf("expected bullet-separated Codex status title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("gpt-5.5 xhigh · ~/workspace/planet/…/worktrees/…51448272af5"); !ok || state != "idle" {
		t.Fatalf("expected abbreviated Codex status title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("vibedriv..."); !ok || state != "idle" {
		t.Fatalf("expected truncated idle title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("...vibedrive"); !ok || state != "idle" {
		t.Fatalf("expected suffix-truncated idle title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("busy vibedrive"); !ok || state != "busy" {
		t.Fatalf("expected busy title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("⠴ vibedrive"); !ok || state != "busy" {
		t.Fatalf("expected spinner title classification, got state=%q ok=%v", state, ok)
	}
}

func TestTitleMonitorAcceptsGitRootTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Tether")
	workdir := filepath.Join(root, "TetherGame")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git returned error: %v", err)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll workdir returned error: %v", err)
	}

	monitor := newTitleMonitor(workdir)
	if state, ok := monitor.classifyTitle("Tether"); !ok || state != "idle" {
		t.Fatalf("expected git-root title to classify idle, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("⠴ Tether"); !ok || state != "busy" {
		t.Fatalf("expected spinner git-root title to classify busy, got state=%q ok=%v", state, ok)
	}
}

func TestTitleMonitorWaitsForBusyThenIdleBeforeStartupReady(t *testing.T) {
	monitor := newTitleMonitor("/tmp/vibedrive")

	monitor.consume(titleChunk("vibedrive"))
	if monitor.snapshot().readyForPrompt() {
		t.Fatal("expected initial idle title to be insufficient for startup readiness")
	}

	monitor.consume(titleChunk("busy vibedrive"))
	if monitor.snapshot().readyForPrompt() {
		t.Fatal("expected busy title to remain not ready")
	}

	monitor.consume(titleChunk("vibedrive"))
	if !monitor.snapshot().readyForPrompt() {
		t.Fatal("expected idle after busy to mark startup ready")
	}
}

func TestStartTUIAcceptsReadyScreenWhenTitleDoesNotMatchWorkspace(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	writeExecutable(t, scriptPath, `#!/usr/bin/env python3
import os
import sys

sys.stdout.write("\x1b]0;repo-root\x07")
sys.stdout.write("\x1b]0;⠋ repo-root\x07")
sys.stdout.write("""
OpenAI Codex (v0.130.0)

model:        gpt-5.5 xhigh
directory:    ~/workspace/Tether/TetherGame
permissions: YOLO mode

Tip: Use /rename to rename your threads for easier thread resuming.
""")
sys.stdout.flush()

while True:
    ch = os.read(0, 1)
    if not ch or ch == b"\x04":
        break
`)

	var stdout bytes.Buffer
	client, err := New(scriptPath, []string{"--dangerously-bypass-approvals-and-sandbox"}, dir, TransportTUI, "500ms", &stdout, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	tui, err := client.startTUI(context.Background())
	if err != nil {
		t.Fatalf("startTUI returned error: %v\nstdout:\n%s", err, stdout.String())
	}
	defer func() {
		_ = tui.Close()
	}()
}

func TestRunPromptCompletesWhenCodexUsesGitRootTitle(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "Tether")
	workdir := filepath.Join(root, "TetherGame")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git returned error: %v", err)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll workdir returned error: %v", err)
	}
	scriptPath := filepath.Join(dir, "fake-codex.py")
	writeExecutable(t, scriptPath, `#!/usr/bin/env python3
import os
import sys
import time

sys.stdout.write("\x1b]0;Tether\x07")
sys.stdout.flush()
time.sleep(0.05)
sys.stdout.write("\x1b]0;⠋ Tether\x07")
sys.stdout.flush()
time.sleep(0.05)
sys.stdout.write("\x1b]0;Tether\x07")
sys.stdout.flush()

while True:
    ch = os.read(0, 1)
    if not ch or ch == b"\x04":
        break
    if ch in (b"\r", b"\n"):
        sys.stdout.write("\x1b]0;⠋ Tether\x07")
        sys.stdout.flush()
        time.sleep(0.05)
        sys.stdout.write("done\n")
        sys.stdout.write("\x1b]0;Tether\x07")
        sys.stdout.flush()
`)

	var stdout bytes.Buffer
	client, err := New(scriptPath, []string{"--dangerously-bypass-approvals-and-sandbox"}, workdir, TransportTUI, "2s", &stdout, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	session, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	defer func() {
		_ = client.Close(session)
	}()

	if err := client.RunPrompt(context.Background(), session, "write components"); err != nil {
		t.Fatalf("RunPrompt returned error: %v\nstdout:\n%s", err, stdout.String())
	}
}

func TestTitleMonitorDetectsCodexTrustPrompt(t *testing.T) {
	monitor := newTitleMonitor("/tmp/vibedrive")

	monitor.consume([]byte(`Do you trust the contents of this directory?
› 1. Yes, continue
  2. No, quit

  Press enter to continue
`))

	snapshot := monitor.snapshot()
	if snapshot.trustPrompts != 1 {
		t.Fatalf("expected one trust prompt, got %#v", snapshot)
	}

	monitor.consume([]byte("still showing trust prompt"))
	if got := monitor.snapshot().trustPrompts; got != 1 {
		t.Fatalf("expected repeated trust prompt text not to increment, got %d", got)
	}
}

func TestStartTUIConfirmsCodexTrustPrompt(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	markerPath := filepath.Join(dir, "trusted")
	writeExecutable(t, scriptPath, fmt.Sprintf(`#!/usr/bin/env python3
import os
import sys
import time

idle = os.path.basename(os.getcwd()) or "codex"
sys.stdout.write(f"\x1b]0;{idle}\x07")
sys.stdout.write("""Do you trust the contents of this directory?
› 1. Yes, continue
  2. No, quit

  Press enter to continue
""")
sys.stdout.flush()

ch = os.read(0, 1)
if ch in (b"\r", b"\n"):
    with open(%q, "w") as marker:
        marker.write("trusted\n")

sys.stdout.write(f"\x1b]0;busy {idle}\x07")
sys.stdout.flush()
time.sleep(0.05)
sys.stdout.write(f"\x1b]0;{idle}\x07")
sys.stdout.flush()

while True:
    ch = os.read(0, 1)
    if not ch or ch == b"\x04":
        break
`, markerPath))

	var stdout bytes.Buffer
	client, err := New(scriptPath, []string{"--dangerously-bypass-approvals-and-sandbox"}, dir, TransportTUI, "2s", &stdout, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	tui, err := client.startTUI(context.Background())
	if err != nil {
		t.Fatalf("startTUI returned error: %v\nstdout:\n%s", err, stdout.String())
	}
	defer func() {
		_ = tui.Close()
	}()

	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected trust prompt marker to be written, stat err=%v\nstdout:\n%s", err, stdout.String())
	}
}

func TestRunPromptReturnsTUISubmitErrorWithoutExecFallback(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	writeExecutable(t, scriptPath, `#!/usr/bin/env python3
import os
import sys
import time

idle = os.path.basename(os.getcwd()) or "codex"
sys.stdout.write(f"\x1b]0;{idle}\x07")
sys.stdout.flush()
time.sleep(0.05)
sys.stdout.write(f"\x1b]0;busy {idle}\x07")
sys.stdout.flush()
time.sleep(0.05)
sys.stdout.write(f"\x1b]0;{idle}\x07")
sys.stdout.flush()

while True:
    ch = os.read(0, 1)
    if not ch or ch == b"\x04":
        break
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	client, err := New(scriptPath, []string{"--dangerously-bypass-approvals-and-sandbox"}, dir, TransportTUI, "2s", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	session, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	defer func() {
		_ = client.Close(session)
	}()

	err = client.RunPrompt(context.Background(), session, "ignored")
	if err == nil {
		t.Fatal("expected RunPrompt to fail when the TUI does not acknowledge submission")
	}
	if !strings.Contains(err.Error(), "codex tui did not start processing") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(stderr.String(), "falling back") {
		t.Fatalf("expected no exec fallback warning, got %q", stderr.String())
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}

func readCodexJSON(t *testing.T, path string, target any) {
	t.Helper()

	data := readCodexFile(t, path)
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("parse JSON %s: %v\n%s", path, err, data)
	}
}

func readCodexFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func codexArtifact(t *testing.T, manifest diagnostics.Manifest, kind string) diagnostics.ArtifactEntry {
	t.Helper()

	for _, entry := range manifest.Artifacts {
		if entry.Kind == kind {
			return entry
		}
	}
	t.Fatalf("artifact %q not found in manifest: %#v", kind, manifest.Artifacts)
	return diagnostics.ArtifactEntry{}
}

func titleChunk(title string) []byte {
	return []byte("\x1b]0;" + title + "\x07")
}
