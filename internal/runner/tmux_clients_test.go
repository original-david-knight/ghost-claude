package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vibedrive/internal/tmuxagent"
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
