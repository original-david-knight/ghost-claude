package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vibedrive/internal/tmuxagent"
	"vibedrive/pkg/agentcli/claude"
	"vibedrive/pkg/agentcli/codex"
	"vibedrive/pkg/ptyrunner"
)

const (
	tmuxCloseTimeout        = 5 * time.Second
	tmuxStatePollInterval   = 50 * time.Millisecond
	tmuxSubmitKeyDelay      = 100 * time.Millisecond
	tmuxSubmitRetryInterval = 2 * time.Second
	tmuxSubmitMaxAttempts   = 3
)

type tmuxClaudeClient struct {
	command        string
	args           []string
	workdir        string
	startupTimeout time.Duration
	controller     *tmuxagent.Controller
	name           string

	mu       sync.Mutex
	sessions map[*claude.Session]*tmuxClaudeSession
}

type tmuxCodexClient struct {
	command        string
	args           []string
	workdir        string
	startupTimeout time.Duration
	controller     *tmuxagent.Controller
	name           string

	mu       sync.Mutex
	sessions map[*codex.Session]*tmuxCodexSession
}

type tmuxClaudeSession struct {
	pane                *tmuxagent.Pane
	idleTransitions     int
	busyTransitions     int
	trustPrompts        int
	trustPromptDetected bool
	currentState        string
}

type tmuxCodexSession struct {
	pane            *tmuxagent.Pane
	idleTitle       string
	idleTransitions int
	busyTransitions int
	currentState    string
}

type tmuxTitleSnapshot struct {
	idleTransitions int
	busyTransitions int
	trustPrompts    int
	currentState    string
}

func newTmuxClaudeClient(command string, args []string, workdir, startupTimeout string, controller *tmuxagent.Controller, name string) (*tmuxClaudeClient, error) {
	timeout, err := time.ParseDuration(startupTimeout)
	if err != nil {
		return nil, fmt.Errorf("parse claude.startup_timeout %q: %w", startupTimeout, err)
	}
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	return &tmuxClaudeClient{
		command:        command,
		args:           append([]string{}, args...),
		workdir:        workdir,
		startupTimeout: timeout,
		controller:     controller,
		name:           name,
		sessions:       make(map[*claude.Session]*tmuxClaudeSession),
	}, nil
}

func newTmuxCodexClient(command string, args []string, workdir, startupTimeout string, controller *tmuxagent.Controller, name string) (*tmuxCodexClient, error) {
	timeout := 30 * time.Second
	if strings.TrimSpace(startupTimeout) != "" {
		parsed, err := time.ParseDuration(startupTimeout)
		if err != nil {
			return nil, fmt.Errorf("parse codex.startup_timeout %q: %w", startupTimeout, err)
		}
		timeout = parsed
	}
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}
	return &tmuxCodexClient{
		command:        command,
		args:           append([]string{}, args...),
		workdir:        workdir,
		startupTimeout: timeout,
		controller:     controller,
		name:           name,
		sessions:       make(map[*codex.Session]*tmuxCodexSession),
	}, nil
}

func (c *tmuxClaudeClient) RunPrompt(ctx context.Context, session *claude.Session, prompt string) error {
	if session == nil {
		return fmt.Errorf("claude tmux tui requires a session")
	}
	state, err := c.ensureSession(ctx, session)
	if err != nil {
		return err
	}
	if err := state.sendPrompt(ctx, prompt); err != nil {
		return state.withDiagnostics(ctx, err)
	}
	return nil
}

func (c *tmuxClaudeClient) Close(session *claude.Session) error {
	if session == nil {
		return nil
	}
	c.mu.Lock()
	state := c.sessions[session]
	delete(c.sessions, session)
	c.mu.Unlock()
	if state == nil || state.pane == nil {
		return nil
	}
	return state.close(context.Background())
}

func (c *tmuxClaudeClient) IsFullscreenTUI() bool {
	return true
}

func (c *tmuxClaudeClient) ensureSession(ctx context.Context, session *claude.Session) (*tmuxClaudeSession, error) {
	c.mu.Lock()
	state := c.sessions[session]
	if state == nil {
		state = &tmuxClaudeSession{}
		c.sessions[session] = state
	}
	c.mu.Unlock()

	if session.Started {
		return state, nil
	}
	pane, err := c.controller.NewPane(ctx, tmuxagent.PaneSpec{
		Name:    c.name,
		Agent:   "claude",
		Command: c.command,
		Args:    c.args,
		Workdir: c.workdir,
	})
	if err != nil {
		return nil, err
	}
	state.pane = pane

	readyCtx, cancel := context.WithTimeout(ctx, c.startupTimeout)
	defer cancel()
	if err := state.completeStartup(readyCtx); err != nil {
		_ = state.close(context.Background())
		return nil, state.withDiagnostics(ctx, err)
	}
	session.Started = true
	return state, nil
}

func (s *tmuxClaudeSession) sendPrompt(ctx context.Context, prompt string) error {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	if err := s.submitPrompt(ctx, prompt, snapshot.busyTransitions, false); err != nil {
		return err
	}
	if err := s.waitForIdleTransition(ctx, snapshot.idleTransitions, snapshot.busyTransitions); err != nil {
		return fmt.Errorf("wait for claude tmux tui to become idle: %w", err)
	}
	return nil
}

func (s *tmuxClaudeSession) submitPrompt(ctx context.Context, prompt string, busyStart int, bracketed bool) error {
	normalized := ptyrunner.NormalizePrompt(prompt)
	if normalized == "" {
		return fmt.Errorf("claude tmux tui prompt is empty after normalization")
	}
	if bracketed {
		normalized = ptyrunner.BracketedPasteStart + normalized + ptyrunner.BracketedPasteEnd
	}
	if err := s.pane.Paste(ctx, normalized); err != nil {
		return fmt.Errorf("write prompt to claude tmux tui: %w", err)
	}
	if err := ptyrunner.Sleep(ctx, tmuxSubmitKeyDelay); err != nil {
		return err
	}
	for range tmuxSubmitMaxAttempts {
		if err := s.pane.SendEnter(ctx); err != nil {
			return fmt.Errorf("submit prompt to claude tmux tui: %w", err)
		}
		busy, err := s.waitForBusyTransition(ctx, busyStart, tmuxSubmitRetryInterval)
		if err != nil {
			return err
		}
		if busy {
			return nil
		}
	}
	return fmt.Errorf("claude tmux tui did not start processing after %d enter presses", tmuxSubmitMaxAttempts)
}

func (s *tmuxClaudeSession) completeStartup(ctx context.Context) error {
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	handledTrustPrompts := 0
	for {
		snapshot, err := s.snapshot(ctx)
		if err != nil {
			return err
		}
		if snapshot.idleTransitions > 0 {
			return nil
		}
		if snapshot.trustPrompts > handledTrustPrompts {
			if err := s.pane.SendEnter(ctx); err != nil {
				return fmt.Errorf("confirm claude trust dialog: %w", err)
			}
			handledTrustPrompts = snapshot.trustPrompts
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *tmuxClaudeSession) snapshot(ctx context.Context) (tmuxTitleSnapshot, error) {
	title, err := s.pane.Title(ctx)
	if err != nil {
		return tmuxTitleSnapshot{}, err
	}
	if state, ok := classifyClaudeTmuxTitle(title); ok {
		s.recordState(state)
	}
	if text, err := s.pane.Capture(ctx, 80); err == nil {
		trustDetected := strings.Contains(ptyrunner.CompactVisibleText([]byte(text)), "yesitrustthisfolder")
		if trustDetected && !s.trustPromptDetected {
			s.trustPrompts++
		}
		s.trustPromptDetected = trustDetected
	}
	return tmuxTitleSnapshot{
		idleTransitions: s.idleTransitions,
		busyTransitions: s.busyTransitions,
		trustPrompts:    s.trustPrompts,
		currentState:    s.currentState,
	}, nil
}

func classifyClaudeTmuxTitle(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "\u2733 ") {
		return "idle", true
	}
	return "busy", true
}

func (c *tmuxCodexClient) RunPrompt(ctx context.Context, session *codex.Session, prompt string) error {
	if session == nil {
		return fmt.Errorf("codex tmux tui requires a session")
	}
	state, err := c.ensureSession(ctx, session)
	if err != nil {
		return err
	}
	if err := state.sendPrompt(ctx, prompt); err != nil {
		return state.withDiagnostics(ctx, err)
	}
	return nil
}

func (c *tmuxCodexClient) Close(session *codex.Session) error {
	if session == nil {
		return nil
	}
	c.mu.Lock()
	state := c.sessions[session]
	delete(c.sessions, session)
	c.mu.Unlock()
	if state == nil || state.pane == nil {
		return nil
	}
	return state.close(context.Background())
}

func (c *tmuxCodexClient) IsFullscreenTUI() bool {
	return true
}

func (c *tmuxCodexClient) ensureSession(ctx context.Context, session *codex.Session) (*tmuxCodexSession, error) {
	c.mu.Lock()
	state := c.sessions[session]
	if state == nil {
		state = newTmuxCodexSession(c.workdir)
		c.sessions[session] = state
	}
	c.mu.Unlock()

	if session.Started {
		return state, nil
	}
	pane, err := c.controller.NewPane(ctx, tmuxagent.PaneSpec{
		Name:    c.name,
		Agent:   "codex",
		Command: c.command,
		Args:    c.args,
		Workdir: c.workdir,
	})
	if err != nil {
		return nil, err
	}
	state.pane = pane

	readyCtx, cancel := context.WithTimeout(ctx, c.startupTimeout)
	defer cancel()
	if err := state.completeStartup(readyCtx); err != nil {
		_ = state.close(context.Background())
		return nil, state.withDiagnostics(ctx, err)
	}
	session.Started = true
	return state, nil
}

func newTmuxCodexSession(workdir string) *tmuxCodexSession {
	idleTitle := filepath.Base(filepath.Clean(workdir))
	if idleTitle == "." || idleTitle == string(filepath.Separator) || idleTitle == "" {
		idleTitle = "codex"
	}
	return &tmuxCodexSession{
		idleTitle:       idleTitle,
		busyTransitions: 1,
		currentState:    "busy",
	}
}

func (s *tmuxCodexSession) sendPrompt(ctx context.Context, prompt string) error {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	if err := s.submitPrompt(ctx, prompt, snapshot.busyTransitions); err != nil {
		return err
	}
	if err := s.waitForIdleTransition(ctx, snapshot.idleTransitions, snapshot.busyTransitions); err != nil {
		return fmt.Errorf("wait for codex tmux tui to become idle: %w", err)
	}
	return nil
}

func (s *tmuxCodexSession) submitPrompt(ctx context.Context, prompt string, busyStart int) error {
	normalized := ptyrunner.NormalizePrompt(prompt)
	if normalized == "" {
		return fmt.Errorf("codex tmux tui prompt is empty after normalization")
	}
	payload := ptyrunner.BracketedPasteStart + normalized + ptyrunner.BracketedPasteEnd
	if err := s.pane.Paste(ctx, payload); err != nil {
		return fmt.Errorf("write prompt to codex tmux tui: %w", err)
	}
	if err := ptyrunner.Sleep(ctx, tmuxSubmitKeyDelay); err != nil {
		return err
	}
	for range tmuxSubmitMaxAttempts {
		if err := s.pane.SendEnter(ctx); err != nil {
			return fmt.Errorf("submit prompt to codex tmux tui: %w", err)
		}
		busy, err := s.waitForBusyTransition(ctx, busyStart, tmuxSubmitRetryInterval)
		if err != nil {
			return err
		}
		if busy {
			return nil
		}
	}
	return fmt.Errorf("%w after %d enter presses", errCodexTmuxPromptNotAccepted, tmuxSubmitMaxAttempts)
}

func (s *tmuxCodexSession) completeStartup(ctx context.Context) error {
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	for {
		snapshot, err := s.snapshot(ctx)
		if err != nil {
			return err
		}
		if snapshot.currentState == "idle" {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *tmuxCodexSession) snapshot(ctx context.Context) (tmuxTitleSnapshot, error) {
	title, err := s.pane.Title(ctx)
	if err != nil {
		return tmuxTitleSnapshot{}, err
	}
	if state, ok := s.classifyTitle(title); ok {
		s.recordState(state)
	}
	return tmuxTitleSnapshot{
		idleTransitions: s.idleTransitions,
		busyTransitions: s.busyTransitions,
		currentState:    s.currentState,
	}, nil
}

func (s *tmuxCodexSession) classifyTitle(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", false
	}
	if trimmed == s.idleTitle {
		return "idle", true
	}
	return "busy", true
}

func (s *tmuxClaudeSession) waitForBusyTransition(ctx context.Context, busyStart int, timeout time.Duration) (bool, error) {
	return waitForTmuxBusy(ctx, s.pane, s.snapshot, busyStart, timeout, "claude")
}

func (s *tmuxCodexSession) waitForBusyTransition(ctx context.Context, busyStart int, timeout time.Duration) (bool, error) {
	return waitForTmuxBusy(ctx, s.pane, s.snapshot, busyStart, timeout, "codex")
}

func (s *tmuxClaudeSession) waitForIdleTransition(ctx context.Context, idleStart, busyStart int) error {
	return waitForTmuxIdle(ctx, s.pane, s.snapshot, idleStart, busyStart, "claude")
}

func (s *tmuxCodexSession) waitForIdleTransition(ctx context.Context, idleStart, busyStart int) error {
	return waitForTmuxIdle(ctx, s.pane, s.snapshot, idleStart, busyStart, "codex")
}

func waitForTmuxBusy(ctx context.Context, pane *tmuxagent.Pane, snapshot func(context.Context) (tmuxTitleSnapshot, error), busyStart int, timeout time.Duration, agent string) (bool, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	for {
		current, err := snapshot(ctx)
		if err != nil {
			return false, err
		}
		if current.busyTransitions > busyStart {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		if dead, err := pane.Dead(ctx); err != nil {
			return false, err
		} else if dead {
			return false, fmt.Errorf("%s tmux tui exited unexpectedly", agent)
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForTmuxIdle(ctx context.Context, pane *tmuxagent.Pane, snapshot func(context.Context) (tmuxTitleSnapshot, error), idleStart, busyStart int, agent string) error {
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	for {
		current, err := snapshot(ctx)
		if err != nil {
			return err
		}
		if current.busyTransitions > busyStart && current.idleTransitions > idleStart {
			return nil
		}
		if dead, err := pane.Dead(ctx); err != nil {
			return err
		} else if dead {
			return fmt.Errorf("%s tmux tui exited unexpectedly", agent)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *tmuxClaudeSession) recordState(state string) {
	if state == "" || state == s.currentState {
		return
	}
	s.currentState = state
	if state == "idle" {
		s.idleTransitions++
	} else {
		s.busyTransitions++
	}
}

func (s *tmuxCodexSession) recordState(state string) {
	if state == "" || state == s.currentState {
		return
	}
	s.currentState = state
	if state == "idle" {
		s.idleTransitions++
	} else {
		s.busyTransitions++
	}
}

func (s *tmuxClaudeSession) close(ctx context.Context) error {
	if s.pane == nil {
		return nil
	}
	if err := s.pane.Paste(ctx, "/exit"); err != nil {
		return err
	}
	if err := s.pane.SendEnter(ctx); err != nil {
		return err
	}
	return waitForTmuxClose(ctx, s.pane)
}

func (s *tmuxCodexSession) close(ctx context.Context) error {
	if s.pane == nil {
		return nil
	}
	if err := s.pane.SendCtrlD(ctx); err != nil {
		return err
	}
	return waitForTmuxClose(ctx, s.pane)
}

func waitForTmuxClose(ctx context.Context, pane *tmuxagent.Pane) error {
	closeCtx, cancel := context.WithTimeout(ctx, tmuxCloseTimeout)
	defer cancel()

	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()
	for {
		dead, err := pane.Dead(closeCtx)
		if err != nil {
			return err
		}
		if dead {
			return nil
		}
		select {
		case <-closeCtx.Done():
			_ = pane.Kill(context.Background())
			return closeCtx.Err()
		case <-ticker.C:
		}
	}
}

func (s *tmuxClaudeSession) withDiagnostics(ctx context.Context, err error) error {
	return tmuxPaneDiagnostics(ctx, s.pane, err)
}

func (s *tmuxCodexSession) withDiagnostics(ctx context.Context, err error) error {
	return tmuxPaneDiagnostics(ctx, s.pane, err)
}

func tmuxPaneDiagnostics(ctx context.Context, pane *tmuxagent.Pane, err error) error {
	if err == nil || pane == nil {
		return err
	}
	output, captureErr := pane.Capture(ctx, 160)
	if captureErr != nil || strings.TrimSpace(output) == "" {
		return err
	}
	return fmt.Errorf("%w; last tmux pane output:\n%s", err, strings.TrimSpace(output))
}

var errCodexTmuxPromptNotAccepted = fmt.Errorf("codex tmux tui did not start processing")
