package codex

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
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

	client, err := New(scriptPath, []string{"exec"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), "ignored"); err != nil {
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

	client, err := New(scriptPath, []string{"exec"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), "ignored"); err != nil {
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

	client, err := New(scriptPath, []string{"exec", "--json"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := client.RunPrompt(context.Background(), "hello"); err != nil {
		t.Fatalf("RunPrompt returned error: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, "--json\nhello\n") {
		t.Fatalf("expected passthrough args and prompt, got %q", got)
	}
}

func TestShouldFilterExecOutputDetectsExecAfterGlobalFlags(t *testing.T) {
	client, err := New("codex", []string{"--dangerously-bypass-approvals-and-sandbox", "exec", "-c", `model_reasoning_effort="xhigh"`}, ".", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if !client.shouldFilterExecOutput() {
		t.Fatalf("expected exec detection to ignore leading global flags")
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
