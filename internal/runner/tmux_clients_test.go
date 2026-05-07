package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vibedrive/internal/diagnostics"
	"vibedrive/internal/tmuxagent"
	"vibedrive/pkg/agentcli/claude"
	"vibedrive/pkg/agentcli/codex"
	"vibedrive/pkg/ptyrunner"
)

func TestWaitForTmuxCloseForceKillsPaneWhenCloseContextExpires(t *testing.T) {
	killed := false
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(ctx context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%1\n"), nil
			case len(args) > 0 && args[0] == "display-message":
				<-ctx.Done()
				return nil, ctx.Err()
			case len(args) > 0 && args[0] == "kill-pane":
				killed = true
				return nil, nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForTmuxClose(ctx, pane); err != nil {
		t.Fatalf("waitForTmuxClose returned error: %v", err)
	}
	if !killed {
		t.Fatal("expected close timeout path to force-kill the tmux pane")
	}
}

func TestCodexCloseIgnoresAlreadyMissingTmuxPane(t *testing.T) {
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%23\n"), nil
			case len(args) > 0 && args[0] == "send-keys":
				return []byte("can't find pane: %23\n"), errors.New("exit status 1")
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	if err := (&tmuxCodexSession{pane: pane}).close(context.Background()); err != nil {
		t.Fatalf("close returned error: %v", err)
	}
}

func TestClaudeCloseIgnoresAlreadyMissingTmuxPane(t *testing.T) {
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%22\n"), nil
			case len(args) > 0 && args[0] == "load-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return []byte("can't find pane: %22\n"), errors.New("exit status 1")
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "claude",
		Command: "claude",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	if err := (&tmuxClaudeSession{pane: pane}).close(context.Background()); err != nil {
		t.Fatalf("close returned error: %v", err)
	}
}

func TestWaitForTmuxIdleReportsExpiredContextDirectly(t *testing.T) {
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(ctx context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%1\n"), nil
			case len(args) > 0 && args[0] == "display-message":
				return nil, ctx.Err()
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := newTmuxCodexSession("")
	session.pane = pane
	err = session.waitForIdleTransition(ctx, 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if strings.Contains(err.Error(), "display tmux pane") {
		t.Fatalf("expected direct context error, got %v", err)
	}
}

func TestTmuxCodexClassifiesPathStyleIdleTitle(t *testing.T) {
	session := newTmuxCodexSession("/tmp/worktrees/001-render-api-coordinate-contract-abcdef123456")

	for _, title := range []string{
		"001-render-api-coordinate-contract-abcdef123456",
		"/tmp/worktrees/001-render-api-coordinate-contract-abcdef123456",
		"david@host:~/workspace/planet/.vibedrive/worktrees/001-render-api-coordinate-contract-abcdef123456",
		"gpt-5.5 xhigh · ~/workspace/planet/.vibedrive/worktrees/001-render-api-coordinate-contract-abcdef123456",
		"gpt-5.5 xhigh • ~/workspace/planet/.vibedrive/worktrees/001-render-api-coordinate-contract-abcdef123456",
		"gpt-5.5 xhigh · ~/workspace/planet/…/worktrees/…51448272af5",
		"001-render-api-coordi...",
		"001-render-api-coordinate-contract-...",
		"...abcdef123456",
	} {
		state, ok := session.classifyTitle(title)
		if !ok || state != "idle" {
			t.Fatalf("expected idle title classification for %q, got state=%q ok=%v", title, state, ok)
		}
	}

	for _, title := range []string{
		"busy 001-render-api-coordinate-contract-abcdef123456",
		"⠴ 001-render-api-coordinate-contract-abcdef123456",
	} {
		state, ok := session.classifyTitle(title)
		if !ok || state != "busy" {
			t.Fatalf("expected busy title classification for %q, got state=%q ok=%v", title, state, ok)
		}
	}
}

func TestTmuxCodexSnapshotTreatsTruncatedIdlePaneTitleAsIdle(t *testing.T) {
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%1\n"), nil
			case len(args) > 0 && args[0] == "display-message":
				return []byte("001-render-api-coordi...\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(`
• Implemented render-api-coordinate-contract.

─ Worked for 9m 38s ───────────────────────────────────────────────────────────────────

 
› Explain this codebase
 
  gpt-5.5 xhigh · ~/workspace/planet/.vibedrive/worktrees/001-render-api-coordinate-co…
`), nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	session := newTmuxCodexSession("/tmp/worktrees/001-render-api-coordinate-contract-abcdef123456")
	session.pane = pane
	session.currentState = "busy"
	session.busyTransitions = 2

	snapshot, err := session.snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot returned error: %v", err)
	}
	if snapshot.currentState != "idle" || snapshot.idleTransitions != 1 {
		t.Fatalf("expected truncated title to record idle transition, got %#v", snapshot)
	}
}

func TestTmuxCodexStartupAcceptsReadyScreenWhenTitleIsAbbreviated(t *testing.T) {
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%1\n"), nil
			case len(args) > 0 && args[0] == "display-message":
				return []byte("gpt-5.5 xhigh · ~/workspace/planet/…/worktrees/…51448272af5\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(`
OpenAI Codex (v0.128.0)

model:        gpt-5.5 xhigh
directory:    ~/workspace/planet/…/worktrees/…51448272af5
permissions: YOLO mode

Tip: You can resume a previous conversation by running codex resume
`), nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	session := newTmuxCodexSession("/tmp/worktrees/001-render-api-coordinate-contract-51448272af5")
	session.pane = pane
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.completeStartup(ctx); err != nil {
		t.Fatalf("completeStartup returned error: %v", err)
	}
}

func TestTmuxCodexStartupConfirmsTrustPromptBeforeIdleTitle(t *testing.T) {
	enterCount := 0
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%1\n"), nil
			case len(args) > 0 && args[0] == "display-message":
				return []byte("vibedrive\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				if enterCount == 0 {
					return []byte(`
Do you trust the contents of this directory? Working with untrusted contents
comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit

  Press enter to continue
`), nil
				}
				return []byte(`
OpenAI Codex (v0.128.0)

model:        gpt-5.5 xhigh
directory:    /tmp/vibedrive
permissions: YOLO mode
`), nil
			case len(args) > 0 && args[0] == "send-keys":
				enterCount++
				return nil, nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	session := newTmuxCodexSession("/tmp/vibedrive")
	session.pane = pane
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.completeStartup(ctx); err != nil {
		t.Fatalf("completeStartup returned error: %v", err)
	}
	if enterCount != 1 {
		t.Fatalf("expected one trust prompt confirmation, got %d", enterCount)
	}
}

func TestTmuxCodexSnapshotTreatsWorkingScreenAsBusyWithIdleTitle(t *testing.T) {
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%1\n"), nil
			case len(args) > 0 && args[0] == "display-message":
				return []byte("gpt-5.5 xhigh · ~/workspace/planet/…/worktrees/…51448272af5\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(`
• Working (6s • esc to interrupt)

> Explain this codebase

gpt-5.5 xhigh · ~/workspace/planet/.vibedrive/worktrees/001-data-fixtures-and-asset-51448272af5
`), nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}

	session := newTmuxCodexSession("/tmp/worktrees/001-data-fixtures-and-asset-51448272af5")
	session.pane = pane
	session.currentState = "idle"
	session.idleTransitions = 1
	session.busyTransitions = 1

	snapshot, err := session.snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot returned error: %v", err)
	}
	if snapshot.currentState != "busy" || snapshot.busyTransitions != 2 {
		t.Fatalf("expected working screen to record busy transition, got %#v", snapshot)
	}
}

func TestTmuxCodexDiagnosticsWrittenOnUnexpectedExit(t *testing.T) {
	workspace := t.TempDir()
	var pasted string
	entered := false
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%1\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				return []byte("vibedrive\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				if entered {
					return []byte("1\n"), nil
				}
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte("FULL CODEX PANE SNAPSHOT\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(`
OpenAI Codex (v0.128.0)

model:        gpt-5.5 xhigh
directory:    /tmp/vibedrive
permissions: YOLO mode
`), nil
			case len(args) > 0 && args[0] == "load-buffer":
				pasted = stdin
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				if tmuxArgsContain(args, "Enter") {
					entered = true
				}
				return nil, nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	client, err := newTmuxCodexClient("codex", nil, workspace, "", controller, "task")
	if err != nil {
		t.Fatalf("newTmuxCodexClient returned error: %v", err)
	}

	ctx := withTmuxDiagnosticsIdentity(context.Background(), "run-1", "task-one", "execute")
	err = client.RunPrompt(ctx, &codex.Session{}, "hello\nworld")
	if err == nil {
		t.Fatal("expected RunPrompt to fail")
	}
	dir := filepath.Join(workspace, ".vibedrive", "debug", "run-1", "task-one", "execute")
	if !strings.Contains(err.Error(), "tmux diagnostics captured at "+dir) {
		t.Fatalf("expected diagnostics path in error, got %v", err)
	}
	if strings.Contains(err.Error(), "last tmux pane output") {
		t.Fatalf("expected structured diagnostics instead of pane output in error, got %v", err)
	}

	assertTmuxDiagnosticsBundle(t, dir, tmuxFailureUnexpectedExit, "codex", "FULL CODEX PANE SNAPSHOT")
	if want := ptyrunner.BracketedPasteStart + "hello world" + ptyrunner.BracketedPasteEnd; pasted != want {
		t.Fatalf("unexpected pasted payload %q, want %q", pasted, want)
	}
	if got := readTextFile(t, filepath.Join(dir, "prompt", "raw.bin")); got != pasted {
		t.Fatalf("prompt/raw.bin = %q, want pasted payload %q", got, pasted)
	}
	if got := readTextFile(t, filepath.Join(dir, "prompt", "normalized.txt")); got != "hello world" {
		t.Fatalf("prompt/normalized.txt = %q, want normalized prompt", got)
	}
}

func TestTmuxClaudeDiagnosticsWrittenOnSubmitTimeout(t *testing.T) {
	workspace := t.TempDir()
	var pasted string
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%2\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				return []byte("✳ ready\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte("FULL CLAUDE PANE SNAPSHOT\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte("Claude ready\n"), nil
			case len(args) > 0 && args[0] == "load-buffer":
				pasted = stdin
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				return nil, nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	client, err := newTmuxClaudeClient("claude", nil, workspace, "1s", controller, "task")
	if err != nil {
		t.Fatalf("newTmuxClaudeClient returned error: %v", err)
	}

	base := withTmuxDiagnosticsIdentity(context.Background(), "run-2", "task-two", "review")
	ctx, cancel := context.WithTimeout(base, 180*time.Millisecond)
	defer cancel()
	err = client.RunPrompt(ctx, &claude.Session{}, "review\nnow")
	if err == nil {
		t.Fatal("expected RunPrompt to fail")
	}
	dir := filepath.Join(workspace, ".vibedrive", "debug", "run-2", "task-two", "review")
	if !strings.Contains(err.Error(), "tmux diagnostics captured at "+dir) {
		t.Fatalf("expected diagnostics path in error, got %v", err)
	}

	assertTmuxDiagnosticsBundle(t, dir, tmuxFailureSubmitTimeout, "claude", "FULL CLAUDE PANE SNAPSHOT")
	if pasted != "review now" {
		t.Fatalf("unexpected pasted payload %q", pasted)
	}
	if got := readTextFile(t, filepath.Join(dir, "prompt", "raw.bin")); got != "review now" {
		t.Fatalf("prompt/raw.bin = %q, want pasted payload", got)
	}
}

func TestTmuxCodexDiagnosticsWrittenOnStartupTimeout(t *testing.T) {
	workspace := t.TempDir()
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%3\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				return []byte("busy vibedrive\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("1\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte("STARTUP TIMEOUT PANE SNAPSHOT\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte("still starting\n"), nil
			case len(args) > 0 && args[0] == "send-keys":
				return nil, nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	client, err := newTmuxCodexClient("codex", nil, workspace, "1ms", controller, "task")
	if err != nil {
		t.Fatalf("newTmuxCodexClient returned error: %v", err)
	}

	ctx := withTmuxDiagnosticsIdentity(context.Background(), "run-3", "task-three", "startup")
	err = client.RunPrompt(ctx, &codex.Session{}, "ignored")
	if err == nil {
		t.Fatal("expected RunPrompt to fail")
	}
	dir := filepath.Join(workspace, ".vibedrive", "debug", "run-3", "task-three", "startup")
	if !strings.Contains(err.Error(), "tmux diagnostics captured at "+dir) {
		t.Fatalf("expected diagnostics path in error, got %v", err)
	}

	manifest := assertTmuxDiagnosticsBundle(t, dir, tmuxFailureStartupTimeout, "codex", "STARTUP TIMEOUT PANE SNAPSHOT")
	if entry := diagnosticsArtifact(t, manifest, diagnostics.ArtifactPromptRaw); entry.Status != diagnostics.ArtifactStatusUnavailable {
		t.Fatalf("startup prompt artifact status = %q, want unavailable", entry.Status)
	}
}

func assertTmuxDiagnosticsBundle(t *testing.T, dir, failurePath, agent, paneContains string) diagnostics.Manifest {
	t.Helper()
	manifest := readDiagnosticsManifest(t, filepath.Join(dir, "manifest.json"))
	if manifest.Failure.Path != failurePath {
		t.Fatalf("failure path = %q, want %q", manifest.Failure.Path, failurePath)
	}
	if manifest.Transport.Kind != "tmux" || manifest.Transport.Agent != agent || !manifest.Transport.Interactive {
		t.Fatalf("unexpected transport metadata: %#v", manifest.Transport)
	}
	if got := readTextFile(t, filepath.Join(dir, "tmux", "pane.txt")); !strings.Contains(got, paneContains) {
		t.Fatalf("tmux pane diagnostics did not contain %q:\n%s", paneContains, got)
	}
	if got := strings.TrimSpace(readTextFile(t, filepath.Join(dir, "tmux", "title-history.jsonl"))); got == "" {
		t.Fatal("expected title history diagnostics to be written")
	}
	metadata := readTextFile(t, filepath.Join(dir, "tmux", "metadata.json"))
	for _, want := range []string{`"agent": "` + agent + `"`, `"timing"`, `"transition_counters"`} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("metadata missing %q:\n%s", want, metadata)
		}
	}
	for _, kind := range []string{
		diagnostics.ArtifactTmuxPane,
		diagnostics.ArtifactTmuxTitles,
		diagnostics.ArtifactTmuxMetadata,
		diagnostics.ArtifactParentStdout,
		diagnostics.ArtifactParentStderr,
		diagnostics.ArtifactManifest,
	} {
		entry := diagnosticsArtifact(t, manifest, kind)
		if entry.Status == "" {
			t.Fatalf("manifest missing artifact %q", kind)
		}
	}
	return manifest
}

func readDiagnosticsManifest(t *testing.T, path string) diagnostics.Manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest %s: %v", path, err)
	}
	var manifest diagnostics.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest %s: %v", path, err)
	}
	return manifest
}

func diagnosticsArtifact(t *testing.T, manifest diagnostics.Manifest, kind string) diagnostics.ArtifactEntry {
	t.Helper()
	for _, entry := range manifest.Artifacts {
		if entry.Kind == kind {
			return entry
		}
	}
	t.Fatalf("manifest missing artifact %q", kind)
	return diagnostics.ArtifactEntry{}
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func tmuxArgsContain(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
