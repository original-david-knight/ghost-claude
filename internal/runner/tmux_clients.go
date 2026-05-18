package runner

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
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

var tmuxPaneRedrawInterval = 5 * time.Second

const (
	tmuxFailureStartupTimeout     = "tmux_startup_timeout"
	tmuxFailureSubmitTimeout      = "tmux_submit_prompt_timeout"
	tmuxFailureSubmitUnknownState = "tmux_submit_prompt_unknown_state"
	tmuxFailureUnexpectedExit     = "tmux_unexpected_exit"
	tmuxObservedStateUnknown      = "unknown"
	tmuxObservedStateTrustPrompt  = "trust_prompt"
)

type tmuxClaudeClient struct {
	command             string
	args                []string
	workdir             string
	startupTimeout      time.Duration
	idleActivityTimeout time.Duration
	controller          *tmuxagent.Controller
	capturer            *diagnostics.Capturer
	name                string

	mu       sync.Mutex
	sessions map[*claude.Session]*tmuxClaudeSession
}

type tmuxCodexClient struct {
	command             string
	args                []string
	workdir             string
	startupTimeout      time.Duration
	idleActivityTimeout time.Duration
	controller          *tmuxagent.Controller
	capturer            *diagnostics.Capturer
	name                string

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
	idleActivityTimeout time.Duration
	diag                tmuxSessionDiagnostics
}

type tmuxCodexSession struct {
	pane                *tmuxagent.Pane
	idleTitle           string
	idleTitles          []string
	idleTransitions     int
	busyTransitions     int
	trustPrompts        int
	trustPromptDetected bool
	currentState        string
	idleActivityTimeout time.Duration
	diag                tmuxSessionDiagnostics
}

type tmuxTitleSnapshot struct {
	idleTransitions    int
	busyTransitions    int
	trustPrompts       int
	currentState       string
	observedState      string
	observedSource     string
	observedTitle      string
	classified         bool
	captureFingerprint uint64
	captureValid       bool
}

type tmuxSessionDiagnostics struct {
	capturer            *diagnostics.Capturer
	agent               string
	command             string
	args                []string
	workdir             string
	startupTimeout      time.Duration
	submitRetryInterval time.Duration

	titleHistory []diagnostics.TitleEvent
	titleReadErr string

	lastPromptRaw        []byte
	lastPromptNormalized []byte
	lastPromptBracketed  bool
	lastPromptInputBytes int
	lastSubmitAttempts   int
	lastObservedState    string
	lastObservedSource   string
	lastObservedTitle    string
	lastObservedKnown    bool

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

type tmuxBusyWaitResult struct {
	startedProcessing bool
	lastSnapshot      tmuxTitleSnapshot
}

type tmuxSubmitStateError struct {
	agent   string
	attempt int
	state   string
	source  string
}

type tmuxSubmitReadyError struct {
	agent  string
	state  string
	source string
}

func (e *tmuxSubmitStateError) Error() string {
	agent := strings.TrimSpace(e.agent)
	if agent == "" {
		agent = "tmux"
	}
	state := strings.TrimSpace(e.state)
	if state == "" {
		state = tmuxObservedStateUnknown
	}
	source := strings.TrimSpace(e.source)
	if source == "" {
		source = "transport"
	}
	attempt := e.attempt
	if attempt < 1 {
		attempt = 1
	}
	if state == tmuxObservedStateUnknown {
		return fmt.Sprintf("%s tmux tui submit aborted after enter press %d: unclassified TUI state %q from %s; refusing blind retry because the prompt may already be accepted", agent, attempt, state, source)
	}
	return fmt.Sprintf("%s tmux tui submit aborted after enter press %d: TUI state %q from %s is not accepting input; refusing blind retry because the prompt may already be accepted", agent, attempt, state, source)
}

func (e *tmuxSubmitReadyError) Error() string {
	agent := strings.TrimSpace(e.agent)
	if agent == "" {
		agent = "tmux"
	}
	state := strings.TrimSpace(e.state)
	if state == "" {
		state = tmuxObservedStateUnknown
	}
	source := strings.TrimSpace(e.source)
	if source == "" {
		source = "transport"
	}
	if state == tmuxObservedStateUnknown {
		return fmt.Sprintf("%s tmux tui is not ready for prompt submission: unclassified TUI state %q from %s", agent, state, source)
	}
	return fmt.Sprintf("%s tmux tui is not ready for prompt submission: TUI state %q from %s is not accepting input", agent, state, source)
}

func (s tmuxTitleSnapshot) acceptingInputForSubmitRetry() bool {
	return s.classified && s.observedState == "idle"
}

func (s tmuxTitleSnapshot) readyForPromptSubmission() bool {
	return s.acceptingInputForSubmitRetry() || s.currentState == "idle"
}

func (s tmuxTitleSnapshot) submitRetryState() string {
	if strings.TrimSpace(s.observedState) != "" {
		return s.observedState
	}
	return tmuxObservedStateUnknown
}

func (s tmuxTitleSnapshot) submitRetrySource() string {
	if strings.TrimSpace(s.observedSource) != "" {
		return s.observedSource
	}
	return "transport"
}

func newTmuxClaudeClient(command string, args []string, workdir, startupTimeout, idleActivityTimeout string, controller *tmuxagent.Controller, name string) (*tmuxClaudeClient, error) {
	timeout, err := time.ParseDuration(startupTimeout)
	if err != nil {
		return nil, fmt.Errorf("parse claude.startup_timeout %q: %w", startupTimeout, err)
	}
	idleActivity, err := parseIdleActivityTimeout(idleActivityTimeout, "claude.idle_activity_timeout")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	return &tmuxClaudeClient{
		command:             command,
		args:                append([]string{}, args...),
		workdir:             workdir,
		startupTimeout:      timeout,
		idleActivityTimeout: idleActivity,
		controller:          controller,
		capturer:            diagnostics.New(workdir),
		name:                name,
		sessions:            make(map[*claude.Session]*tmuxClaudeSession),
	}, nil
}

func newTmuxCodexClient(command string, args []string, workdir, startupTimeout, idleActivityTimeout string, controller *tmuxagent.Controller, name string) (*tmuxCodexClient, error) {
	timeout := 30 * time.Second
	if strings.TrimSpace(startupTimeout) != "" {
		parsed, err := time.ParseDuration(startupTimeout)
		if err != nil {
			return nil, fmt.Errorf("parse codex.startup_timeout %q: %w", startupTimeout, err)
		}
		timeout = parsed
	}
	idleActivity, err := parseIdleActivityTimeout(idleActivityTimeout, "codex.idle_activity_timeout")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}
	return &tmuxCodexClient{
		command:             command,
		args:                append([]string{}, args...),
		workdir:             workdir,
		startupTimeout:      timeout,
		idleActivityTimeout: idleActivity,
		controller:          controller,
		capturer:            diagnostics.New(workdir),
		name:                name,
		sessions:            make(map[*codex.Session]*tmuxCodexSession),
	}, nil
}

func parseIdleActivityTimeout(value, field string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", field, value, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must be >= 0", field)
	}
	return d, nil
}

func tmuxCaptureFingerprint(text string) uint64 {
	if text == "" {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	sum := h.Sum64()
	if sum == 0 {
		sum = 1
	}
	return sum
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
		capturer:            capturer,
		agent:               agent,
		command:             command,
		args:                append([]string{}, args...),
		workdir:             workdir,
		startupTimeout:      startupTimeout,
		submitRetryInterval: tmuxSubmitRetryInterval,
		createdAt:           time.Now(),
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
		state = newTmuxClaudeSession(c.capturer, c.command, c.args, c.workdir, c.startupTimeout, c.idleActivityTimeout)
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

func newTmuxClaudeSession(capturer *diagnostics.Capturer, command string, args []string, workdir string, startupTimeout, idleActivityTimeout time.Duration) *tmuxClaudeSession {
	return &tmuxClaudeSession{
		idleActivityTimeout: idleActivityTimeout,
		diag:                newTmuxSessionDiagnostics(capturer, "claude", command, args, workdir, startupTimeout),
	}
}

func (s *tmuxClaudeSession) sendPrompt(ctx context.Context, prompt string) error {
	s.diag.lastPromptStartedAt = time.Now()
	snapshot, err := s.waitForSubmitReady(ctx)
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
	for attempt := 1; attempt <= tmuxSubmitMaxAttempts; attempt++ {
		s.diag.lastSubmitAttempts++
		if err := s.pane.SendEnter(ctx); err != nil {
			return fmt.Errorf("submit prompt to claude tmux tui: %w", err)
		}
		result, err := s.waitForBusyTransition(ctx, busyStart, s.diag.effectiveSubmitRetryInterval())
		if err != nil {
			return err
		}
		if result.startedProcessing {
			s.diag.lastPromptAcceptedAt = time.Now()
			return nil
		}
		if !result.lastSnapshot.acceptingInputForSubmitRetry() {
			return &tmuxSubmitStateError{
				agent:   "claude",
				attempt: attempt,
				state:   result.lastSnapshot.submitRetryState(),
				source:  result.lastSnapshot.submitRetrySource(),
			}
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
	observedSource := "title"
	observedState := tmuxObservedStateUnknown
	observedKnown := false
	if state, ok := classifyClaudeTmuxTitle(title); ok {
		s.recordState("title", title, state)
		observedState = state
		observedKnown = true
	}
	var fingerprint uint64
	captureValid := false
	if text, err := s.pane.Capture(ctx, 80); err == nil {
		fingerprint = tmuxCaptureFingerprint(text)
		captureValid = true
		trustDetected := strings.Contains(ptyrunner.CompactVisibleText([]byte(text)), "yesitrustthisfolder")
		if trustDetected && !s.trustPromptDetected {
			s.trustPrompts++
			s.recordTitleEvent("trust_prompt", "", "trust_prompt")
		}
		s.trustPromptDetected = trustDetected
		if trustDetected {
			observedSource = "trust_prompt"
			observedState = tmuxObservedStateTrustPrompt
			observedKnown = true
		} else if state, ok := claude.ScreenState(text); ok {
			s.recordState("screen", title, state)
			snap := s.snapshotWithObservation("screen", title, state, true)
			snap.captureFingerprint = fingerprint
			snap.captureValid = captureValid
			return snap, nil
		}
	}
	snap := s.snapshotWithObservation(observedSource, title, observedState, observedKnown)
	snap.captureFingerprint = fingerprint
	snap.captureValid = captureValid
	return snap, nil
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
		state = newTmuxCodexSession(c.workdir, c.idleActivityTimeout)
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

func newTmuxCodexSession(workdir string, idleActivityTimeout time.Duration) *tmuxCodexSession {
	idleTitle := filepath.Base(filepath.Clean(workdir))
	if idleTitle == "." || idleTitle == string(filepath.Separator) || idleTitle == "" {
		idleTitle = "codex"
	}
	return &tmuxCodexSession{
		idleTitle:           idleTitle,
		idleTitles:          codexTmuxIdleTitles(workdir, idleTitle),
		busyTransitions:     1,
		currentState:        "busy",
		idleActivityTimeout: idleActivityTimeout,
	}
}

func (s *tmuxCodexSession) sendPrompt(ctx context.Context, prompt string) error {
	s.diag.lastPromptStartedAt = time.Now()
	snapshot, err := s.waitForSubmitReady(ctx)
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
	for attempt := 1; attempt <= tmuxSubmitMaxAttempts; attempt++ {
		s.diag.lastSubmitAttempts++
		if err := s.pane.SendEnter(ctx); err != nil {
			return fmt.Errorf("submit prompt to codex tmux tui: %w", err)
		}
		result, err := s.waitForBusyTransition(ctx, busyStart, s.diag.effectiveSubmitRetryInterval())
		if err != nil {
			return err
		}
		if result.startedProcessing {
			s.diag.lastPromptAcceptedAt = time.Now()
			return nil
		}
		if !result.lastSnapshot.acceptingInputForSubmitRetry() {
			return &tmuxSubmitStateError{
				agent:   "codex",
				attempt: attempt,
				state:   result.lastSnapshot.submitRetryState(),
				source:  result.lastSnapshot.submitRetrySource(),
			}
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
	observedSource := "screen"
	observedState := tmuxObservedStateUnknown
	observedKnown := false
	var fingerprint uint64
	captureValid := false
	if text, err := s.pane.Capture(ctx, 80); err == nil {
		fingerprint = tmuxCaptureFingerprint(text)
		captureValid = true
		trustDetected := codexTrustPrompt(text)
		if trustDetected && !s.trustPromptDetected {
			s.trustPrompts++
			s.recordTitleEvent("trust_prompt", "", "trust_prompt")
		}
		s.trustPromptDetected = trustDetected
		if trustDetected {
			snap := s.snapshotWithObservation("trust_prompt", "", tmuxObservedStateTrustPrompt, true)
			snap.captureFingerprint = fingerprint
			snap.captureValid = captureValid
			return snap, nil
		}
		if state, ok := codexScreenState(text); ok {
			if state == "idle" && isCodexBusyTitle(strings.TrimSpace(title)) {
				s.recordState("title", title, "busy")
				snap := s.snapshotWithObservation("title", title, "busy", true)
				snap.captureFingerprint = fingerprint
				snap.captureValid = captureValid
				return snap, nil
			}
			s.recordState("screen", title, state)
			snap := s.snapshotWithObservation("screen", title, state, true)
			snap.captureFingerprint = fingerprint
			snap.captureValid = captureValid
			return snap, nil
		}
	}
	if state, ok := s.classifyTitle(title); ok {
		s.recordState("title", title, state)
	}
	snap := s.snapshotWithObservation(observedSource, title, observedState, observedKnown)
	snap.captureFingerprint = fingerprint
	snap.captureValid = captureValid
	return snap, nil
}

func (s *tmuxClaudeSession) snapshotWithObservation(source, title, state string, classified bool) tmuxTitleSnapshot {
	return tmuxSnapshotWithObservation(&s.diag, s.idleTransitions, s.busyTransitions, s.trustPrompts, s.currentState, source, title, state, classified, s.recordTitleEvent)
}

func (s *tmuxCodexSession) snapshotWithObservation(source, title, state string, classified bool) tmuxTitleSnapshot {
	return tmuxSnapshotWithObservation(&s.diag, s.idleTransitions, s.busyTransitions, s.trustPrompts, s.currentState, source, title, state, classified, s.recordTitleEvent)
}

func tmuxSnapshotWithObservation(diag *tmuxSessionDiagnostics, idleTransitions, busyTransitions, trustPrompts int, currentState, source, title, state string, classified bool, recordEvent func(source, title, state string)) tmuxTitleSnapshot {
	if strings.TrimSpace(source) == "" {
		source = "transport"
	}
	if strings.TrimSpace(state) == "" {
		state = tmuxObservedStateUnknown
	}
	if !classified && recordEvent != nil {
		recordEvent(source, title, tmuxObservedStateUnknown)
	}
	snapshot := tmuxTitleSnapshot{
		idleTransitions: idleTransitions,
		busyTransitions: busyTransitions,
		trustPrompts:    trustPrompts,
		currentState:    currentState,
		observedState:   state,
		observedSource:  source,
		observedTitle:   title,
		classified:      classified,
	}
	if diag != nil {
		diag.recordObservedSnapshot(snapshot)
	}
	return snapshot
}

func (s *tmuxCodexSession) classifyTitle(title string) (string, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", false
	}
	if isCodexBusyTitle(trimmed) {
		return "busy", true
	}
	for _, idleTitle := range s.idleTitles {
		if codexTitleMatchesIdle(trimmed, idleTitle) {
			return "idle", true
		}
	}
	if codexTitleLooksIdleStatus(trimmed) {
		return "idle", true
	}
	return "busy", true
}

func codexTmuxIdleTitles(workdir, fallback string) []string {
	seen := map[string]bool{}
	var titles []string
	add := func(title string) {
		title = strings.TrimSpace(title)
		if title == "" || seen[title] {
			return
		}
		seen[title] = true
		titles = append(titles, title)
	}

	add(fallback)
	if gitRoot, ok := findTmuxGitRoot(workdir); ok {
		add(filepath.Base(gitRoot))
	}
	return titles
}

func findTmuxGitRoot(workdir string) (string, bool) {
	dir := filepath.Clean(workdir)
	if dir == "." || dir == "" {
		abs, err := filepath.Abs(dir)
		if err == nil {
			dir = abs
		}
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
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

func (s *tmuxClaudeSession) waitForBusyTransition(ctx context.Context, busyStart int, timeout time.Duration) (tmuxBusyWaitResult, error) {
	return waitForTmuxBusy(ctx, s.pane, s.snapshot, busyStart, timeout, "claude")
}

func (s *tmuxCodexSession) waitForBusyTransition(ctx context.Context, busyStart int, timeout time.Duration) (tmuxBusyWaitResult, error) {
	return waitForTmuxBusy(ctx, s.pane, s.snapshot, busyStart, timeout, "codex")
}

func (s *tmuxClaudeSession) waitForSubmitReady(ctx context.Context) (tmuxTitleSnapshot, error) {
	return waitForTmuxSubmitReady(ctx, s.pane, s.snapshot, s.diag.effectiveSubmitReadyTimeout(), "claude")
}

func (s *tmuxCodexSession) waitForSubmitReady(ctx context.Context) (tmuxTitleSnapshot, error) {
	return waitForTmuxSubmitReady(ctx, s.pane, s.snapshot, s.diag.effectiveSubmitReadyTimeout(), "codex")
}

func (s *tmuxClaudeSession) waitForIdleTransition(ctx context.Context, idleStart, busyStart int) error {
	return waitForTmuxIdle(ctx, s.pane, s.snapshot, idleStart, busyStart, s.idleActivityTimeout, "claude", s.recordIdleActivityFallback)
}

func (s *tmuxCodexSession) waitForIdleTransition(ctx context.Context, idleStart, busyStart int) error {
	return waitForTmuxIdle(ctx, s.pane, s.snapshot, idleStart, busyStart, s.idleActivityTimeout, "codex", s.recordIdleActivityFallback)
}

func (s *tmuxClaudeSession) recordIdleActivityFallback(title string) {
	s.recordState("idle_activity_fallback", title, "idle")
}

func (s *tmuxCodexSession) recordIdleActivityFallback(title string) {
	s.recordState("idle_activity_fallback", title, "idle")
}

func waitForTmuxSubmitReady(ctx context.Context, pane *tmuxagent.Pane, snapshot func(context.Context) (tmuxTitleSnapshot, error), timeout time.Duration, agent string) (tmuxTitleSnapshot, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	var last tmuxTitleSnapshot
	redraw := newTmuxPaneRedrawNudger()
	for {
		current, err := snapshot(ctx)
		if err != nil {
			return tmuxTitleSnapshot{}, err
		}
		last = current
		if current.readyForPromptSubmission() {
			return current, nil
		}
		if !time.Now().Before(deadline) {
			return tmuxTitleSnapshot{}, &tmuxSubmitReadyError{
				agent:  agent,
				state:  last.submitRetryState(),
				source: last.submitRetrySource(),
			}
		}
		if dead, err := pane.Dead(ctx); err != nil {
			return tmuxTitleSnapshot{}, err
		} else if dead {
			return tmuxTitleSnapshot{}, fmt.Errorf("%s tmux tui exited unexpectedly", agent)
		}
		if err := redraw.MaybeRequest(ctx, pane); err != nil {
			return tmuxTitleSnapshot{}, err
		}

		select {
		case <-ctx.Done():
			return tmuxTitleSnapshot{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForTmuxBusy(ctx context.Context, pane *tmuxagent.Pane, snapshot func(context.Context) (tmuxTitleSnapshot, error), busyStart int, timeout time.Duration, agent string) (tmuxBusyWaitResult, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	redraw := newTmuxPaneRedrawNudger()
	for {
		current, err := snapshot(ctx)
		if err != nil {
			return tmuxBusyWaitResult{}, err
		}
		if current.busyTransitions > busyStart {
			return tmuxBusyWaitResult{startedProcessing: true, lastSnapshot: current}, nil
		}
		if !time.Now().Before(deadline) {
			return tmuxBusyWaitResult{lastSnapshot: current}, nil
		}
		if dead, err := pane.Dead(ctx); err != nil {
			return tmuxBusyWaitResult{}, err
		} else if dead {
			return tmuxBusyWaitResult{}, fmt.Errorf("%s tmux tui exited unexpectedly", agent)
		}
		if err := redraw.MaybeRequest(ctx, pane); err != nil {
			return tmuxBusyWaitResult{}, err
		}

		select {
		case <-ctx.Done():
			return tmuxBusyWaitResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForTmuxIdle(ctx context.Context, pane *tmuxagent.Pane, snapshot func(context.Context) (tmuxTitleSnapshot, error), idleStart, busyStart int, idleActivityTimeout time.Duration, agent string, onFallback func(title string)) error {
	ticker := time.NewTicker(tmuxStatePollInterval)
	defer ticker.Stop()

	redraw := newTmuxPaneRedrawNudger()
	var lastFingerprint uint64
	var lastChange time.Time
	for {
		current, err := snapshot(ctx)
		if err != nil {
			return err
		}
		if current.busyTransitions > busyStart && current.idleTransitions > idleStart {
			return nil
		}
		now := time.Now()
		if current.captureValid {
			if current.captureFingerprint != lastFingerprint {
				lastFingerprint = current.captureFingerprint
				lastChange = now
			} else if lastChange.IsZero() {
				lastChange = now
			}
		}
		if idleActivityTimeout > 0 &&
			current.busyTransitions > busyStart &&
			!lastChange.IsZero() &&
			now.Sub(lastChange) >= idleActivityTimeout {
			if onFallback != nil {
				onFallback(current.observedTitle)
			}
			return nil
		}
		if dead, err := pane.Dead(ctx); err != nil {
			return err
		} else if dead {
			return fmt.Errorf("%s tmux tui exited unexpectedly", agent)
		}
		if err := redraw.MaybeRequest(ctx, pane); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type tmuxPaneRedrawNudger struct {
	interval time.Duration
	next     time.Time
}

func newTmuxPaneRedrawNudger() tmuxPaneRedrawNudger {
	return tmuxPaneRedrawNudger{
		interval: tmuxPaneRedrawInterval,
		next:     time.Now().Add(tmuxPaneRedrawInterval),
	}
}

func (n *tmuxPaneRedrawNudger) MaybeRequest(ctx context.Context, pane *tmuxagent.Pane) error {
	if n == nil || pane == nil || n.interval <= 0 {
		return nil
	}
	now := time.Now()
	if now.Before(n.next) {
		return nil
	}
	n.next = now.Add(n.interval)
	if err := pane.RequestRedraw(ctx); err != nil && !tmuxagent.IsTargetMissingError(err) {
		return err
	}
	return nil
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
	var stateErr *tmuxSubmitStateError
	if errors.As(err, &stateErr) {
		return tmuxFailureSubmitUnknownState
	}
	var readyErr *tmuxSubmitReadyError
	if errors.As(err, &readyErr) {
		return tmuxFailureSubmitUnknownState
	}
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

func (d *tmuxSessionDiagnostics) recordObservedSnapshot(snapshot tmuxTitleSnapshot) {
	d.lastObservedState = snapshot.submitRetryState()
	d.lastObservedSource = snapshot.submitRetrySource()
	d.lastObservedTitle = snapshot.observedTitle
	d.lastObservedKnown = snapshot.classified
}

func (d tmuxSessionDiagnostics) effectiveSubmitRetryInterval() time.Duration {
	if d.submitRetryInterval > 0 {
		return d.submitRetryInterval
	}
	return tmuxSubmitRetryInterval
}

func (d tmuxSessionDiagnostics) effectiveSubmitReadyTimeout() time.Duration {
	if d.startupTimeout > 0 {
		return d.startupTimeout
	}
	return 30 * time.Second
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
			"last_observed_state": map[string]any{
				"state":      d.lastObservedState,
				"source":     d.lastObservedSource,
				"title":      d.lastObservedTitle,
				"classified": d.lastObservedKnown,
			},
		},
	}
}

func (d tmuxSessionDiagnostics) timingMetadata() map[string]any {
	timing := map[string]any{
		"submit_retry_interval_ms": d.effectiveSubmitRetryInterval().Milliseconds(),
		"submit_ready_timeout_ms":  d.effectiveSubmitReadyTimeout().Milliseconds(),
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
