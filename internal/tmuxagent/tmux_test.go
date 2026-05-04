package tmuxagent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type tmuxCall struct {
	command string
	args    []string
	stdin   string
}

type fakeTmux struct {
	calls []tmuxCall
}

func (f *fakeTmux) run(_ context.Context, command string, args []string, stdin string) ([]byte, error) {
	f.calls = append(f.calls, tmuxCall{
		command: command,
		args:    append([]string{}, args...),
		stdin:   stdin,
	})
	switch {
	case len(args) == 1 && args[0] == "-V":
		return []byte("tmux 3.4\n"), nil
	case len(args) > 0 && args[0] == "new-window":
		return []byte("%42\n"), nil
	case len(args) > 0 && args[0] == "display-message":
		return []byte("0\n"), nil
	default:
		return nil, nil
	}
}

func TestRunSessionNameIsDeterministicAndSanitized(t *testing.T) {
	got := RunSessionName("/tmp/work space", "/tmp/work space/vibedrive-plan.yaml", 123)
	again := RunSessionName("/tmp/work space", "/tmp/work space/vibedrive-plan.yaml", 123)

	if got != again {
		t.Fatalf("expected deterministic session name, got %q then %q", got, again)
	}
	if !strings.HasPrefix(got, "vibedrive-123-") {
		t.Fatalf("unexpected session name %q", got)
	}
	if strings.Contains(got, " ") {
		t.Fatalf("expected sanitized session name, got %q", got)
	}
}

func TestWindowNameIncludesSequenceTaskAndAgent(t *testing.T) {
	got := WindowName("007-api/db-abcdef123456", "codex", 3)
	if got != "003-007-api-db-abcdef123456-codex" {
		t.Fatalf("unexpected window name %q", got)
	}
}

func TestShellCommandQuotesArgs(t *testing.T) {
	got := ShellCommand("codex", []string{"--profile", "team one", "it's ok"})
	want := "exec 'codex' '--profile' 'team one' 'it'\"'\"'s ok'"
	if got != want {
		t.Fatalf("ShellCommand = %q, want %q", got, want)
	}
}

func TestControllerStartsSessionAndCreatesWindow(t *testing.T) {
	fake := &fakeTmux{}
	controller := NewController(Options{
		Command:     "tmux",
		SessionName: "vibedrive test",
		Run:         fake.run,
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})

	pane, err := controller.NewPane(context.Background(), PaneSpec{
		Name:    "api/task",
		Agent:   "codex",
		Command: "codex",
		Args:    []string{"--profile", "team one"},
		Workdir: "/repo/worktree",
	})
	if err != nil {
		t.Fatalf("NewPane returned error: %v", err)
	}
	if pane.Target != "%42" {
		t.Fatalf("expected pane target %%42, got %q", pane.Target)
	}

	if len(fake.calls) < 3 {
		t.Fatalf("expected tmux version, session, and window calls, got %#v", fake.calls)
	}
	if !reflect.DeepEqual(fake.calls[1].args[:4], []string{"new-session", "-d", "-s", "vibedrive-test"}) {
		t.Fatalf("unexpected new-session args: %#v", fake.calls[1].args)
	}
	windowArgs := fake.calls[2].args
	if !containsOrdered(windowArgs, []string{"new-window", "-d", "-P", "-F", "#{pane_id}"}) {
		t.Fatalf("unexpected new-window args: %#v", windowArgs)
	}
	if !containsOrdered(windowArgs, []string{"-n", "001-api-task-codex", "-c", "/repo/worktree"}) {
		t.Fatalf("expected name and cwd in new-window args: %#v", windowArgs)
	}
	if windowArgs[len(windowArgs)-1] != "exec 'codex' '--profile' 'team one'" {
		t.Fatalf("unexpected shell command arg %q", windowArgs[len(windowArgs)-1])
	}
}

func TestPanePasteLoadsAndPastesBuffer(t *testing.T) {
	fake := &fakeTmux{}
	controller := NewController(Options{
		SessionName: "vibedrive",
		Run:         fake.run,
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane := &Pane{controller: controller, Target: "%1"}

	if err := pane.Paste(context.Background(), "line 1\nline 2"); err != nil {
		t.Fatalf("Paste returned error: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("expected load-buffer and paste-buffer calls, got %#v", fake.calls)
	}
	if !reflect.DeepEqual(fake.calls[0].args, []string{"load-buffer", "-b", "vibedrive-1", "-"}) {
		t.Fatalf("unexpected load-buffer args: %#v", fake.calls[0].args)
	}
	if fake.calls[0].stdin != "line 1\nline 2" {
		t.Fatalf("unexpected paste stdin %q", fake.calls[0].stdin)
	}
	if !reflect.DeepEqual(fake.calls[1].args, []string{"paste-buffer", "-d", "-b", "vibedrive-1", "-t", "%1"}) {
		t.Fatalf("unexpected paste-buffer args: %#v", fake.calls[1].args)
	}
}

func TestControllerReportsMissingTmux(t *testing.T) {
	controller := NewController(Options{
		Run: func(context.Context, string, []string, string) ([]byte, error) {
			t.Fatal("expected LookPath failure to stop before running tmux")
			return nil, nil
		},
		LookPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
	})

	err := controller.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to fail")
	}
	if !strings.Contains(err.Error(), "tmux is required for parallel TUI execution") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func containsOrdered(values, want []string) bool {
	if len(want) == 0 {
		return true
	}
	index := 0
	for _, value := range values {
		if value == want[index] {
			index++
			if index == len(want) {
				return true
			}
		}
	}
	return false
}
