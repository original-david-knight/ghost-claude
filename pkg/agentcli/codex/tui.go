package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vibedrive/pkg/ptyrunner"
)

const (
	exitByte            = "\x04"
	closeTimeout        = 5 * time.Second
	statePollInterval   = 50 * time.Millisecond
	submitKeyDelay      = 100 * time.Millisecond
	submitRetryInterval = 2 * time.Second
	submitMaxAttempts   = 3
)

var errTUIPromptNotAccepted = errors.New("codex tui did not start processing")

type tuiSession struct {
	pty     *ptyrunner.Session
	monitor *titleMonitor
}

type titleMonitor struct {
	mu              sync.Mutex
	idleTitle       string
	parser          ptyrunner.TitleParser
	idleTransitions int
	busyTransitions int
	currentState    string
}

type titleSnapshot struct {
	idleTransitions int
	busyTransitions int
	currentState    string
}

func (c *Client) startTUI(ctx context.Context) (*tuiSession, error) {
	monitor := newTitleMonitor(c.workdir)
	ptySession, err := ptyrunner.Start(ctx, ptyrunner.Config{
		Label:   "codex tui",
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

	normalized := ptyrunner.NormalizePrompt(prompt)
	if normalized == "" {
		return fmt.Errorf("codex tui prompt is empty after normalization")
	}

	if err := ptyrunner.WriteBracketedPaste(s.pty, normalized); err != nil {
		return fmt.Errorf("write prompt to codex tui: %w", err)
	}
	if err := ptyrunner.Sleep(ctx, submitKeyDelay); err != nil {
		return err
	}

	submitted := false
	for range submitMaxAttempts {
		if _, err := io.WriteString(s.pty, "\r"); err != nil {
			return fmt.Errorf("submit prompt to codex tui: %w", err)
		}
		busy, err := s.waitForBusyTransition(ctx, snapshot.busyTransitions, submitRetryInterval)
		if err != nil {
			return err
		}
		if busy {
			submitted = true
			break
		}
	}
	if !submitted {
		return fmt.Errorf("%w after %d enter presses", errTUIPromptNotAccepted, submitMaxAttempts)
	}

	if err := s.waitForIdleTransition(ctx, snapshot.idleTransitions, snapshot.busyTransitions); err != nil {
		return fmt.Errorf("wait for codex tui to become idle: %w", err)
	}

	return nil
}

func (s *tuiSession) Close() error {
	return s.pty.Close(exitByte, closeTimeout)
}

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
				return false, fmt.Errorf("codex tui exited: %w", err)
			}
			return false, fmt.Errorf("codex tui exited unexpectedly")
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
				return fmt.Errorf("codex tui exited: %w", err)
			}
			return fmt.Errorf("codex tui exited unexpectedly")
		case <-ticker.C:
		}
	}
}

func (s *tuiSession) completeStartup(ctx context.Context) error {
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	for {
		snapshot := s.monitor.snapshot()
		if snapshot.readyForPrompt() {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.pty.Done():
			if err := s.pty.ExitErr(); err != nil {
				return fmt.Errorf("codex tui exited: %w", err)
			}
			return fmt.Errorf("codex tui exited unexpectedly")
		case <-ticker.C:
		}
	}
}

func newTitleMonitor(workdir string) *titleMonitor {
	idleTitle := filepath.Base(filepath.Clean(workdir))
	if idleTitle == "." || idleTitle == string(filepath.Separator) || idleTitle == "" {
		idleTitle = "codex"
	}

	return &titleMonitor{idleTitle: idleTitle}
}

func (m *titleMonitor) Consume(chunk []byte) {
	m.consume(chunk)
}

func (m *titleMonitor) consume(chunk []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, title := range m.parser.Consume(chunk) {
		state, ok := m.classifyTitle(title)
		if !ok {
			continue
		}
		m.currentState = state
		if state == "idle" {
			m.idleTransitions++
		} else {
			m.busyTransitions++
		}
	}
}

func (m *titleMonitor) snapshot() titleSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	return titleSnapshot{
		idleTransitions: m.idleTransitions,
		busyTransitions: m.busyTransitions,
		currentState:    m.currentState,
	}
}

func (s titleSnapshot) readyForPrompt() bool {
	return s.currentState == "idle" && s.busyTransitions > 0
}

func (m *titleMonitor) classifyTitle(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", false
	}
	if trimmed == m.idleTitle {
		return "idle", true
	}
	return "busy", true
}
