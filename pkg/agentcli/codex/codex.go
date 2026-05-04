package codex

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
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
	Started bool
	tui     *tuiSession
}

func New(command string, args []string, workdir, transport, startupTimeout string, stdout, stderr io.Writer) (*Client, error) {
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}

	normalizedTransport := normalizeTransport(transport)
	if normalizedTransport == "" {
		normalizedTransport = TransportTUI
	}

	baseArgs := append([]string{}, args...)
	if len(baseArgs) == 0 {
		baseArgs = defaultArgs()
	}
	if subcommand := Subcommand(baseArgs); !IsInteractiveSubcommand(subcommand) {
		if IsNonInteractiveSubcommand(subcommand) {
			return nil, fmt.Errorf("codex args subcommand %q is non-interactive and is not supported for TUI agent steps", subcommand)
		}
		return nil, fmt.Errorf("codex args subcommand %q is not supported for TUI agent steps", subcommand)
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
	case TransportTUI:
	case "exec":
		return nil, fmt.Errorf("codex transport %q is no longer supported; use %q", transport, TransportTUI)
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
	case TransportTUI:
		return c.runTUIPrompt(ctx, session, prompt)
	default:
		return fmt.Errorf("unsupported transport %q", c.transport)
	}
}

func (c *Client) RunInteractivePrompt(ctx context.Context, session *Session, prompt string) error {
	switch c.transport {
	case TransportTUI:
		return c.runTUIInteractivePrompt(ctx, session, prompt)
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

	if !session.Started {
		tui, err := c.startTUI(ctx)
		if err != nil {
			return err
		}
		session.tui = tui
		session.Started = true
	}

	return session.tui.SendPrompt(ctx, prompt)
}

func (c *Client) runTUIInteractivePrompt(ctx context.Context, session *Session, prompt string) error {
	if session == nil {
		return fmt.Errorf("codex tui requires a session")
	}

	if !session.Started {
		tui, err := c.startTUI(ctx)
		if err != nil {
			return err
		}
		session.tui = tui
		session.Started = true
	}

	return session.tui.SendInteractivePrompt(ctx, prompt)
}

func normalizeTransport(transport string) string {
	return strings.TrimSpace(strings.ToLower(transport))
}

func defaultArgs() []string {
	return nil
}
