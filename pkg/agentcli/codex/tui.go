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
	trustPrompts    int
	trustDetected   bool
	currentState    string
	visibleTail     string
}

type titleSnapshot struct {
	idleTransitions int
	busyTransitions int
	trustPrompts    int
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
		return nil, fmt.Errorf("wait for codex tui startup (%s): %w", c.startupTimeout, err)
	}

	return session, nil
}

func (s *tuiSession) SendPrompt(ctx context.Context, prompt string) error {
	snapshot := s.monitor.snapshot()

	if err := s.submitPrompt(ctx, prompt, snapshot.busyTransitions); err != nil {
		return err
	}
	if err := s.waitForIdleTransition(ctx, snapshot.idleTransitions, snapshot.busyTransitions); err != nil {
		return fmt.Errorf("wait for codex tui to become idle: %w", err)
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
	return s.pty.Close(exitByte, closeTimeout)
}

func (s *tuiSession) submitPrompt(ctx context.Context, prompt string, busyStart int) error {
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

	for range submitMaxAttempts {
		if _, err := io.WriteString(s.pty, "\r"); err != nil {
			return fmt.Errorf("submit prompt to codex tui: %w", err)
		}
		busy, err := s.waitForBusyTransition(ctx, busyStart, submitRetryInterval)
		if err != nil {
			return err
		}
		if busy {
			return nil
		}
	}

	return fmt.Errorf("%w after %d enter presses", errTUIPromptNotAccepted, submitMaxAttempts)
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
		if snapshot.trustPrompts > handledTrustPrompts {
			if _, err := io.WriteString(s.pty, "\r"); err != nil {
				return fmt.Errorf("confirm codex trust dialog: %w", err)
			}
			handledTrustPrompts = snapshot.trustPrompts
		}
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

	m.recordVisibleTextLocked(chunk)
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
		trustPrompts:    m.trustPrompts,
		currentState:    m.currentState,
	}
}

func (m *titleMonitor) recordVisibleTextLocked(chunk []byte) {
	m.visibleTail += ptyrunner.CompactVisibleText(chunk)
	const maxVisibleTail = 4096
	if len(m.visibleTail) > maxVisibleTail {
		m.visibleTail = m.visibleTail[len(m.visibleTail)-maxVisibleTail:]
	}

	detected := codexTrustPromptCompact(m.visibleTail)
	if detected && !m.trustDetected {
		m.trustPrompts++
	}
	m.trustDetected = detected
}

func (s titleSnapshot) readyForPrompt() bool {
	return s.currentState == "idle" && s.busyTransitions > 0
}

func (m *titleMonitor) classifyTitle(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", false
	}
	if isBusyTitle(trimmed) {
		return "busy", true
	}
	if titleMatchesIdle(trimmed, m.idleTitle) {
		return "idle", true
	}
	if titleLooksIdleStatus(trimmed) {
		return "idle", true
	}
	return "busy", true
}

func isBusyTitle(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	if lower == "" {
		return false
	}
	if strings.HasPrefix(lower, "busy ") {
		return true
	}
	for _, marker := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func titleMatchesIdle(title, idleTitle string) bool {
	title = strings.TrimSpace(title)
	idleTitle = strings.TrimSpace(idleTitle)
	if title == "" || idleTitle == "" {
		return false
	}
	if title == idleTitle {
		return true
	}
	if filepath.Base(filepath.Clean(title)) == idleTitle {
		return true
	}
	if titleAbbreviatesIdle(title, idleTitle) {
		return true
	}
	return strings.Contains(title, idleTitle)
}

func titleAbbreviatesIdle(title, idleTitle string) bool {
	for _, marker := range []string{"...", "…"} {
		if !strings.Contains(title, marker) {
			continue
		}
		parts := strings.Split(title, marker)
		prefix := strings.TrimSpace(parts[0])
		suffix := strings.TrimSpace(parts[len(parts)-1])
		if idleTitleHasPrefix(idleTitle, prefix) || idleTitleHasSuffix(idleTitle, suffix) {
			return true
		}
	}
	return false
}

func idleTitleHasPrefix(idleTitle, prefix string) bool {
	if !idleFragmentLongEnough(idleTitle, prefix) {
		return false
	}
	if strings.HasPrefix(idleTitle, prefix) {
		return true
	}
	base := filepath.Base(filepath.Clean(prefix))
	return base != prefix && idleFragmentLongEnough(idleTitle, base) && strings.HasPrefix(idleTitle, base)
}

func idleTitleHasSuffix(idleTitle, suffix string) bool {
	if !idleFragmentLongEnough(idleTitle, suffix) {
		return false
	}
	if strings.HasSuffix(idleTitle, suffix) {
		return true
	}
	base := filepath.Base(filepath.Clean(suffix))
	return base != suffix && idleFragmentLongEnough(idleTitle, base) && strings.HasSuffix(idleTitle, base)
}

func idleFragmentLongEnough(idleTitle, fragment string) bool {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return false
	}
	required := 8
	if len(idleTitle) < required {
		required = len(idleTitle)
	}
	return len(fragment) >= required
}

func titleLooksIdleStatus(title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return false
	}
	return (strings.Contains(title, " · ") || strings.Contains(title, " • ")) && (strings.Contains(title, "/") || strings.Contains(title, "~"))
}

func codexTrustPrompt(text string) bool {
	return codexTrustPromptCompact(ptyrunner.CompactVisibleText([]byte(text)))
}

func codexTrustPromptCompact(compact string) bool {
	return strings.Contains(compact, "doyoutrustthecontentsofthisdirectory") &&
		strings.Contains(compact, "yescontinue") &&
		strings.Contains(compact, "noquit")
}

func codexReadyScreen(text string) bool {
	return ReadyScreen(text)
}

// ReadyScreen reports whether captured Codex screen text looks ready for a prompt.
func ReadyScreen(text string) bool {
	compact := strings.ToLower(ptyrunner.CompactVisibleText([]byte(text)))
	return strings.Contains(compact, "openaicodex") &&
		strings.Contains(compact, "model") &&
		strings.Contains(compact, "directory") &&
		strings.Contains(compact, "permissions")
}

func codexScreenState(text string) (string, bool) {
	return ScreenState(text)
}

// ScreenState classifies captured Codex screen text as idle or busy when known.
func ScreenState(text string) (string, bool) {
	compact := strings.ToLower(ptyrunner.CompactVisibleText([]byte(text)))
	if strings.Contains(compact, "working") && strings.Contains(compact, "interrupt") {
		return "busy", true
	}
	if codexReadyScreen(text) {
		return "idle", true
	}
	return "", false
}
