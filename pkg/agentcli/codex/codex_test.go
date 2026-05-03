package codex

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunPromptFiltersFileReadOutputForExec(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.sh")
	writeExecutable(t, scriptPath, `#!/usr/bin/env bash
if [ "$1" != "exec" ]; then
  exit 11
fi
if [ "$2" != "--json" ]; then
  exit 12
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"Inspecting README.md"}}'
printf '%s\n' '{"type":"item.started","item":{"type":"command_execution","command":"/bin/bash -lc '\''sed -n \"1,5p\" README.md'\''"}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"command_execution","command":"/bin/bash -lc '\''sed -n \"1,5p\" README.md'\''","aggregated_output":"line 1\nline 2\n","exit_code":0}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"README.md","kind":"update"}]}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"DONE"}}'
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	client, err := New(scriptPath, []string{"exec"}, dir, TransportExec, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), nil, "ignored"); err != nil {
		t.Fatalf("RunPrompt returned error: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, "Inspecting README.md") {
		t.Fatalf("expected agent message in output, got %q", got)
	}
	if !strings.Contains(got, `$ /bin/bash -lc 'sed -n "1,5p" README.md'`) {
		t.Fatalf("expected command line in output, got %q", got)
	}
	if strings.Contains(got, "line 1") || strings.Contains(got, "line 2") {
		t.Fatalf("expected file contents to be suppressed, got %q", got)
	}
	if !strings.Contains(got, "updated README.md") {
		t.Fatalf("expected file change summary in output, got %q", got)
	}
}

func TestRunPromptSuppressesCommandOutputForExec(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.sh")
	writeExecutable(t, scriptPath, `#!/usr/bin/env bash
if [ "$1" != "exec" ]; then
  exit 21
fi
if [ "$2" != "--json" ]; then
  exit 22
fi
printf '%s\n' '{"type":"item.started","item":{"type":"command_execution","command":"/bin/bash -lc '\''go test ./...'\''"}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"command_execution","command":"/bin/bash -lc '\''go test ./...'\''","aggregated_output":"FAIL\tpkg/example\t0.123s\n","exit_code":1}}'
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	client, err := New(scriptPath, []string{"exec"}, dir, TransportExec, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), nil, "ignored"); err != nil {
		t.Fatalf("RunPrompt returned error: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, `$ /bin/bash -lc 'go test ./...'`) {
		t.Fatalf("expected command line in output, got %q", got)
	}
	if strings.Contains(got, "FAIL\tpkg/example\t0.123s") {
		t.Fatalf("expected command output to be suppressed, got %q", got)
	}
}

func TestRunPromptLeavesExplicitJSONPassthroughUntouched(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.sh")
	writeExecutable(t, scriptPath, `#!/usr/bin/env bash
for arg in "$@"; do
  printf '%s\n' "$arg"
done
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	client, err := New(scriptPath, []string{"exec", "--json"}, dir, TransportExec, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), nil, "hello"); err != nil {
		t.Fatalf("RunPrompt returned error: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, "--json\nhello\n") {
		t.Fatalf("expected passthrough args and prompt, got %q", got)
	}
}

func TestShouldFilterExecOutputDetectsExecAfterGlobalFlags(t *testing.T) {
	client, err := New("codex", []string{"--dangerously-bypass-approvals-and-sandbox", "exec", "-c", `model_reasoning_effort="xhigh"`}, ".", TransportExec, "", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if !client.shouldFilterExecOutput() {
		t.Fatalf("expected exec detection to ignore leading global flags")
	}
}

func TestNewDefaultsToTUIWhenNoExecSubcommandIsPresent(t *testing.T) {
	client, err := New("codex", []string{"--dangerously-bypass-approvals-and-sandbox"}, ".", "", "", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if client.transport != TransportTUI {
		t.Fatalf("expected default transport %q, got %q", TransportTUI, client.transport)
	}
}

func TestTitleMonitorClassifiesIdleAndBusyTitles(t *testing.T) {
	monitor := newTitleMonitor("/tmp/vibedrive")

	if state, ok := monitor.classifyTitle("vibedrive"); !ok || state != "idle" {
		t.Fatalf("expected idle title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("⠋ vibedrive"); !ok || state != "busy" {
		t.Fatalf("expected busy title classification, got state=%q ok=%v", state, ok)
	}
}

func TestTitleMonitorWaitsForBusyThenIdleBeforeStartupReady(t *testing.T) {
	monitor := newTitleMonitor("/tmp/vibedrive")

	monitor.consume(titleChunk("vibedrive"))
	if monitor.snapshot().readyForPrompt() {
		t.Fatal("expected initial idle title to be insufficient for startup readiness")
	}

	monitor.consume(titleChunk("⠋ vibedrive"))
	if monitor.snapshot().readyForPrompt() {
		t.Fatal("expected busy title to remain not ready")
	}

	monitor.consume(titleChunk("vibedrive"))
	if !monitor.snapshot().readyForPrompt() {
		t.Fatal("expected idle after busy to mark startup ready")
	}
}

func TestExecArgsAppendsExecAndDropsNoAltScreenForTUIFallback(t *testing.T) {
	client, err := New("codex", []string{"--dangerously-bypass-approvals-and-sandbox", "--no-alt-screen", "-c", `model_reasoning_effort="xhigh"`}, ".", TransportTUI, "", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	got := client.execArgs()
	want := []string{"--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="xhigh"`, "exec"}
	if !slices.Equal(got, want) {
		t.Fatalf("execArgs = %v, want %v", got, want)
	}
}

func TestRunPromptFallsBackToExecWhenTUISubmitIsNotAcknowledged(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	writeExecutable(t, scriptPath, `#!/usr/bin/env python3
import json
import os
import sys
import time

if "exec" in sys.argv[1:]:
    print(json.dumps({"type":"item.completed","item":{"type":"agent_message","text":"fell back to exec"}}))
    sys.exit(0)

idle = os.path.basename(os.getcwd()) or "codex"
sys.stdout.write(f"\x1b]0;{idle}\x07")
sys.stdout.flush()
time.sleep(0.05)
sys.stdout.write(f"\x1b]0;⠋ {idle}\x07")
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

	client, err := New(scriptPath, []string{"--dangerously-bypass-approvals-and-sandbox", "--no-alt-screen"}, dir, TransportTUI, "2s", &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	session, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), session, "ignored"); err != nil {
		t.Fatalf("RunPrompt returned error: %v", err)
	}
	if !session.ExecFallback {
		t.Fatal("expected session to switch into exec fallback mode")
	}
	if !strings.Contains(stdout.String(), "fell back to exec") {
		t.Fatalf("expected exec fallback output, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "falling back to `codex exec`") {
		t.Fatalf("expected fallback warning, got %q", stderr.String())
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}

func titleChunk(title string) []byte {
	return []byte("\x1b]0;" + title + "\x07")
}
