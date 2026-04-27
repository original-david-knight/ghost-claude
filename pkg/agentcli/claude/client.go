package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	TransportPrint = "print"
	TransportTUI   = "tui"

	SessionStrategySessionID = "session_id"
	SessionStrategyContinue  = "continue"
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
	Strategy string
	ID       string
	Started  bool
	tui      *tuiSession
}

func New(command string, args []string, workdir, transport, startupTimeout string, stdout, stderr io.Writer) (*Client, error) {
	timeout, err := time.ParseDuration(startupTimeout)
	if err != nil {
		return nil, fmt.Errorf("parse claude.startup_timeout %q: %w", startupTimeout, err)
	}

	client := &Client{
		command: command,
		args:    append([]string{}, args...),
		workdir: workdir,
		transport: func() string {
			if strings.TrimSpace(transport) == "" {
				return TransportTUI
			}
			return strings.TrimSpace(strings.ToLower(transport))
		}(),
		startupTimeout: timeout,
		stdout:         stdout,
		stderr:         stderr,
	}

	switch client.transport {
	case TransportPrint, TransportTUI:
	default:
		return nil, fmt.Errorf("unsupported claude transport %q", transport)
	}

	return client, nil
}

func NewSession(strategy string) (*Session, error) {
	normalized := strings.TrimSpace(strings.ToLower(strategy))
	switch normalized {
	case "", SessionStrategySessionID:
		id, err := newUUID()
		if err != nil {
			return nil, err
		}
		return &Session{Strategy: SessionStrategySessionID, ID: id}, nil
	case SessionStrategyContinue:
		return &Session{Strategy: SessionStrategyContinue}, nil
	default:
		return nil, fmt.Errorf("unsupported session strategy %q", strategy)
	}
}

func (c *Client) RunPrompt(ctx context.Context, session *Session, prompt string) error {
	switch c.transport {
	case TransportPrint:
		return c.runPrintPrompt(ctx, session, prompt)
	case TransportTUI:
		return c.runTUIPrompt(ctx, session, prompt)
	default:
		return fmt.Errorf("unsupported transport %q", c.transport)
	}
}

func (c *Client) RunInteractivePrompt(ctx context.Context, session *Session, prompt string) error {
	switch c.transport {
	case TransportPrint:
		return fmt.Errorf("claude transport %q cannot run an interactive prompt; set claude.transport to %q", c.transport, TransportTUI)
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

func (c *Client) runPrintPrompt(ctx context.Context, session *Session, prompt string) error {
	args := append([]string{}, c.args...)
	args = append(args, "--print")

	switch session.Strategy {
	case SessionStrategySessionID:
		if session.Started {
			args = append(args, "--resume", session.ID)
		} else {
			args = append(args, "--session-id", session.ID)
		}
	case SessionStrategyContinue:
		if session.Started {
			args = append(args, "--continue")
		}
	default:
		return fmt.Errorf("unsupported session strategy %q", session.Strategy)
	}

	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = c.workdir
	cmd.Stdout = c.stdout
	cmd.Stderr = c.stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	session.Started = true
	return nil
}

func (c *Client) runTUIPrompt(ctx context.Context, session *Session, prompt string) error {
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

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}

	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80

	buf := make([]byte, 36)
	hex.Encode(buf[0:8], raw[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], raw[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], raw[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], raw[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], raw[10:16])

	return string(buf), nil
}
