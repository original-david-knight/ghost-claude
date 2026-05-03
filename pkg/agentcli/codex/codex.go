package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	TransportExec         = "exec"
	TransportTUI          = "tui"
	defaultStartupTimeout = 30 * time.Second
)

type Client struct {
	command        string
	args           []string
	workdir        string
	transport      string
	startupTimeout time.Duration
	stdout         io.Writer
	stderr         io.Writer
}

type Session struct {
	Started      bool
	ExecFallback bool
	tui          *tuiSession
}

func New(command string, args []string, workdir, transport, startupTimeout string, stdout, stderr io.Writer) (*Client, error) {
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}

	normalizedTransport := normalizeTransport(transport)
	if normalizedTransport == "" {
		normalizedTransport = defaultTransport(args)
	}

	baseArgs := append([]string{}, args...)
	if len(baseArgs) == 0 {
		baseArgs = defaultArgs(normalizedTransport)
	}

	timeout := defaultStartupTimeout
	if strings.TrimSpace(startupTimeout) != "" {
		parsedTimeout, err := time.ParseDuration(startupTimeout)
		if err != nil {
			return nil, fmt.Errorf("parse codex.startup_timeout %q: %w", startupTimeout, err)
		}
		timeout = parsedTimeout
	}

	client := &Client{
		command:        command,
		args:           baseArgs,
		workdir:        workdir,
		transport:      normalizedTransport,
		startupTimeout: timeout,
		stdout:         stdout,
		stderr:         stderr,
	}

	switch client.transport {
	case TransportExec, TransportTUI:
	default:
		return nil, fmt.Errorf("unsupported codex transport %q", transport)
	}

	return client, nil
}

func NewSession() (*Session, error) {
	return &Session{}, nil
}

func (c *Client) RunPrompt(ctx context.Context, session *Session, prompt string) error {
	switch c.transport {
	case TransportExec:
		return c.runExecPrompt(ctx, prompt)
	case TransportTUI:
		return c.runTUIPrompt(ctx, session, prompt)
	default:
		return fmt.Errorf("unsupported transport %q", c.transport)
	}
}

func (c *Client) Close(session *Session) error {
	if session == nil || session.tui == nil {
		return nil
	}
	return session.tui.Close()
}

func (c *Client) IsFullscreenTUI() bool {
	return c.transport == TransportTUI
}

func (c *Client) runTUIPrompt(ctx context.Context, session *Session, prompt string) error {
	if session == nil {
		return fmt.Errorf("codex tui requires a session")
	}
	if session.ExecFallback {
		return c.runExecPrompt(ctx, prompt)
	}

	if !session.Started {
		tui, err := c.startTUI(ctx)
		if err != nil {
			return err
		}
		session.tui = tui
		session.Started = true
	}

	if err := session.tui.SendPrompt(ctx, prompt); err != nil {
		if errors.Is(err, errTUIPromptNotAccepted) {
			return c.fallbackFromTUIToExec(ctx, session, prompt, err)
		}
		return err
	}

	return nil
}

func (c *Client) runExecPrompt(ctx context.Context, prompt string) error {
	return c.runExecPromptWithArgs(ctx, c.execArgs(), prompt)
}

func (c *Client) runExecPromptWithArgs(ctx context.Context, args []string, prompt string) error {
	if shouldFilterExecOutput(args) {
		return c.runPromptJSON(ctx, args, prompt)
	}

	return c.runPromptPassthrough(ctx, args, prompt)
}

func (c *Client) runPromptPassthrough(ctx context.Context, baseArgs []string, prompt string) error {
	args := append([]string{}, baseArgs...)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = c.workdir
	cmd.Stdout = c.stdout
	cmd.Stderr = c.stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	return nil
}

func (c *Client) runPromptJSON(ctx context.Context, baseArgs []string, prompt string) error {
	args := append([]string{}, baseArgs...)
	args = append(args, "--json", prompt)

	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = c.workdir

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe for %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe for %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		_, _ = io.Copy(c.stderr, stderrPipe)
	}()

	renderErr := c.renderJSONStream(stdoutPipe)
	waitErr := cmd.Wait()
	stderrWG.Wait()

	if renderErr != nil {
		return fmt.Errorf("render %q json stream: %w", c.command, renderErr)
	}
	if waitErr != nil {
		return fmt.Errorf("run %q with args %q: %w", c.command, strings.Join(args, " "), waitErr)
	}

	return nil
}

func (c *Client) shouldFilterExecOutput() bool {
	return shouldFilterExecOutput(c.execArgs())
}

func shouldFilterExecOutput(args []string) bool {
	return Subcommand(args) == "exec" && !containsArg(args, "--json")
}

func (c *Client) execArgs() []string {
	if c.transport == TransportExec {
		return append([]string{}, c.args...)
	}

	args := append([]string{}, c.args...)
	switch Subcommand(args) {
	case "", "exec":
	default:
		return nil
	}

	filtered := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if arg == "--no-alt-screen" {
			continue
		}
		filtered = append(filtered, arg)
	}
	if Subcommand(filtered) == "" {
		filtered = append(filtered, "exec")
	}
	return filtered
}

func (c *Client) fallbackFromTUIToExec(ctx context.Context, session *Session, prompt string, cause error) error {
	if session == nil {
		return cause
	}
	execArgs := c.execArgs()
	if len(execArgs) == 0 {
		return cause
	}

	if session.tui != nil {
		if closeErr := session.tui.Close(); closeErr != nil && c.stderr != nil {
			fmt.Fprintf(c.stderr, "warning: codex tui submit failed (%v); closing tui also failed: %v\n", cause, closeErr)
		}
		session.tui = nil
	}
	session.Started = false
	session.ExecFallback = true

	if c.stderr != nil {
		fmt.Fprintf(c.stderr, "warning: codex tui did not acknowledge the submitted prompt; falling back to `codex exec` for the rest of this item\n")
	}

	return c.runExecPromptWithArgs(ctx, execArgs, prompt)
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func normalizeTransport(transport string) string {
	return strings.TrimSpace(strings.ToLower(transport))
}

func defaultTransport(args []string) string {
	if IsInteractiveSubcommand(Subcommand(args)) {
		return TransportTUI
	}
	return TransportExec
}

func defaultArgs(transport string) []string {
	switch transport {
	case TransportExec:
		return []string{"exec"}
	default:
		return nil
	}
}

type event struct {
	Type string `json:"type"`
	Item *item  `json:"item,omitempty"`
}

type item struct {
	Type             string       `json:"type"`
	Text             string       `json:"text,omitempty"`
	Command          string       `json:"command,omitempty"`
	AggregatedOutput string       `json:"aggregated_output,omitempty"`
	ExitCode         *int         `json:"exit_code,omitempty"`
	Changes          []fileChange `json:"changes,omitempty"`
}

type fileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (c *Client) renderJSONStream(r io.Reader) error {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		if err := c.renderJSONLine(line); err != nil {
			return err
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (c *Client) renderJSONLine(line string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}

	var evt event
	if err := json.Unmarshal([]byte(trimmed), &evt); err != nil {
		return writeText(c.stdout, trimmed)
	}

	switch evt.Type {
	case "item.started":
		return c.renderItem(evt.Item, true)
	case "item.completed":
		return c.renderItem(evt.Item, false)
	default:
		return nil
	}
}

func (c *Client) renderItem(item *item, started bool) error {
	if item == nil {
		return nil
	}

	switch item.Type {
	case "agent_message":
		if started {
			return nil
		}
		return writeText(c.stdout, item.Text)
	case "command_execution":
		return c.renderCommand(item, started)
	case "file_change":
		if started {
			return nil
		}
		for _, change := range item.Changes {
			if err := writeText(c.stdout, fmt.Sprintf("%s %s", formatChangeVerb(change.Kind), change.Path)); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Client) renderCommand(item *item, started bool) error {
	if strings.TrimSpace(item.Command) == "" {
		return nil
	}

	if started {
		return writeText(c.stdout, "$ "+item.Command)
	}

	return nil
}

func writeText(w io.Writer, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	_, err := fmt.Fprintln(w, text)
	return err
}

func formatChangeVerb(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "add":
		return "added"
	case "update":
		return "updated"
	case "delete":
		return "deleted"
	default:
		return "changed"
	}
}
