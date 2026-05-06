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
	Command       string
	SessionName   string
	Stderr        io.Writer
	Run           CommandRunner
	LookPath      func(string) (string, error)
	StatusCommand string
	StatusArgs    []string
	StatusWorkdir string
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
	statusCommand string
	statusArgs    []string
	statusWorkdir string
	statusTarget  string
	agentTarget   string
	clientOpened  bool
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
		command:       command,
		sessionName:   sanitizeName(opts.SessionName, "vibedrive"),
		stderr:        opts.Stderr,
		run:           run,
		lookPath:      lookPath,
		statusCommand: strings.TrimSpace(opts.StatusCommand),
		statusArgs:    append([]string{}, opts.StatusArgs...),
		statusWorkdir: strings.TrimSpace(opts.StatusWorkdir),
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

func (c *Controller) OpenClient(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if c == nil {
		return fmt.Errorf("tmux controller is nil")
	}
	c.mu.Lock()
	if c.clientOpened {
		c.mu.Unlock()
		return nil
	}
	c.clientOpened = true
	command := c.command
	sessionName := c.sessionName
	c.mu.Unlock()

	args := []string{"attach-session", "-t", sessionName}
	if strings.TrimSpace(os.Getenv("TMUX")) != "" {
		args = []string{"switch-client", "-t", sessionName}
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		c.mu.Lock()
		c.clientOpened = false
		c.mu.Unlock()
		return fmt.Errorf("open tmux session %s: %w", sessionName, err)
	}
	go func() {
		_ = cmd.Wait()
	}()
	return nil
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

	args := []string{"new-session", "-d", "-s", c.sessionName, "-n", "vibedrive"}
	if c.dashboardMode() {
		args = append(args, "-P", "-F", "#{pane_id}")
		if c.statusWorkdir != "" {
			args = append(args, "-c", c.statusWorkdir)
		}
		args = append(args, ShellCommand(c.statusCommand, c.statusArgs))
	} else {
		args = append(args, "sleep 31536000")
	}
	output, err := c.run(ctx, c.command, args, "")
	if err != nil {
		message := strings.TrimSpace(string(output))
		if strings.Contains(strings.ToLower(message), "duplicate session") {
			if c.dashboardMode() {
				_ = c.loadDashboardTargetsLocked(ctx)
			}
			c.started = true
			return nil
		}
		return fmt.Errorf("create tmux session %s: %w: %s", c.sessionName, err, message)
	}
	if c.dashboardMode() {
		c.statusTarget = strings.TrimSpace(string(output))
		if c.statusTarget == "" {
			return fmt.Errorf("create tmux session %s: tmux did not return a status pane id", c.sessionName)
		}
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
	if c.dashboardMode() {
		return c.newDashboardPane(ctx, spec)
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

func (c *Controller) newDashboardPane(ctx context.Context, spec PaneSpec) (*Pane, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.statusTarget == "" {
		if err := c.loadDashboardTargetsLocked(ctx); err != nil {
			return nil, err
		}
	}
	if c.statusTarget == "" {
		return nil, fmt.Errorf("tmux dashboard status pane is unavailable")
	}
	if c.agentTarget != "" {
		alive, err := c.paneAliveLocked(ctx, c.agentTarget)
		if err != nil {
			return nil, err
		}
		if !alive {
			c.agentTarget = ""
		}
	}

	c.windowCounter++
	paneName := WindowName(spec.Name, spec.Agent, c.windowCounter)
	args := c.dashboardSplitArgsLocked(spec)

	output, err := c.run(ctx, c.command, args, "")
	if err != nil && c.agentTarget != "" && tmuxTargetMissing(output) {
		c.agentTarget = ""
		args = c.dashboardSplitArgsLocked(spec)
		output, err = c.run(ctx, c.command, args, "")
	}
	if err != nil {
		return nil, fmt.Errorf("create tmux pane %s: %w: %s", paneName, err, strings.TrimSpace(string(output)))
	}
	target := strings.TrimSpace(string(output))
	if target == "" {
		return nil, fmt.Errorf("create tmux pane %s: tmux did not return a pane id", paneName)
	}
	c.agentTarget = target
	if err := c.selectDashboardLayoutLocked(ctx); err != nil {
		return nil, err
	}
	return &Pane{controller: c, Target: target, WindowName: paneName}, nil
}

func (c *Controller) dashboardSplitArgsLocked(spec PaneSpec) []string {
	args := []string{"split-window", "-d", "-P", "-F", "#{pane_id}"}
	if c.agentTarget == "" {
		args = append(args, "-h", "-t", c.statusTarget)
	} else {
		args = append(args, "-v", "-t", c.agentTarget)
	}
	if strings.TrimSpace(spec.Workdir) != "" {
		args = append(args, "-c", spec.Workdir)
	}
	return append(args, ShellCommand(spec.Command, spec.Args))
}

func (c *Controller) dashboardMode() bool {
	return strings.TrimSpace(c.statusCommand) != ""
}

func (c *Controller) loadDashboardTargetsLocked(ctx context.Context) error {
	output, err := c.run(ctx, c.command, []string{"list-panes", "-t", c.sessionName + ":vibedrive", "-F", "#{pane_id}"}, "")
	if err != nil {
		return fmt.Errorf("inspect tmux dashboard panes: %w: %s", err, strings.TrimSpace(string(output)))
	}
	lines := strings.Fields(strings.TrimSpace(string(output)))
	if len(lines) == 0 {
		return nil
	}
	c.statusTarget = lines[0]
	if len(lines) > 1 {
		c.agentTarget = lines[len(lines)-1]
	}
	return nil
}

func (c *Controller) selectDashboardLayoutLocked(ctx context.Context) error {
	output, err := c.run(ctx, c.command, []string{"select-layout", "-t", c.sessionName + ":vibedrive", "main-vertical"}, "")
	if err != nil {
		return fmt.Errorf("select tmux dashboard layout: %w: %s", err, strings.TrimSpace(string(output)))
	}
	c.resizeDashboardStatusPaneLocked(ctx)
	return nil
}

func (c *Controller) resizeDashboardStatusPaneLocked(ctx context.Context) {
	if c.statusTarget == "" {
		return
	}
	output, err := c.run(ctx, c.command, []string{"display-message", "-p", "-t", c.statusTarget, "#{window_width}"}, "")
	if err != nil {
		return
	}
	windowWidth, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || windowWidth < 3 {
		return
	}
	statusWidth := windowWidth / 3
	if statusWidth < 1 {
		return
	}
	_, _ = c.run(ctx, c.command, []string{"resize-pane", "-t", c.statusTarget, "-x", strconv.Itoa(statusWidth)}, "")
}

func (c *Controller) paneAliveLocked(ctx context.Context, target string) (bool, error) {
	output, err := c.run(ctx, c.command, []string{"display-message", "-p", "-t", target, "#{pane_dead}"}, "")
	if err != nil {
		message := strings.TrimSpace(string(output))
		if tmuxTargetMissing(output) {
			return false, nil
		}
		return false, fmt.Errorf("inspect tmux pane %s: %w: %s", target, err, message)
	}
	return strings.TrimSpace(string(output)) != "1", nil
}

func tmuxTargetMissing(output []byte) bool {
	return tmuxMessageTargetMissing(string(output))
}

func tmuxMessageTargetMissing(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	for _, marker := range []string{
		"can't find pane",
		"can't find window",
		"can't find session",
		"pane not found",
		"window not found",
		"session not found",
		"target not found",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func IsTargetMissingError(err error) bool {
	if err == nil {
		return false
	}
	return tmuxMessageTargetMissing(err.Error())
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("load tmux paste buffer: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := c.run(ctx, c.command, []string{"paste-buffer", "-d", "-b", bufferName, "-t", p.Target}, ""); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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
	if err != nil && !tmuxTargetMissing(output) && !IsTargetMissingError(err) {
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
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
		if IsTargetMissingError(err) {
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("display tmux pane %s: %w: %s", p.Target, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func (c *Controller) checkAvailable() error {
	if _, err := c.lookPath(c.command); err != nil {
		return fmt.Errorf("tmux is required for vibedrive TUI execution; install tmux and retry: %w", err)
	}
	output, err := c.run(context.Background(), c.command, []string{"-V"}, "")
	if err != nil {
		return fmt.Errorf("tmux is required for vibedrive TUI execution; %s -V failed: %w: %s", c.command, err, strings.TrimSpace(string(output)))
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
