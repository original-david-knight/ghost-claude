package tmuxagent

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type tmuxCall struct {
	command string
	args    []string
	stdin   string
}

type fakeTmux struct {
	calls      []tmuxCall
	nextPaneID int
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
	case len(args) > 0 && args[0] == "new-session" && containsOrdered(args, []string{"-P", "-F", "#{pane_id}"}):
		return f.nextPane(), nil
	case len(args) > 0 && args[0] == "new-window":
		return f.nextPane(), nil
	case len(args) > 0 && args[0] == "split-window":
		return f.nextPane(), nil
	case len(args) > 0 && args[0] == "list-panes":
		return []byte("%1\n%2\n"), nil
	case len(args) > 0 && args[0] == "display-message":
		if args[len(args)-1] == "#{window_width}" {
			return []byte("120\n"), nil
		}
		return []byte("0\n"), nil
	default:
		return nil, nil
	}
}

func (f *fakeTmux) nextPane() []byte {
	f.nextPaneID++
	return []byte("%" + strconv.Itoa(f.nextPaneID) + "\n")
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
	if pane.Target != "%1" {
		t.Fatalf("expected pane target %%1, got %q", pane.Target)
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

func TestControllerDashboardStacksAgentPanesBesideStatus(t *testing.T) {
	fake := &fakeTmux{}
	controller := NewController(Options{
		Command:       "tmux",
		SessionName:   "vibedrive test",
		Run:           fake.run,
		StatusCommand: "sh",
		StatusArgs:    []string{"-lc", "watch vibedrive"},
		StatusWorkdir: "/repo",
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})

	first, err := controller.NewPane(context.Background(), PaneSpec{
		Name:    "api",
		Agent:   "codex",
		Command: "codex",
		Workdir: "/repo",
	})
	if err != nil {
		t.Fatalf("first NewPane returned error: %v", err)
	}
	second, err := controller.NewPane(context.Background(), PaneSpec{
		Name:    "ui",
		Agent:   "claude",
		Command: "claude",
		Workdir: "/repo",
	})
	if err != nil {
		t.Fatalf("second NewPane returned error: %v", err)
	}
	if first.Target != "%2" || second.Target != "%3" {
		t.Fatalf("unexpected pane targets: first=%q second=%q", first.Target, second.Target)
	}

	if len(fake.calls) < 11 {
		t.Fatalf("expected version, session, splits, and layouts, got %#v", fake.calls)
	}
	sessionArgs := fake.calls[1].args
	if !containsOrdered(sessionArgs, []string{"new-session", "-d", "-s", "vibedrive-test", "-n", "vibedrive", "-P", "-F", "#{pane_id}", "-c", "/repo"}) {
		t.Fatalf("unexpected dashboard session args: %#v", sessionArgs)
	}
	if sessionArgs[len(sessionArgs)-1] != "exec 'sh' '-lc' 'watch vibedrive'" {
		t.Fatalf("unexpected status command arg %q", sessionArgs[len(sessionArgs)-1])
	}

	firstSplit := fake.calls[2].args
	if !containsOrdered(firstSplit, []string{"split-window", "-d", "-P", "-F", "#{pane_id}", "-h", "-t", "%1", "-c", "/repo"}) {
		t.Fatalf("expected first agent pane to split right of status pane, got %#v", firstSplit)
	}
	secondSplit := fake.calls[7].args
	if !containsOrdered(secondSplit, []string{"split-window", "-d", "-P", "-F", "#{pane_id}", "-v", "-t", "%2", "-c", "/repo"}) {
		t.Fatalf("expected second agent pane to split below first agent pane, got %#v", secondSplit)
	}
	for _, callIndex := range []int{3, 8} {
		if !reflect.DeepEqual(fake.calls[callIndex].args, []string{"select-layout", "-t", "vibedrive-test:vibedrive", "main-vertical"}) {
			t.Fatalf("expected main-vertical layout call, got %#v", fake.calls[callIndex].args)
		}
	}
	for _, callIndex := range []int{5, 10} {
		if !reflect.DeepEqual(fake.calls[callIndex].args, []string{"resize-pane", "-t", "%1", "-x", "40"}) {
			t.Fatalf("expected status pane resize to one third width, got %#v", fake.calls[callIndex].args)
		}
	}
}

func TestControllerDashboardFallsBackWhenAgentPaneDisappears(t *testing.T) {
	var calls []tmuxCall
	nextPaneID := 0
	sawMissingAgentSplit := false
	controller := NewController(Options{
		Command:       "tmux",
		SessionName:   "vibedrive test",
		StatusCommand: "sh",
		StatusArgs:    []string{"-lc", "watch vibedrive"},
		StatusWorkdir: "/repo",
		Run: func(_ context.Context, command string, args []string, stdin string) ([]byte, error) {
			calls = append(calls, tmuxCall{
				command: command,
				args:    append([]string{}, args...),
				stdin:   stdin,
			})
			switch {
			case len(args) == 1 && args[0] == "-V":
				return []byte("tmux 3.4\n"), nil
			case len(args) > 0 && args[0] == "new-session" && containsOrdered(args, []string{"-P", "-F", "#{pane_id}"}):
				nextPaneID++
				return []byte("%" + strconv.Itoa(nextPaneID) + "\n"), nil
			case len(args) > 0 && args[0] == "split-window" && containsOrdered(args, []string{"-v", "-t", "%2"}):
				sawMissingAgentSplit = true
				return []byte("can't find pane: %2\n"), errors.New("exit status 1")
			case len(args) > 0 && args[0] == "split-window":
				nextPaneID++
				return []byte("%" + strconv.Itoa(nextPaneID) + "\n"), nil
			case len(args) > 0 && args[0] == "display-message":
				if args[len(args)-1] == "#{window_width}" {
					return []byte("120\n"), nil
				}
				return []byte("0\n"), nil
			default:
				return nil, nil
			}
		},
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})

	first, err := controller.NewPane(context.Background(), PaneSpec{
		Name:    "api",
		Agent:   "codex",
		Command: "codex",
		Workdir: "/repo",
	})
	if err != nil {
		t.Fatalf("first NewPane returned error: %v", err)
	}
	second, err := controller.NewPane(context.Background(), PaneSpec{
		Name:    "ui",
		Agent:   "claude",
		Command: "claude",
		Workdir: "/repo",
	})
	if err != nil {
		t.Fatalf("second NewPane returned error: %v", err)
	}
	if first.Target != "%2" || second.Target != "%3" {
		t.Fatalf("unexpected pane targets: first=%q second=%q", first.Target, second.Target)
	}
	if !sawMissingAgentSplit {
		t.Fatal("expected test to exercise missing prior agent pane")
	}

	foundFallback := false
	for _, call := range calls {
		if containsOrdered(call.args, []string{"split-window", "-d", "-P", "-F", "#{pane_id}", "-h", "-t", "%1", "-c", "/repo"}) {
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Fatalf("expected fallback split from status pane after stale agent target, got %#v", calls)
	}
}

func TestIsTargetMissingErrorRecognizesOnlyTmuxTargets(t *testing.T) {
	if !IsTargetMissingError(errors.New("can't find pane: %23")) {
		t.Fatal("expected pane lookup failure to be recognized")
	}
	if IsTargetMissingError(errors.New("tmux: command not found")) {
		t.Fatal("expected unrelated not found error to remain visible")
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

func TestPaneRequestRedrawSignalsPaneProcess(t *testing.T) {
	fake := &fakeTmux{}
	controller := NewController(Options{
		SessionName: "vibedrive",
		Run:         fake.run,
		LookPath: func(string) (string, error) {
			return "/usr/bin/tmux", nil
		},
	})
	pane := &Pane{controller: controller, Target: "%9"}

	if err := pane.RequestRedraw(context.Background()); err != nil {
		t.Fatalf("RequestRedraw returned error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected one run-shell call, got %#v", fake.calls)
	}
	if !reflect.DeepEqual(fake.calls[0].args, []string{"run-shell", "-t", "%9", "kill -WINCH #{pane_pid}"}) {
		t.Fatalf("unexpected redraw args: %#v", fake.calls[0].args)
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
	if !strings.Contains(err.Error(), "tmux is required for vibedrive TUI execution") {
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
