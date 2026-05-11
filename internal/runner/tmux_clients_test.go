package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	session := newTmuxCodexSession("", 0)
	session.pane = pane
	err = session.waitForIdleTransition(ctx, 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if strings.Contains(err.Error(), "display tmux pane") {
		t.Fatalf("expected direct context error, got %v", err)
	}
}

func TestWaitForTmuxIdleRequestsRedrawWhenPaneStateStaysStale(t *testing.T) {
	workspace := t.TempDir()
	readyFixture, readyScreen := readCodexDetectorFixture(t, "ready-screen")
	busyFixture, workingScreen := readCodexDetectorFixture(t, "working-screen")

	oldInterval := tmuxPaneRedrawInterval
	tmuxPaneRedrawInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		tmuxPaneRedrawInterval = oldInterval
	})

	redraws := 0
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%8\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				if redraws == 0 {
					return []byte(busyFixture.Title + "\n"), nil
				}
				return []byte(readyFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				if redraws == 0 {
					return []byte(workingScreen), nil
				}
				return []byte(readyScreen), nil
			case len(args) > 0 && args[0] == "run-shell":
				if !reflect.DeepEqual(args, []string{"run-shell", "-t", "%8", "kill -WINCH #{pane_pid}"}) {
					t.Fatalf("unexpected redraw args: %#v", args)
				}
				redraws++
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

	session := newTmuxCodexSession(readyFixture.Workdir, 0)
	session.pane = pane
	session.diag = newTmuxSessionDiagnostics(diagnostics.New(workspace), "codex", "codex", nil, workspace, time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.waitForIdleTransition(ctx, 0, 0); err != nil {
		t.Fatalf("waitForIdleTransition returned error: %v", err)
	}
	if redraws == 0 {
		t.Fatal("expected stale pane polling to request a redraw")
	}
}

func TestWaitForTmuxIdleFallsBackWhenPaneActivityStalls(t *testing.T) {
	workspace := t.TempDir()
	busyFixture, workingScreen := readCodexDetectorFixture(t, "working-screen")

	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%9\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				return []byte(busyFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(workingScreen), nil
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

	session := newTmuxCodexSession(busyFixture.Workdir, 75*time.Millisecond)
	session.pane = pane
	session.diag = newTmuxSessionDiagnostics(diagnostics.New(workspace), "codex", "codex", nil, workspace, time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := session.waitForIdleTransition(ctx, 0, 0); err != nil {
		t.Fatalf("waitForIdleTransition returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("fallback fired too quickly (%s); want >= 50ms", elapsed)
	}
	if session.currentState != "idle" {
		t.Fatalf("expected fallback to record idle state, got %q", session.currentState)
	}
}

func TestWaitForTmuxIdleStaysBlockedWhenFallbackDisabled(t *testing.T) {
	workspace := t.TempDir()
	busyFixture, workingScreen := readCodexDetectorFixture(t, "working-screen")

	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, _ string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%10\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				return []byte(busyFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(workingScreen), nil
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

	session := newTmuxCodexSession(busyFixture.Workdir, 0)
	session.pane = pane
	session.diag = newTmuxSessionDiagnostics(diagnostics.New(workspace), "codex", "codex", nil, workspace, time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = session.waitForIdleTransition(ctx, 0, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded when fallback is disabled, got %v", err)
	}
}

func TestTmuxCodexClassifiesPathStyleIdleTitle(t *testing.T) {
	session := newTmuxCodexSession("/tmp/worktrees/001-render-api-coordinate-contract-abcdef123456", 0)

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

	session := newTmuxCodexSession("/tmp/worktrees/001-render-api-coordinate-contract-abcdef123456", 0)
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

	session := newTmuxCodexSession("/tmp/worktrees/001-render-api-coordinate-contract-51448272af5", 0)
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

	session := newTmuxCodexSession("/tmp/vibedrive", 0)
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

	session := newTmuxCodexSession("/tmp/worktrees/001-data-fixtures-and-asset-51448272af5", 0)
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

// TestTmuxCodexSnapshotPrefersBusyTitleOverWelcomeBannerIdle reproduces the
// ECSNet failure where Codex was actively working on a prompt but a single
// pane capture happened to land mid-redraw without the "Working ... interrupt"
// line. The welcome banner was still in scrollback so codexScreenState
// reported idle, while the pane title's braille spinner correctly indicated
// busy. The runner trusted the screen, declared the prompt complete, and tried
// to submit a follow-up which Codex (still busy) refused. The snapshot must
// resolve that conflict in favor of the busy title.
func TestTmuxCodexSnapshotPrefersBusyTitleOverWelcomeBannerIdle(t *testing.T) {
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
				return []byte("⠏ ECSNet\n"), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(`
╭───────────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.129.0)                    │
│                                               │
│ model:       gpt-5.5 xhigh   /model to change │
│ directory:   ~/workspace/ECSNet               │
│ permissions: YOLO mode                        │
╰───────────────────────────────────────────────╯
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

	session := newTmuxCodexSession("/home/david/workspace/ECSNet", 0)
	session.pane = pane
	session.currentState = "busy"
	session.idleTransitions = 1
	session.busyTransitions = 2

	snapshot, err := session.snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot returned error: %v", err)
	}
	if snapshot.observedState != "busy" || snapshot.observedSource != "title" || !snapshot.classified {
		t.Fatalf("expected busy spinner title to override welcome-banner idle, got %#v", snapshot)
	}
	if snapshot.currentState != "busy" {
		t.Fatalf("expected currentState to remain busy, got %q", snapshot.currentState)
	}
	if snapshot.idleTransitions != 1 {
		t.Fatalf("expected no idle transition to be recorded, got %d", snapshot.idleTransitions)
	}
}

func TestTmuxCodexSubmitRetriesWhenFixtureScreenStillAcceptsInput(t *testing.T) {
	workspace := t.TempDir()
	readyFixture, readyScreen := readCodexDetectorFixture(t, "ready-screen")
	workingFixture, workingScreen := readCodexDetectorFixture(t, "working-screen")

	enterCount := 0
	workingScreenConsumed := false
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%4\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				if enterCount >= 2 && !workingScreenConsumed {
					return []byte(workingFixture.Title + "\n"), nil
				}
				return []byte(readyFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte(readyScreen), nil
			case len(args) > 0 && args[0] == "capture-pane":
				if enterCount >= 2 && !workingScreenConsumed {
					workingScreenConsumed = true
					return []byte(workingScreen), nil
				}
				return []byte(readyScreen), nil
			case len(args) > 0 && args[0] == "load-buffer":
				if stdin == "" {
					t.Fatal("expected prompt payload to be pasted")
				}
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				if tmuxArgsContain(args, "Enter") {
					enterCount++
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

	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}
	session := newTmuxCodexSession(readyFixture.Workdir, 0)
	session.pane = pane
	session.diag = newTmuxSessionDiagnostics(diagnostics.New(workspace), "codex", "codex", nil, workspace, 30*time.Second)
	session.diag.submitRetryInterval = 10 * time.Millisecond

	if err := session.submitPrompt(context.Background(), "retry me", session.busyTransitions); err != nil {
		t.Fatalf("submitPrompt returned error: %v", err)
	}
	if enterCount != 2 {
		t.Fatalf("expected one classified-idle retry and then acceptance, got %d enter presses", enterCount)
	}
}

func TestTmuxCodexSendPromptWaitsForReadyBeforePasting(t *testing.T) {
	workspace := t.TempDir()
	readyFixture, readyScreen := readCodexDetectorFixture(t, "ready-screen")
	busyFixture, workingScreen := readCodexDetectorFixture(t, "working-screen")
	_, unknownScreen := readCodexDetectorFixture(t, "stuck-no-submit")

	titleCallsBeforePaste := 0
	titleCallsAtPaste := 0
	enterCount := 0
	afterEnterCaptures := 0
	pasted := false
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%9\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				if !pasted {
					titleCallsBeforePaste++
					if titleCallsBeforePaste == 1 {
						return []byte(busyFixture.Title + "\n"), nil
					}
					return []byte(readyFixture.Title + "\n"), nil
				}
				if enterCount > 0 && afterEnterCaptures < 2 {
					return []byte(busyFixture.Title + "\n"), nil
				}
				return []byte(readyFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte(readyScreen), nil
			case len(args) > 0 && args[0] == "capture-pane":
				if !pasted {
					return []byte(unknownScreen), nil
				}
				if enterCount > 0 {
					afterEnterCaptures++
					if afterEnterCaptures == 1 {
						return []byte(workingScreen), nil
					}
					return []byte(readyScreen), nil
				}
				return []byte(unknownScreen), nil
			case len(args) > 0 && args[0] == "load-buffer":
				if stdin == "" {
					t.Fatal("expected prompt payload to be pasted")
				}
				titleCallsAtPaste = titleCallsBeforePaste
				pasted = true
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				if tmuxArgsContain(args, "Enter") {
					enterCount++
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

	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}
	session := newTmuxCodexSession(readyFixture.Workdir, 0)
	session.pane = pane
	session.diag = newTmuxSessionDiagnostics(diagnostics.New(workspace), "codex", "codex", nil, workspace, 30*time.Second)
	session.diag.submitRetryInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.sendPrompt(ctx, "handle transient startup title"); err != nil {
		t.Fatalf("sendPrompt returned error: %v", err)
	}
	if titleCallsAtPaste < 2 {
		t.Fatalf("expected sendPrompt to wait for an idle title before pasting, title calls at paste = %d", titleCallsAtPaste)
	}
	if enterCount != 1 {
		t.Fatalf("expected one enter press after prompt paste, got %d", enterCount)
	}
}

func TestTmuxCodexSendPromptUsesStartupTimeoutWhileWaitingForReady(t *testing.T) {
	workspace := t.TempDir()
	readyFixture, readyScreen := readCodexDetectorFixture(t, "ready-screen")
	busyFixture, workingScreen := readCodexDetectorFixture(t, "working-screen")

	titleCallsBeforePaste := 0
	pasted := false
	enterCount := 0
	afterEnterCaptures := 0
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%10\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				if !pasted {
					titleCallsBeforePaste++
					if titleCallsBeforePaste <= 2 {
						return []byte(busyFixture.Title + "\n"), nil
					}
					return []byte(readyFixture.Title + "\n"), nil
				}
				if enterCount > 0 && afterEnterCaptures < 2 {
					return []byte(busyFixture.Title + "\n"), nil
				}
				return []byte(readyFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte(readyScreen), nil
			case len(args) > 0 && args[0] == "capture-pane":
				if !pasted {
					if titleCallsBeforePaste > 2 {
						return []byte(readyScreen), nil
					}
					return []byte(workingScreen), nil
				}
				if enterCount > 0 {
					afterEnterCaptures++
					if afterEnterCaptures == 1 {
						return []byte(workingScreen), nil
					}
					return []byte(readyScreen), nil
				}
				return []byte(readyScreen), nil
			case len(args) > 0 && args[0] == "load-buffer":
				if stdin == "" {
					t.Fatal("expected prompt payload to be pasted")
				}
				pasted = true
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				if tmuxArgsContain(args, "Enter") {
					enterCount++
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

	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}
	session := newTmuxCodexSession(readyFixture.Workdir, 0)
	session.pane = pane
	session.diag = newTmuxSessionDiagnostics(diagnostics.New(workspace), "codex", "codex", nil, workspace, 250*time.Millisecond)
	session.diag.submitRetryInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.sendPrompt(ctx, "wait through codex startup busy state"); err != nil {
		t.Fatalf("sendPrompt returned error: %v", err)
	}
	if titleCallsBeforePaste < 3 {
		t.Fatalf("expected sendPrompt to wait past submit retry interval, title calls before paste = %d", titleCallsBeforePaste)
	}
	if enterCount != 1 {
		t.Fatalf("expected one enter press after prompt paste, got %d", enterCount)
	}
}

func TestTmuxCodexSubmitAbortsOnUnclassifiedFixtureScreen(t *testing.T) {
	workspace := t.TempDir()
	stuckFixture, stuckScreen := readCodexDetectorFixture(t, "stuck-no-submit")

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
				return []byte("%5\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				return []byte(stuckFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte(stuckScreen), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(stuckScreen), nil
			case len(args) > 0 && args[0] == "load-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				if tmuxArgsContain(args, "Enter") {
					enterCount++
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

	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "codex",
		Command: "codex",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}
	session := newTmuxCodexSession(stuckFixture.Workdir, 0)
	session.pane = pane
	session.diag = newTmuxSessionDiagnostics(diagnostics.New(workspace), "codex", "codex", nil, workspace, 30*time.Second)
	session.diag.submitRetryInterval = 10 * time.Millisecond

	ctx := withTmuxDiagnosticsIdentity(context.Background(), "run-unknown", "task-unknown", "execute")
	err = session.submitPrompt(ctx, "please do work", session.busyTransitions)
	if err == nil {
		t.Fatal("expected submitPrompt to fail on unclassified screen")
	}
	err = session.withDiagnostics(ctx, tmuxPromptFailurePath(err), err)
	if enterCount != 1 {
		t.Fatalf("expected fail-fast after one enter press, got %d", enterCount)
	}
	if !strings.Contains(err.Error(), `unclassified TUI state "unknown"`) {
		t.Fatalf("expected error to name the unknown state, got %v", err)
	}
	dir := filepath.Join(workspace, ".vibedrive", "debug", "run-unknown", "task-unknown", "execute")
	if !strings.Contains(err.Error(), "tmux diagnostics captured at "+dir) {
		t.Fatalf("expected diagnostics path in error, got %v", err)
	}

	manifest := assertTmuxDiagnosticsBundle(t, dir, tmuxFailureSubmitUnknownState, "codex", "What is 20 + 21?")
	metadata := readTextFile(t, filepath.Join(dir, "tmux", "metadata.json"))
	if !strings.Contains(metadata, `"state": "unknown"`) || !strings.Contains(metadata, `"classified": false`) {
		t.Fatalf("expected metadata to record the unclassified submit state:\n%s", metadata)
	}
	if entry := diagnosticsArtifact(t, manifest, diagnostics.ArtifactPromptRaw); entry.Status != diagnostics.ArtifactStatusWritten {
		t.Fatalf("prompt artifact status = %q, want written", entry.Status)
	}
}

func TestTmuxClaudeSubmitRetriesWhenFixtureTitleStillAcceptsInput(t *testing.T) {
	workspace := t.TempDir()
	idleFixture, idleScreen := readClaudeDetectorFixture(t, "idle-title")
	busyFixture, busyScreen := readClaudeDetectorFixture(t, "busy-title")

	enterCount := 0
	busyScreenConsumed := false
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%6\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				if enterCount >= 2 && !busyScreenConsumed {
					return []byte(busyFixture.Title + "\n"), nil
				}
				return []byte(idleFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte(idleScreen), nil
			case len(args) > 0 && args[0] == "capture-pane":
				if enterCount >= 2 && !busyScreenConsumed {
					busyScreenConsumed = true
					return []byte(busyScreen), nil
				}
				return []byte(idleScreen), nil
			case len(args) > 0 && args[0] == "load-buffer":
				if stdin == "" {
					t.Fatal("expected prompt payload to be pasted")
				}
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				if tmuxArgsContain(args, "Enter") {
					enterCount++
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
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "claude",
		Command: "claude",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}
	session := newTmuxClaudeSession(diagnostics.New(workspace), "claude", nil, workspace, time.Second, 0)
	session.pane = pane
	session.diag.submitRetryInterval = 10 * time.Millisecond

	if err := session.submitPrompt(context.Background(), "retry me", session.busyTransitions, false); err != nil {
		t.Fatalf("submitPrompt returned error: %v", err)
	}
	if enterCount != 2 {
		t.Fatalf("expected one classified-idle retry and then acceptance, got %d enter presses", enterCount)
	}
}

func TestTmuxClaudeSubmitAbortsOnBusyFixtureScreen(t *testing.T) {
	workspace := t.TempDir()
	ambiguousFixture, ambiguousScreen := readClaudeDetectorFixture(t, "ambiguous-permission-error")

	enterCount := 0
	controller := tmuxagent.NewController(tmuxagent.Options{
		Command: "tmux",
		Run: func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session":
				return nil, nil
			case len(args) > 0 && args[0] == "new-window":
				return []byte("%7\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_title}"):
				return []byte(ambiguousFixture.Title + "\n"), nil
			case len(args) > 0 && args[0] == "display-message" && tmuxArgsContain(args, "#{pane_dead}"):
				return []byte("0\n"), nil
			case len(args) > 0 && args[0] == "capture-pane" && tmuxArgsContain(args, "-3000"):
				return []byte(ambiguousScreen), nil
			case len(args) > 0 && args[0] == "capture-pane":
				return []byte(ambiguousScreen), nil
			case len(args) > 0 && args[0] == "load-buffer":
				if stdin == "" {
					t.Fatal("expected prompt payload to be pasted")
				}
				return nil, nil
			case len(args) > 0 && args[0] == "paste-buffer":
				return nil, nil
			case len(args) > 0 && args[0] == "send-keys":
				if tmuxArgsContain(args, "Enter") {
					enterCount++
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
	pane, err := controller.NewPane(context.Background(), tmuxagent.PaneSpec{
		Name:    "task",
		Agent:   "claude",
		Command: "claude",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}
	session := newTmuxClaudeSession(diagnostics.New(workspace), "claude", nil, workspace, time.Second, 0)
	session.pane = pane
	session.currentState = "busy"
	session.busyTransitions = 1
	session.recordTitleEvent("transport", "", session.currentState)
	session.diag.submitRetryInterval = 10 * time.Millisecond

	ctx := withTmuxDiagnosticsIdentity(context.Background(), "run-claude", "task-claude", "review")
	submitErr := session.submitPrompt(ctx, "please review", session.busyTransitions, false)
	if submitErr == nil {
		t.Fatal("expected submitPrompt to fail on busy fixture screen")
	}
	if enterCount != 1 {
		t.Fatalf("expected fail-fast after one enter press, got %d", enterCount)
	}
	var stateErr *tmuxSubmitStateError
	if !errors.As(submitErr, &stateErr) {
		t.Fatalf("expected submit error to wrap tmuxSubmitStateError, got %T %v", submitErr, submitErr)
	}
	if stateErr.agent != "claude" || stateErr.state != "busy" {
		t.Fatalf("unexpected submit state error: %#v", stateErr)
	}

	err = session.withDiagnostics(ctx, tmuxPromptFailurePath(submitErr), submitErr)
	if !errors.As(err, &stateErr) {
		t.Fatalf("expected diagnostic error to wrap tmuxSubmitStateError, got %T %v", err, err)
	}
	dir := filepath.Join(workspace, ".vibedrive", "debug", "run-claude", "task-claude", "review")
	if !strings.Contains(err.Error(), "tmux diagnostics captured at "+dir) {
		t.Fatalf("expected diagnostics path in error, got %v", err)
	}

	manifest := assertTmuxDiagnosticsBundle(t, dir, tmuxFailureSubmitUnknownState, "claude", "argument 'impossible' is invalid")
	if entry := diagnosticsArtifact(t, manifest, diagnostics.ArtifactPromptRaw); entry.Status != diagnostics.ArtifactStatusWritten {
		t.Fatalf("prompt artifact status = %q, want written", entry.Status)
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
	client, err := newTmuxCodexClient("codex", nil, workspace, "", "", controller, "task")
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
	client, err := newTmuxClaudeClient("claude", nil, workspace, "1s", "", controller, "task")
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
	client, err := newTmuxCodexClient("codex", nil, workspace, "1ms", "", controller, "task")
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

type codexDetectorFixture struct {
	Workdir    string `json:"workdir"`
	Title      string `json:"title"`
	ScreenFile string `json:"screen_file"`
}

func readCodexDetectorFixture(t *testing.T, name string) (codexDetectorFixture, string) {
	t.Helper()
	fixtureDir := filepath.Join("..", "..", "pkg", "agentcli", "codex", "testdata", "screens")
	fixturePath := filepath.Join(fixtureDir, name+".json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read Codex fixture %s: %v", fixturePath, err)
	}
	var fixture codexDetectorFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse Codex fixture %s: %v", fixturePath, err)
	}
	if fixture.Workdir == "" || fixture.Title == "" || fixture.ScreenFile == "" {
		t.Fatalf("Codex fixture %s is missing required fields: %#v", fixturePath, fixture)
	}
	screenPath := filepath.Join(fixtureDir, fixture.ScreenFile)
	screen, err := os.ReadFile(screenPath)
	if err != nil {
		t.Fatalf("read Codex fixture screen %s: %v", screenPath, err)
	}
	return fixture, string(screen)
}

type claudeDetectorFixture struct {
	Workdir    string `json:"workdir"`
	Title      string `json:"title"`
	ScreenFile string `json:"screen_file"`
}

func readClaudeDetectorFixture(t *testing.T, name string) (claudeDetectorFixture, string) {
	t.Helper()
	fixtureDir := filepath.Join("..", "..", "pkg", "agentcli", "claude", "testdata", "screens")
	fixturePath := filepath.Join(fixtureDir, name+".json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read Claude fixture %s: %v", fixturePath, err)
	}
	var fixture claudeDetectorFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse Claude fixture %s: %v", fixturePath, err)
	}
	if fixture.Workdir == "" || fixture.Title == "" || fixture.ScreenFile == "" {
		t.Fatalf("Claude fixture %s is missing required fields: %#v", fixturePath, fixture)
	}
	screenPath := filepath.Join(fixtureDir, fixture.ScreenFile)
	screen, err := os.ReadFile(screenPath)
	if err != nil {
		t.Fatalf("read Claude fixture screen %s: %v", screenPath, err)
	}
	return fixture, string(screen)
}

func tmuxArgsContain(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
