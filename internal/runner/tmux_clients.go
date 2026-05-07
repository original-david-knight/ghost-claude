package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vibedrive/internal/diagnostics"
	"vibedrive/internal/tmuxagent"
	"vibedrive/pkg/agentcli/claude"
	"vibedrive/pkg/agentcli/codex"
	"vibedrive/pkg/ptyrunner"
)

const (
	tmuxCloseTimeout        = 5 * time.Second
	tmuxForceCloseTimeout   = 1 * time.Second
	tmuxStatePollInterval   = 50 * time.Millisecond
	tmuxSubmitKeyDelay      = 100 * time.Millisecond
	tmuxSubmitRetryInterval = 2 * time.Second
	tmuxSubmitMaxAttempts   = 3
	tmuxDiagnosticsTimeout  = 5 * time.Second
)

const (
	tmuxFailureStartupTimeout = "tmux_startup_timeout"
	tmuxFailureSubmitTimeout  = "tmux_submit_prompt_timeout"
	tmuxFailureUnexpectedExit = "tmux_unexpected_exit"
)

type tmuxClaudeClient struct {
	command        string
	args           []string
	workdir        string
	startupTimeout time.Duration
	controller     *tmuxagent.Controller
	capturer       *diagnostics.Capturer
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
	capturer       *diagnostics.Capturer
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
	diag                tmuxSessionDiagnostics
}

type tmuxCodexSession struct {
	pane                *tmuxagent.Pane
	idleTitle           string
	idleTransitions     int
	busyTransitions     int
	trustPrompts        int
	trustPromptDetected bool
	currentState        string
	diag                tmuxSessionDiagnostics
}

type tmuxTitleSnapshot struct {
	idleTransitions int
	busyTransitions int
	trustPrompts    int
	currentState    string
}

type tmuxSessionDiagnostics struct {
	capturer       *diagnostics.Capturer
	agent          string
	command        string
	args           []string
	workdir        string
	startupTimeout time.Duration

	titleHistory []diagnostics.TitleEvent
	titleReadErr string

	lastPromptRaw        []byte
	lastPromptNormalized []byte
	lastPromptBracketed  bool
	lastPromptInputBytes int
	lastSubmitAttempts   int

	createdAt            time.Time
	startupStartedAt     time.Time
	startupCompletedAt   time.Time
	lastPromptStartedAt  time.Time
	lastPromptPastedAt   time.Time
	lastPromptAcceptedAt time.Time
	lastIdleAt           time.Time
	lastFailureAt        time.Time
}

type tmuxDiagnosticsContextValue struct {
	identity     diagnostics.Identity
	parentStdout diagnostics.ByteArtifact
	parentStderr diagnostics.ByteArtifact
}

type tmuxDiagnosticsContextKey struct{}

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
		capturer:       diagnostics.New(workdir),
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
		capturer:       diagnostics.New(workdir),
		name:           name,
		sessions:       make(map[*codex.Session]*tmuxCodexSession),
	}, nil
}

func withTmuxDiagnosticsIdentity(ctx context.Context, runID, taskID, stepName string) context.Context {
	value := tmuxDiagnosticsContextFrom(ctx)
	value.identity = diagnostics.Identity{
		RunID:    runID,
		TaskID:   taskID,
		StepName: stepName,
	}
	return context.WithValue(ctx, tmuxDiagnosticsContextKey{}, value)
}

func withTmuxDiagnosticsParentOutput(ctx context.Context, stdout, stderr diagnostics.ByteArtifact) context.Context {
	value := tmuxDiagnosticsContextFrom(ctx)
	value.parentStdout = stdout
	value.parentStderr = stderr
	return context.WithValue(ctx, tmuxDiagnosticsContextKey{}, value)
}

func tmuxDiagnosticsContextFrom(ctx context.Context) tmuxDiagnosticsContextValue {
	if ctx == nil {
		return tmuxDiagnosticsContextValue{}
	}
	if value, ok := ctx.Value(tmuxDiagnosticsContextKey{}).(tmuxDiagnosticsContextValue); ok {
		return value
	}
	return tmuxDiagnosticsContextValue{}
}

func tmuxDiagnosticsIdentity(ctx context.Context, agent, failurePath string) diagnostics.Identity {
	value := tmuxDiagnosticsContextFrom(ctx)
	id := value.identity
	if strings.TrimSpace(id.RunID) != "" &&
		strings.TrimSpace(id.TaskID) != "" &&
		strings.TrimSpace(id.StepName) != "" {
		return id
	}
	if strings.TrimSpace(agent) == "" {
		agent = "tmux"
	}
	if strings.TrimSpace(failurePath) == "" {
		failurePath = "tmux_failure"
	}
	return diagnostics.Identity{
		RunID:    "unknown-run",
		TaskID:   agent + "-tui",
		StepName: failurePath,
	}
}

func tmuxParentArtifacts(ctx context.Context) (diagnostics.ByteArtifact, diagnostics.ByteArtifact) {
	value := tmuxDiagnosticsContextFrom(ctx)
	stdout := value.parentStdout
	if !stdout.Available {
		stdout = diagnostics.UnavailableBytes()
	}
	stderr := value.parentStderr
	if !stderr.Available {
		stderr = diagnostics.UnavailableBytes()
	}
	return stdout, stderr
}

func newTmuxSessionDiagnostics(capturer *diagnostics.Capturer, agent, command string, args []string, workdir string, startupTimeout time.Duration) tmuxSessionDiagnostics {
	if capturer == nil {
		capturer = diagnostics.New(workdir)
	}
	return tmuxSessionDiagnostics{
		capturer:       capturer,
		agent:          agent,
		command:        command,
		args:           append([]string{}, args...),
		workdir:        workdir,
		startupTimeout: startupTimeout,
		createdAt:      time.Now(),
	}
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
		return state.withDiagnostics(ctx, tmuxPromptFailurePath(err), err)
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
		state = newTmuxClaudeSession(c.capturer, c.command, c.args, c.workdir, c.startupTimeout)
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

	state.diag.startupStartedAt = time.Now()
	readyCtx, cancel := context.WithTimeout(ctx, c.startupTimeout)
	defer cancel()
	if err := state.completeStartup(readyCtx); err != nil {
		diagErr := state.withDiagnostics(ctx, tmuxFailureStartupTimeout, fmt.Errorf("wait for claude tmux tui startup (%s): %w", c.startupTimeout, err))
		_ = state.close(context.Background())
		return nil, diagErr
	}
	state.diag.startupCompletedAt = time.Now()
	session.Started = true
	return state, nil
}

func newTmuxClaudeSession(capturer *diagnostics.Capturer, command string, args []string, workdir string, startupTimeout time.Duration) *tmuxClaudeSession {
	return &tmuxClaudeSession{
		diag: newTmuxSessionDiagnostics(capturer, "claude", command, args, workdir, startupTimeout),
	}
}

func (s *tmuxClaudeSession) sendPrompt(ctx context.Context, prompt string) error {
	s.diag.lastPromptStartedAt = time.Now()
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
	s.diag.lastIdleAt = time.Now()
	return nil
}

func (s *tmuxClaudeSession) submitPrompt(ctx context.Context, prompt string, busyStart int, bracketed bool) error {
	normalized := ptyrunner.NormalizePrompt(prompt)
	if normalized == "" {
		return fmt.Errorf("claude tmux tui prompt is empty after normalization")
	}
	payload := normalized
	if bracketed {
		payload = ptyrunner.BracketedPasteStart + normalized + ptyrunner.BracketedPasteEnd
	}
	s.recordPromptPayload(prompt, payload, normalized, bracketed)
	if err := s.pane.Paste(ctx, payload); err != nil {
		return fmt.Errorf("write prompt to claude tmux tui: %w", err)
	}
	s.diag.lastPromptPastedAt = time.Now()
	if err := ptyrunner.Sleep(ctx, tmuxSubmitKeyDelay); err != nil {
		return err
	}
	for range tmuxSubmitMaxAttempts {
		s.diag.lastSubmitAttempts++
		if err := s.pane.SendEnter(ctx); err != nil {
			return fmt.Errorf("submit prompt to claude tmux tui: %w", err)
		}
		busy, err := s.waitForBusyTransition(ctx, busyStart, tmuxSubmitRetryInterval)
		if err != nil {
			return err
		}
		if busy {
			s.diag.lastPromptAcceptedAt = time.Now()
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
		s.diag.titleReadErr = err.Error()
		return tmuxTitleSnapshot{}, err
	}
	if state, ok := classifyClaudeTmuxTitle(title); ok {
		s.recordState("title", title, state)
	}
	if text, err := s.pane.Capture(ctx, 80); err == nil {
		trustDetected := strings.Contains(ptyrunner.CompactVisibleText([]byte(text)), "yesitrustthisfolder")
		if trustDetected && !s.trustPromptDetected {
			s.trustPrompts++
			s.recordTitleEvent("trust_prompt", "", "trust_prompt")
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
		return state.withDiagnostics(ctx, tmuxPromptFailurePath(err), err)
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
		state.diag = newTmuxSessionDiagnostics(c.capturer, "codex", c.command, c.args, c.workdir, c.startupTimeout)
		state.recordTitleEvent("transport", "", state.currentState)
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

	state.diag.startupStartedAt = time.Now()
	readyCtx, cancel := context.WithTimeout(ctx, c.startupTimeout)
	defer cancel()
	if err := state.completeStartup(readyCtx); err != nil {
		diagErr := state.withDiagnostics(ctx, tmuxFailureStartupTimeout, fmt.Errorf("wait for codex tmux tui startup (%s): %w", c.startupTimeout, err))
		_ = state.close(context.Background())
		return nil, diagErr
	}
	state.diag.startupCompletedAt = time.Now()
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
	s.diag.lastPromptStartedAt = time.Now()
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
	s.diag.lastIdleAt = time.Now()
	return nil
}

func (s *tmuxCodexSession) submitPrompt(ctx context.Context, prompt string, busyStart int) error {
	normalized := ptyrunner.NormalizePrompt(prompt)
	if normalized == "" {
		return fmt.Errorf("codex tmux tui prompt is empty after normalization")
	}
	payload := ptyrunner.BracketedPasteStart + normalized + ptyrunner.BracketedPasteEnd
	s.recordPromptPayload(prompt, payload, normalized, true)
	if err := s.pane.Paste(ctx, payload); err != nil {
		return fmt.Errorf("write prompt to codex tmux tui: %w", err)
	}
	s.diag.lastPromptPastedAt = time.Now()
	if err := ptyrunner.Sleep(ctx, tmuxSubmitKeyDelay); err != nil {
		return err
	}
	for range tmuxSubmitMaxAttempts {
		s.diag.lastSubmitAttempts++
		if err := s.pane.SendEnter(ctx); err != nil {
			return fmt.Errorf("submit prompt to codex tmux tui: %w", err)
		}
		busy, err := s.waitForBusyTransition(ctx, busyStart, tmuxSubmitRetryInterval)
		if err != nil {
			return err
		}
		if busy {
			s.diag.lastPromptAcceptedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("%w after %d enter presses", errCodexTmuxPromptNotAccepted, tmuxSubmitMaxAttempts)
}

func (s *tmuxCodexSession) completeStartup(ctx context.Context) error {
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	handledTrustPrompts := 0
	for {
		snapshot, err := s.snapshot(ctx)
		if err != nil {
			return err
		}
		if snapshot.trustPrompts > handledTrustPrompts {
			if err := s.pane.SendEnter(ctx); err != nil {
				return fmt.Errorf("confirm codex trust dialog: %w", err)
			}
			handledTrustPrompts = snapshot.trustPrompts
			continue
		}
		if snapshot.currentState == "idle" {
			return nil
		}
		if text, err := s.pane.Capture(ctx, 80); err == nil && codexReadyScreen(text) {
			s.recordState("screen", "", "idle")
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
		s.diag.titleReadErr = err.Error()
		return tmuxTitleSnapshot{}, err
	}
	if text, err := s.pane.Capture(ctx, 80); err == nil {
		trustDetected := codexTrustPrompt(text)
		if trustDetected && !s.trustPromptDetected {
			s.trustPrompts++
			s.recordTitleEvent("trust_prompt", "", "trust_prompt")
		}
		s.trustPromptDetected = trustDetected
		if trustDetected {
			return tmuxTitleSnapshot{
				idleTransitions: s.idleTransitions,
				busyTransitions: s.busyTransitions,
				trustPrompts:    s.trustPrompts,
				currentState:    s.currentState,
			}, nil
		}
		if state, ok := codexScreenState(text); ok {
			s.recordState("screen", title, state)
			return tmuxTitleSnapshot{
				idleTransitions: s.idleTransitions,
				busyTransitions: s.busyTransitions,
				trustPrompts:    s.trustPrompts,
				currentState:    s.currentState,
			}, nil
		}
	}
	if state, ok := s.classifyTitle(title); ok {
		s.recordState("title", title, state)
	}
	return tmuxTitleSnapshot{
		idleTransitions: s.idleTransitions,
		busyTransitions: s.busyTransitions,
		trustPrompts:    s.trustPrompts,
		currentState:    s.currentState,
	}, nil
}

func (s *tmuxCodexSession) classifyTitle(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", false
	}
	if isCodexBusyTitle(trimmed) {
		return "busy", true
	}
	if codexTitleMatchesIdle(trimmed, s.idleTitle) {
		return "idle", true
	}
	if codexTitleLooksIdleStatus(trimmed) {
		return "idle", true
	}
	return "busy", true
}

func isCodexBusyTitle(title string) bool {
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

func codexTitleMatchesIdle(title, idleTitle string) bool {
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
	if codexTitleAbbreviatesIdle(title, idleTitle) {
		return true
	}
	return strings.Contains(title, idleTitle)
}

func codexTitleAbbreviatesIdle(title, idleTitle string) bool {
	for _, marker := range []string{"...", "…"} {
		if !strings.Contains(title, marker) {
			continue
		}
		parts := strings.Split(title, marker)
		prefix := strings.TrimSpace(parts[0])
		suffix := strings.TrimSpace(parts[len(parts)-1])
		if codexIdleTitleHasPrefix(idleTitle, prefix) || codexIdleTitleHasSuffix(idleTitle, suffix) {
			return true
		}
	}
	return false
}

func codexIdleTitleHasPrefix(idleTitle, prefix string) bool {
	if !codexIdleFragmentLongEnough(idleTitle, prefix) {
		return false
	}
	if strings.HasPrefix(idleTitle, prefix) {
		return true
	}
	base := filepath.Base(filepath.Clean(prefix))
	return base != prefix && codexIdleFragmentLongEnough(idleTitle, base) && strings.HasPrefix(idleTitle, base)
}

func codexIdleTitleHasSuffix(idleTitle, suffix string) bool {
	if !codexIdleFragmentLongEnough(idleTitle, suffix) {
		return false
	}
	if strings.HasSuffix(idleTitle, suffix) {
		return true
	}
	base := filepath.Base(filepath.Clean(suffix))
	return base != suffix && codexIdleFragmentLongEnough(idleTitle, base) && strings.HasSuffix(idleTitle, base)
}

func codexIdleFragmentLongEnough(idleTitle, fragment string) bool {
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

func codexTitleLooksIdleStatus(title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return false
	}
	return (strings.Contains(title, " · ") || strings.Contains(title, " • ")) && (strings.Contains(title, "/") || strings.Contains(title, "~"))
}

func codexReadyScreen(text string) bool {
	return codex.ReadyScreen(text)
}

func codexTrustPrompt(text string) bool {
	compact := strings.ToLower(ptyrunner.CompactVisibleText([]byte(text)))
	return strings.Contains(compact, "doyoutrustthecontentsofthisdirectory") &&
		strings.Contains(compact, "yescontinue") &&
		strings.Contains(compact, "noquit")
}

func codexScreenState(text string) (string, bool) {
	return codex.ScreenState(text)
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

func (s *tmuxClaudeSession) recordState(source, title, state string) {
	if state == "" || state == s.currentState {
		return
	}
	s.currentState = state
	if state == "idle" {
		s.idleTransitions++
	} else {
		s.busyTransitions++
	}
	s.recordTitleEvent(source, title, state)
}

func (s *tmuxCodexSession) recordState(source, title, state string) {
	if state == "" || state == s.currentState {
		return
	}
	s.currentState = state
	if state == "idle" {
		s.idleTransitions++
	} else {
		s.busyTransitions++
	}
	s.recordTitleEvent(source, title, state)
}

func (s *tmuxClaudeSession) recordTitleEvent(source, title, state string) {
	s.diag.titleHistory = appendTmuxTitleEvent(s.diag.titleHistory, diagnostics.TitleEvent{
		TS:              time.Now(),
		Source:          source,
		Title:           title,
		State:           state,
		IdleTransitions: s.idleTransitions,
		BusyTransitions: s.busyTransitions,
		TrustPrompts:    s.trustPrompts,
	})
}

func (s *tmuxCodexSession) recordTitleEvent(source, title, state string) {
	s.diag.titleHistory = appendTmuxTitleEvent(s.diag.titleHistory, diagnostics.TitleEvent{
		TS:              time.Now(),
		Source:          source,
		Title:           title,
		State:           state,
		IdleTransitions: s.idleTransitions,
		BusyTransitions: s.busyTransitions,
		TrustPrompts:    s.trustPrompts,
	})
}

func appendTmuxTitleEvent(events []diagnostics.TitleEvent, event diagnostics.TitleEvent) []diagnostics.TitleEvent {
	if strings.TrimSpace(event.Source) == "" {
		event.Source = "transport"
	}
	events = append(events, event)
	if len(events) > diagnostics.TitleEventLimit {
		events = events[len(events)-diagnostics.TitleEventLimit:]
	}
	return events
}

func (s *tmuxClaudeSession) close(ctx context.Context) error {
	if s.pane == nil {
		return nil
	}
	if err := s.pane.Paste(ctx, "/exit"); err != nil {
		if tmuxagent.IsTargetMissingError(err) {
			return nil
		}
		return err
	}
	if err := s.pane.SendEnter(ctx); err != nil {
		if tmuxagent.IsTargetMissingError(err) {
			return nil
		}
		return err
	}
	return waitForTmuxClose(ctx, s.pane)
}

func (s *tmuxCodexSession) close(ctx context.Context) error {
	if s.pane == nil {
		return nil
	}
	if err := s.pane.SendCtrlD(ctx); err != nil {
		if tmuxagent.IsTargetMissingError(err) {
			return nil
		}
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
			if closeCtx.Err() != nil {
				return forceKillTmuxPane(pane)
			}
			return err
		}
		if dead {
			return nil
		}
		select {
		case <-closeCtx.Done():
			return forceKillTmuxPane(pane)
		case <-ticker.C:
		}
	}
}

func forceKillTmuxPane(pane *tmuxagent.Pane) error {
	killCtx, cancel := context.WithTimeout(context.Background(), tmuxForceCloseTimeout)
	defer cancel()
	if err := pane.Kill(killCtx); err != nil {
		return fmt.Errorf("force close tmux pane after timeout: %w", err)
	}
	return nil
}

func (s *tmuxClaudeSession) withDiagnostics(ctx context.Context, failurePath string, err error) error {
	if err == nil {
		return nil
	}
	s.diag.lastFailureAt = time.Now()
	return captureTmuxSessionDiagnostics(ctx, s.pane, s.diag, s.tmuxMetadata(), failurePath, err)
}

func (s *tmuxCodexSession) withDiagnostics(ctx context.Context, failurePath string, err error) error {
	if err == nil {
		return nil
	}
	s.diag.lastFailureAt = time.Now()
	return captureTmuxSessionDiagnostics(ctx, s.pane, s.diag, s.tmuxMetadata(), failurePath, err)
}

func (s *tmuxClaudeSession) tmuxMetadata() diagnostics.TmuxMetadata {
	return s.diag.metadata(s.pane, s.idleTransitions, s.busyTransitions, s.trustPrompts, s.currentState)
}

func (s *tmuxCodexSession) tmuxMetadata() diagnostics.TmuxMetadata {
	return s.diag.metadata(s.pane, s.idleTransitions, s.busyTransitions, s.trustPrompts, s.currentState)
}

func captureTmuxSessionDiagnostics(ctx context.Context, pane *tmuxagent.Pane, diag tmuxSessionDiagnostics, metadata diagnostics.TmuxMetadata, failurePath string, err error) error {
	if err == nil {
		return nil
	}
	if strings.TrimSpace(failurePath) == "" {
		failurePath = tmuxFailureSubmitTimeout
	}
	capturer := diag.capturer
	if capturer == nil {
		capturer = diagnostics.New(diag.workdir)
	}

	identity := tmuxDiagnosticsIdentity(ctx, diag.agent, failurePath)
	stepDir, stepDirErr := capturer.StepDir(identity)
	if stepDirErr != nil {
		stepDir = capturer.DebugRoot()
	}

	captureCtx, cancel := context.WithTimeout(context.Background(), tmuxDiagnosticsTimeout)
	defer cancel()

	paneArtifact := diagnostics.UnavailableBytes()
	if pane != nil {
		output, captureErr := pane.Capture(captureCtx, diagnostics.PaneLineLimit)
		if captureErr != nil {
			metadata.Extra = mergeMetadataExtra(metadata.Extra, "pane_capture_error", captureErr.Error())
		} else {
			paneArtifact = diagnostics.Bytes([]byte(output))
		}
	}

	parentStdout, parentStderr := tmuxParentArtifacts(ctx)
	result, captureErr := capturer.CaptureTmux(diagnostics.TmuxCapture{
		Identity: identity,
		Failure: diagnostics.Failure{
			Path:       failurePath,
			Message:    err.Error(),
			CapturedAt: diag.lastFailureAt,
		},
		Transport: diagnostics.Transport{
			Kind:        "tmux",
			Agent:       diag.agent,
			Interactive: true,
		},
		Pane:              paneArtifact,
		TitleHistory:      append([]diagnostics.TitleEvent(nil), diag.titleHistory...),
		TitleHistoryKnown: len(diag.titleHistory) > 0 || diag.titleReadErr == "",
		TitleHistoryError: diag.titleReadErr,
		Metadata:          metadata,
		Prompt:            diag.promptPayload(),
		ParentStdout:      parentStdout,
		ParentStderr:      parentStderr,
	})
	if result.Dir != "" {
		stepDir = result.Dir
	}
	if captureErr != nil {
		return fmt.Errorf("%w; tmux diagnostics capture failed for %s: %v", err, stepDir, captureErr)
	}
	return fmt.Errorf("%w; tmux diagnostics captured at %s", err, stepDir)
}

func tmuxPromptFailurePath(err error) string {
	if err != nil && strings.Contains(err.Error(), "exited unexpectedly") {
		return tmuxFailureUnexpectedExit
	}
	return tmuxFailureSubmitTimeout
}

func (s *tmuxClaudeSession) recordPromptPayload(input, raw, normalized string, bracketed bool) {
	s.diag.recordPromptPayload(input, raw, normalized, bracketed)
}

func (s *tmuxCodexSession) recordPromptPayload(input, raw, normalized string, bracketed bool) {
	s.diag.recordPromptPayload(input, raw, normalized, bracketed)
}

func (d *tmuxSessionDiagnostics) recordPromptPayload(input, raw, normalized string, bracketed bool) {
	d.lastPromptRaw = []byte(raw)
	d.lastPromptNormalized = []byte(normalized)
	d.lastPromptBracketed = bracketed
	d.lastPromptInputBytes = len([]byte(input))
	d.lastSubmitAttempts = 0
}

func (d tmuxSessionDiagnostics) promptPayload() diagnostics.PromptPayload {
	raw := diagnostics.UnavailableBytes()
	if d.lastPromptRaw != nil {
		raw = diagnostics.Bytes(d.lastPromptRaw)
	}
	normalized := diagnostics.UnavailableBytes()
	if d.lastPromptNormalized != nil {
		normalized = diagnostics.Bytes(d.lastPromptNormalized)
	}
	return diagnostics.PromptPayload{
		Raw:               raw,
		Normalized:        normalized,
		NormalizationMode: "ptyrunner.NormalizePrompt",
		BracketedPaste:    d.lastPromptBracketed,
		ExtraMetadata: map[string]any{
			"input_bytes":              d.lastPromptInputBytes,
			"submit_attempts_observed": d.lastSubmitAttempts,
		},
	}
}

func (d tmuxSessionDiagnostics) metadata(pane *tmuxagent.Pane, idleTransitions, busyTransitions, trustPrompts int, finalState string) diagnostics.TmuxMetadata {
	paneTarget := ""
	if pane != nil {
		paneTarget = pane.Target
	}
	return diagnostics.TmuxMetadata{
		PaneTarget:      paneTarget,
		Agent:           d.agent,
		Command:         d.command,
		Args:            append([]string{}, d.args...),
		Workdir:         d.workdir,
		StartupTimeout:  d.startupTimeout.String(),
		IdleTransitions: idleTransitions,
		BusyTransitions: busyTransitions,
		TrustPrompts:    trustPrompts,
		FinalState:      finalState,
		TitleReadError:  d.titleReadErr,
		Extra: map[string]any{
			"timing": d.timingMetadata(),
			"transition_counters": map[string]any{
				"idle":          idleTransitions,
				"busy":          busyTransitions,
				"trust_prompts": trustPrompts,
				"final_state":   finalState,
			},
		},
	}
}

func (d tmuxSessionDiagnostics) timingMetadata() map[string]any {
	timing := map[string]any{
		"submit_retry_interval_ms": tmuxSubmitRetryInterval.Milliseconds(),
		"submit_key_delay_ms":      tmuxSubmitKeyDelay.Milliseconds(),
		"submit_max_attempts":      tmuxSubmitMaxAttempts,
		"observed_submit_attempts": d.lastSubmitAttempts,
	}
	addTime := func(key string, value time.Time) {
		if !value.IsZero() {
			timing[key] = value.UTC().Format(time.RFC3339Nano)
		}
	}
	addTime("session_created_at", d.createdAt)
	addTime("startup_started_at", d.startupStartedAt)
	addTime("startup_completed_at", d.startupCompletedAt)
	addTime("last_prompt_started_at", d.lastPromptStartedAt)
	addTime("last_prompt_pasted_at", d.lastPromptPastedAt)
	addTime("last_prompt_accepted_at", d.lastPromptAcceptedAt)
	addTime("last_idle_at", d.lastIdleAt)
	addTime("last_failure_at", d.lastFailureAt)
	if !d.startupStartedAt.IsZero() {
		end := d.startupCompletedAt
		if end.IsZero() {
			end = d.lastFailureAt
		}
		if !end.IsZero() {
			timing["startup_elapsed_ms"] = end.Sub(d.startupStartedAt).Milliseconds()
		}
	}
	if !d.lastPromptStartedAt.IsZero() {
		end := d.lastIdleAt
		if end.IsZero() {
			end = d.lastFailureAt
		}
		if !end.IsZero() {
			timing["last_prompt_elapsed_ms"] = end.Sub(d.lastPromptStartedAt).Milliseconds()
		}
	}
	return timing
}

func mergeMetadataExtra(extra map[string]any, key string, value any) map[string]any {
	if extra == nil {
		extra = make(map[string]any)
	}
	extra[key] = value
	return extra
}

var errCodexTmuxPromptNotAccepted = fmt.Errorf("codex tmux tui did not start processing")
