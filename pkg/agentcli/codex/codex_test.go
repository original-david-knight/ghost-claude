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

func TestNewDefaultsToTUIWhenNoSubcommandIsPresent(t *testing.T) {
	client, err := New("codex", []string{"--dangerously-bypass-approvals-and-sandbox"}, ".", "", "", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if client.transport != TransportTUI {
		t.Fatalf("expected default transport %q, got %q", TransportTUI, client.transport)
	}
}

func TestNewRejectsExecTransport(t *testing.T) {
	_, err := New("codex", nil, ".", "exec", "", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected New to reject exec transport")
	}
	if !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("unexpected error: %v", err)
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

func TestTitleMonitorClassifiesIdleAndBusyTitles(t *testing.T) {
	monitor := newTitleMonitor("/tmp/vibedrive")

	if state, ok := monitor.classifyTitle("vibedrive"); !ok || state != "idle" {
		t.Fatalf("expected idle title classification, got state=%q ok=%v", state, ok)
	}
	if state, ok := monitor.classifyTitle("busy vibedrive"); !ok || state != "busy" {
		t.Fatalf("expected busy title classification, got state=%q ok=%v", state, ok)
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

func titleChunk(title string) []byte {
	return []byte("\x1b]0;" + title + "\x07")
}
