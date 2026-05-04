package tmuxagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const defaultCommand = "tmux"

type CommandRunner func(ctx context.Context, command string, args []string, stdin string) ([]byte, error)

type Options struct {
	Command     string
	SessionName string
	Stderr      io.Writer
	Run         CommandRunner
	LookPath    func(string) (string, error)
}

type Controller struct {
	command     string
	sessionName string
	stderr      io.Writer
	run         CommandRunner
	lookPath    func(string) (string, error)

	mu            sync.Mutex
	started       bool
	windowCounter int
	bufferCounter int
}

type PaneSpec struct {
	Name    string
	Agent   string
	Command string
	Args    []string
	Workdir string
}

type Pane struct {
	controller *Controller
	Target     string
	WindowName string
}

func NewController(opts Options) *Controller {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		command = defaultCommand
	}
	run := opts.Run
	if run == nil {
		run = defaultRun
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	return &Controller{
		command:     command,
		sessionName: sanitizeName(opts.SessionName, "vibedrive"),
		stderr:      opts.Stderr,
		run:         run,
		lookPath:    lookPath,
	}
}

func RunSessionName(workspace, planFile string, pid int) string {
	if pid < 1 {
		pid = os.Getpid()
	}
	sum := sha256.Sum256([]byte(filepath.Clean(workspace) + "\x00" + filepath.Clean(planFile)))
	return sanitizeName(fmt.Sprintf("vibedrive-%d-%s", pid, hex.EncodeToString(sum[:])[:10]), "vibedrive")
}

func WindowName(taskName, agent string, sequence int) string {
	taskName = sanitizeName(taskName, "task")
	agent = sanitizeName(agent, "agent")
	if sequence < 1 {
		sequence = 1
	}
	name := fmt.Sprintf("%03d-%s-%s", sequence, taskName, agent)
	if len(name) > 80 {
		name = name[:80]
	}
	return strings.TrimRight(name, "-")
}

func ShellCommand(command string, args []string) string {
	parts := []string{"exec", shellQuote(command)}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func (c *Controller) SessionName() string {
	if c == nil {
		return ""
	}
	return c.sessionName
}

func (c *Controller) AttachCommand() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s attach-session -t %s", c.command, shellQuote(c.sessionName))
}

func (c *Controller) Start(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("tmux controller is nil")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	if err := c.checkAvailable(); err != nil {
		return err
	}

	args := []string{"new-session", "-d", "-s", c.sessionName, "-n", "vibedrive", "sleep 31536000"}
	output, err := c.run(ctx, c.command, args, "")
	if err != nil {
		message := strings.TrimSpace(string(output))
		if strings.Contains(strings.ToLower(message), "duplicate session") {
			c.started = true
			return nil
		}
		return fmt.Errorf("create tmux session %s: %w: %s", c.sessionName, err, message)
	}
	c.started = true
	return nil
}

func (c *Controller) NewPane(ctx context.Context, spec PaneSpec) (*Pane, error) {
	if c == nil {
		return nil, fmt.Errorf("tmux controller is nil")
	}
	if err := c.Start(ctx); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.windowCounter++
	windowName := WindowName(spec.Name, spec.Agent, c.windowCounter)
	c.mu.Unlock()

	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", c.sessionName + ":", "-n", windowName}
	if strings.TrimSpace(spec.Workdir) != "" {
		args = append(args, "-c", spec.Workdir)
	}
	args = append(args, ShellCommand(spec.Command, spec.Args))

	output, err := c.run(ctx, c.command, args, "")
	if err != nil {
		return nil, fmt.Errorf("create tmux window %s: %w: %s", windowName, err, strings.TrimSpace(string(output)))
	}
	target := strings.TrimSpace(string(output))
	if target == "" {
		return nil, fmt.Errorf("create tmux window %s: tmux did not return a pane id", windowName)
	}
	return &Pane{controller: c, Target: target, WindowName: windowName}, nil
}

func (c *Controller) Kill(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	started := c.started
	c.started = false
	c.mu.Unlock()
	if !started {
		return nil
	}
	output, err := c.run(ctx, c.command, []string{"kill-session", "-t", c.sessionName}, "")
	if err != nil && !strings.Contains(strings.ToLower(string(output)), "can't find session") {
		return fmt.Errorf("kill tmux session %s: %w: %s", c.sessionName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p *Pane) Paste(ctx context.Context, payload string) error {
	if p == nil || p.controller == nil {
		return fmt.Errorf("tmux pane is nil")
	}
	c := p.controller
	c.mu.Lock()
	c.bufferCounter++
	bufferName := sanitizeName(c.sessionName+"-"+strconv.Itoa(c.bufferCounter), "vibedrive-buffer")
	c.mu.Unlock()

	if output, err := c.run(ctx, c.command, []string{"load-buffer", "-b", bufferName, "-"}, payload); err != nil {
		return fmt.Errorf("load tmux paste buffer: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := c.run(ctx, c.command, []string{"paste-buffer", "-d", "-b", bufferName, "-t", p.Target}, ""); err != nil {
		return fmt.Errorf("paste tmux buffer into %s: %w: %s", p.Target, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p *Pane) SendEnter(ctx context.Context) error {
	return p.sendKeys(ctx, "Enter")
}

func (p *Pane) SendCtrlD(ctx context.Context) error {
	return p.sendKeys(ctx, "C-d")
}

func (p *Pane) Kill(ctx context.Context) error {
	if p == nil || p.controller == nil {
		return nil
	}
	output, err := p.controller.run(ctx, p.controller.command, []string{"kill-pane", "-t", p.Target}, "")
	if err != nil && !strings.Contains(strings.ToLower(string(output)), "can't find pane") {
		return fmt.Errorf("kill tmux pane %s: %w: %s", p.Target, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p *Pane) Capture(ctx context.Context, lines int) (string, error) {
	if p == nil || p.controller == nil {
		return "", fmt.Errorf("tmux pane is nil")
	}
	if lines < 1 {
		lines = 200
	}
	args := []string{"capture-pane", "-p", "-J", "-t", p.Target, "-S", "-" + strconv.Itoa(lines)}
	output, err := p.controller.run(ctx, p.controller.command, args, "")
	if err != nil {
		return "", fmt.Errorf("capture tmux pane %s: %w: %s", p.Target, err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (p *Pane) Title(ctx context.Context) (string, error) {
	return p.display(ctx, "#{pane_title}")
}

func (p *Pane) Dead(ctx context.Context) (bool, error) {
	value, err := p.display(ctx, "#{pane_dead}")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "can't find") {
			return true, nil
		}
		return false, err
	}
	return strings.TrimSpace(value) == "1", nil
}

func (p *Pane) sendKeys(ctx context.Context, key string) error {
	if p == nil || p.controller == nil {
		return fmt.Errorf("tmux pane is nil")
	}
	output, err := p.controller.run(ctx, p.controller.command, []string{"send-keys", "-t", p.Target, key}, "")
	if err != nil {
		return fmt.Errorf("send %s to tmux pane %s: %w: %s", key, p.Target, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p *Pane) display(ctx context.Context, format string) (string, error) {
	if p == nil || p.controller == nil {
		return "", fmt.Errorf("tmux pane is nil")
	}
	output, err := p.controller.run(ctx, p.controller.command, []string{"display-message", "-p", "-t", p.Target, format}, "")
	if err != nil {
		return "", fmt.Errorf("display tmux pane %s: %w: %s", p.Target, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func (c *Controller) checkAvailable() error {
	if _, err := c.lookPath(c.command); err != nil {
		return fmt.Errorf("tmux is required for parallel TUI execution; install tmux and retry: %w", err)
	}
	output, err := c.run(context.Background(), c.command, []string{"-V"}, "")
	if err != nil {
		return fmt.Errorf("tmux is required for parallel TUI execution; %s -V failed: %w: %s", c.command, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func defaultRun(ctx context.Context, command string, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

func sanitizeName(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		isAlpha := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if isAlpha || isDigit || r == '_' || r == '.' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = fallback
	}
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
