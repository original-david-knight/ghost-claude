package claude

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"vibedrive/pkg/ptyrunner"
)

const (
	exitCommand         = "/exit\r"
	visibleTextMaxBytes = 1024
	closeTimeout        = 5 * time.Second
	statePollInterval   = 50 * time.Millisecond
	submitKeyDelay      = 100 * time.Millisecond
	submitRetryInterval = 2 * time.Second
	submitMaxAttempts   = 3
)

type tuiSession struct {
	pty     *ptyrunner.Session
	monitor *titleMonitor
}

type titleMonitor struct {
	mu                  sync.Mutex
	parser              ptyrunner.TitleParser
	text                visibleTextParser
	idleTransitions     int
	busyTransitions     int
	trustPrompts        int
	trustPromptDetected bool
}

type titleSnapshot struct {
	idleTransitions int
	busyTransitions int
	trustPrompts    int
}

type visibleTextParser struct {
	recent string
}

func (c *Client) startTUI(ctx context.Context) (*tuiSession, error) {
	monitor := &titleMonitor{}
	ptySession, err := ptyrunner.Start(ctx, ptyrunner.Config{
		Label:   "claude tui",
		Command: c.command,
		Args:    c.args,
		Workdir: c.workdir,
		Stdout:  c.stdout,
		Monitor: monitor,
	})
	if err != nil {
		return nil, err
	}

	session := &tuiSession{
		pty:     ptySession,
		monitor: monitor,
	}

	readyCtx, cancel := context.WithTimeout(ctx, c.startupTimeout)
	defer cancel()

	if err := session.completeStartup(readyCtx); err != nil {
		_ = session.Close()
		return nil, err
	}

	return session, nil
}

func (s *tuiSession) SendPrompt(ctx context.Context, prompt string) error {
	snapshot := s.monitor.snapshot()

	if err := s.submitPrompt(ctx, prompt, snapshot.busyTransitions); err != nil {
		return err
	}
	if err := s.waitForIdleTransition(ctx, snapshot.idleTransitions, snapshot.busyTransitions); err != nil {
		return fmt.Errorf("wait for claude tui to become idle: %w", err)
	}

	return nil
}

func (s *tuiSession) SendInteractivePrompt(ctx context.Context, prompt string) error {
	snapshot := s.monitor.snapshot()

	if err := s.submitPrompt(ctx, prompt, snapshot.busyTransitions); err != nil {
		return err
	}
	return s.waitForExit(ctx)
}

func (s *tuiSession) Close() error {
	return s.pty.Close(exitCommand, closeTimeout)
}

func (s *tuiSession) submitPrompt(ctx context.Context, prompt string, busyStart int) error {
	normalized := ptyrunner.NormalizePrompt(prompt)
	if normalized == "" {
		return fmt.Errorf("claude tui prompt is empty after normalization")
	}

	if _, err := io.WriteString(s.pty, normalized); err != nil {
		return fmt.Errorf("write prompt to claude tui: %w", err)
	}
	if err := ptyrunner.Sleep(ctx, submitKeyDelay); err != nil {
		return err
	}

	// Press Enter and wait briefly for Claude to start processing. If it
	// doesn't, the Enter likely got eaten by paste-bracketing or composer
	// state — retry a small number of times before giving up so we never
	// hang forever on a silently-dropped submit.
	for range submitMaxAttempts {
		if _, err := io.WriteString(s.pty, "\r"); err != nil {
			return fmt.Errorf("submit prompt to claude tui: %w", err)
		}
		busy, err := s.waitForBusyTransition(ctx, busyStart, submitRetryInterval)
		if err != nil {
			return err
		}
		if busy {
			return nil
		}
	}

	return fmt.Errorf("claude tui did not start processing after %d enter presses", submitMaxAttempts)
}

// waitForBusyTransition polls the title monitor until Claude transitions to a
// busy state (indicating it accepted our submit) or the timeout elapses.
// Returns (false, nil) on timeout — the caller decides whether to retry.
func (s *tuiSession) waitForBusyTransition(ctx context.Context, busyStart int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	for {
		snapshot := s.monitor.snapshot()
		if snapshot.busyTransitions > busyStart {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-s.pty.Done():
			if err := s.pty.ExitErr(); err != nil {
				return false, fmt.Errorf("claude tui exited: %w", err)
			}
			return false, fmt.Errorf("claude tui exited unexpectedly")
		case <-ticker.C:
		}
	}
}

func (s *tuiSession) waitForIdleTransition(ctx context.Context, idleStart, busyStart int) error {
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	for {
		snapshot := s.monitor.snapshot()
		if snapshot.busyTransitions > busyStart && snapshot.idleTransitions > idleStart {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.pty.Done():
			if err := s.pty.ExitErr(); err != nil {
				return fmt.Errorf("claude tui exited: %w", err)
			}
			return fmt.Errorf("claude tui exited unexpectedly")
		case <-ticker.C:
		}
	}
}

func (s *tuiSession) waitForExit(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.pty.Done():
		return s.pty.ExitErr()
	}
}

func (s *tuiSession) completeStartup(ctx context.Context) error {
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	handledTrustPrompts := 0

	for {
		snapshot := s.monitor.snapshot()
		if snapshot.idleTransitions > 0 {
			return nil
		}
		if snapshot.trustPrompts > handledTrustPrompts {
			if _, err := io.WriteString(s.pty, "\r"); err != nil {
				return fmt.Errorf("confirm claude trust dialog: %w", err)
			}
			handledTrustPrompts = snapshot.trustPrompts
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.pty.Done():
			if err := s.pty.ExitErr(); err != nil {
				return fmt.Errorf("claude tui exited: %w", err)
			}
			return fmt.Errorf("claude tui exited unexpectedly")
		case <-ticker.C:
		}
	}
}

func (m *titleMonitor) Consume(chunk []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, title := range m.parser.Consume(chunk) {
		state, ok := classifyTitle(title)
		if !ok {
			continue
		}
		if state == "idle" {
			m.idleTransitions++
		} else {
			m.busyTransitions++
		}
	}

	recentVisible := m.text.consume(chunk)
	trustDetected := strings.Contains(recentVisible, "yesitrustthisfolder")
	if trustDetected && !m.trustPromptDetected {
		m.trustPrompts++
	}
	m.trustPromptDetected = trustDetected
}

func (m *titleMonitor) snapshot() titleSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	return titleSnapshot{
		idleTransitions: m.idleTransitions,
		busyTransitions: m.busyTransitions,
		trustPrompts:    m.trustPrompts,
	}
}

func classifyTitle(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "✳ ") {
		return "idle", true
	}
	return "busy", true
}

func (p *visibleTextParser) consume(chunk []byte) string {
	p.recent += ptyrunner.CompactVisibleText(chunk)
	if len(p.recent) > visibleTextMaxBytes {
		p.recent = p.recent[len(p.recent)-visibleTextMaxBytes:]
	}
	return p.recent
}
