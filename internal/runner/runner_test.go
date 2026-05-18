package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vibedrive/internal/automation"
	"vibedrive/internal/config"
	"vibedrive/internal/diagnostics"
	"vibedrive/internal/plan"
	"vibedrive/internal/runstate"
	"vibedrive/internal/tasknotes"
	"vibedrive/internal/tmuxagent"
	"vibedrive/pkg/agentcli/claude"
	"vibedrive/pkg/agentcli/codex"
)

type fakeAgent struct {
	prompts         []string
	sessionIDs      []string
	closedSessionID []string
	planPath        string
	closeEvents     *[]string
	closeLabel      string
	fullscreen      bool
	onRun           func(string) error
}

func (f *fakeAgent) RunPrompt(_ context.Context, session *claude.Session, prompt string) error {
	f.prompts = append(f.prompts, prompt)
	f.sessionIDs = append(f.sessionIDs, session.ID)

	if f.onRun != nil {
		if err := f.onRun(prompt); err != nil {
			return err
		}
	}
	return handleFakePrompt(prompt, f.planPath)
}

func (f *fakeAgent) Close(session *claude.Session) error {
	f.closedSessionID = append(f.closedSessionID, session.ID)
	if f.closeEvents != nil {
		label := f.closeLabel
		if label == "" {
			label = "claude"
		}
		*f.closeEvents = append(*f.closeEvents, label)
	}
	return nil
}

func (f *fakeAgent) IsFullscreenTUI() bool {
	return f.fullscreen
}

type fakeCodex struct {
	prompts         []string
	closedSessionID []string
	planPath        string
	closeEvents     *[]string
	closeLabel      string
	fullscreen      bool
	onRun           func(string) error
}

func (f *fakeCodex) RunPrompt(_ context.Context, session *codex.Session, prompt string) error {
	f.prompts = append(f.prompts, prompt)

	if f.onRun != nil {
		if err := f.onRun(prompt); err != nil {
			return err
		}
	}
	return handleFakePrompt(prompt, f.planPath)
}

func (f *fakeCodex) Close(_ *codex.Session) error {
	f.closedSessionID = append(f.closedSessionID, "closed")
	if f.closeEvents != nil {
		label := f.closeLabel
		if label == "" {
			label = "codex"
		}
		*f.closeEvents = append(*f.closeEvents, label)
	}
	return nil
}

func (f *fakeCodex) IsFullscreenTUI() bool {
	return f.fullscreen
}

type failingDisplayWriter struct {
	writes int
}

func (w *failingDisplayWriter) Write(_ []byte) (int, error) {
	w.writes++
	return 0, os.ErrClosed
}

func TestNewRejectsPinnedAgentVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	codexCommand := writeRunnerVersionCommand(t, dir, "fake-codex", "codex-cli 4.5.7")

	cfg := &config.Config{
		Workspace: dir,
		Codex: config.CodexConfig{
			Command: codexCommand,
			Version: "codex-cli 4.5.6",
		},
	}

	_, err := New(cfg, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected New to reject mismatched codex.version")
	}
	if !strings.Contains(err.Error(), `codex.version "codex-cli 4.5.6" does not match live codex CLI version "codex-cli 4.5.7"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunExecutesReadyPlanTasksByDependencyOrder(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
  - id: inspect
    title: Implement inspect
    workflow: implement
    status: todo
    deps:
      - scaffold
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "analyze", Type: config.StepTypeClaude, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeClaude, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	agent := &fakeAgent{planPath: planPath}
	sessionCount := 0
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: agent,
		newSession: func(_ string) (*claude.Session, error) {
			sessionCount++
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-" + string(rune('0'+sessionCount)),
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if sessionCount != 2 {
		t.Fatalf("expected 2 sessions, got %d", sessionCount)
	}

	loaded, err := plan.Load(planPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	for _, task := range loaded.Tasks {
		if task.Status != plan.StatusDone {
			t.Fatalf("expected task %q to be done, got %q", task.ID, task.Status)
		}
	}

	wantPrompts := []string{
		"analyze scaffold",
		"finish task scaffold",
		"analyze inspect",
		"finish task inspect",
	}
	if strings.Join(agent.prompts, "\n") != strings.Join(wantPrompts, "\n") {
		t.Fatalf("unexpected prompts:\n%s", strings.Join(agent.prompts, "\n"))
	}
}

func TestRunLeavesSessionIDEmptyForNonTUIClaude(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "render-session", Type: config.StepTypeClaude, Prompt: "session={{ .SessionID }} task={{ .Task.ID }}"},
				},
			},
		},
	}

	agent := &fakeAgent{
		planPath: planPath,
		onRun: func(_ string) error {
			return updateTask(planPath, "scaffold", plan.StatusDone, "done")
		},
	}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: agent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{Strategy: config.SessionStrategySessionID, ID: "session-1"}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(agent.prompts, "\n"); got != "session= task=scaffold" {
		t.Fatalf("expected non-TUI Claude prompt to render an empty SessionID, got %q", got)
	}
}

func TestRunRecordsActiveRuntimeStepState(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "analyze", Type: config.StepTypeClaude, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeClaude, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	var seen []runstate.Task
	agent := &fakeAgent{
		planPath: planPath,
		onRun: func(prompt string) error {
			state, err := runstate.Load(runstate.Path(dir))
			if err != nil {
				return err
			}
			active, ok := runstate.ActiveTasksForPlan(state, planPath)["scaffold"]
			if !ok {
				return os.ErrNotExist
			}
			seen = append(seen, active)
			return nil
		},
	}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: agent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected to observe two active steps, got %#v", seen)
	}
	if seen[0].Status != plan.StatusInProgress || seen[0].StepName != "analyze" || seen[0].StepIndex != 0 || seen[0].StepTotal != 2 {
		t.Fatalf("unexpected first active step: %#v", seen[0])
	}
	if seen[1].Status != plan.StatusInProgress || seen[1].StepName != "finish" || seen[1].StepIndex != 1 || seen[1].StepTotal != 2 {
		t.Fatalf("unexpected second active step: %#v", seen[1])
	}
	if _, err := os.Stat(runstate.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("expected run state to be cleared after Run, stat err=%v", err)
	}
}

func TestIsolatedRunnerRecordsRuntimeStateAgainstRootPlan(t *testing.T) {
	dir := t.TempDir()
	rootPlanPath := filepath.Join(dir, "vibedrive-plan.yaml")
	isolatedDir := filepath.Join(dir, ".vibedrive", "worktrees", "001-api")
	isolatedPlanPath := filepath.Join(isolatedDir, "vibedrive-plan.yaml")

	cfg := &config.Config{
		Workspace:       dir,
		PlanFile:        rootPlanPath,
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeExec, Command: []string{"true"}},
				},
			},
		},
	}
	r := &Runner{cfg: cfg, stdout: io.Discard, stderr: io.Discard}
	r.ensureRunState()

	worker, err := r.isolatedRunner(TaskExecutionPaths{
		TaskID:    "api",
		Name:      "001-api",
		Workspace: isolatedDir,
		PlanFile:  isolatedPlanPath,
		Isolated:  true,
	}, cfg.Workflows["implement"].Steps)
	if err != nil {
		t.Fatalf("isolatedRunner returned error: %v", err)
	}
	worker.recordRuntimeStep(TemplateData{
		WorkflowName: "implement",
		Workspace:    isolatedDir,
		PlanFile:     isolatedPlanPath,
		Task:         plan.Task{ID: "api"},
	}, cfg.Workflows["implement"].Steps[0], 0, 1)

	state, err := runstate.Load(runstate.Path(dir))
	if err != nil {
		t.Fatalf("Load run state returned error: %v", err)
	}
	if _, ok := runstate.ActiveTasksForPlan(state, rootPlanPath)["api"]; !ok {
		t.Fatalf("expected active task to be associated with root plan, state=%#v", state)
	}
	if _, ok := runstate.ActiveTasksForPlan(state, isolatedPlanPath)["api"]; ok {
		t.Fatalf("did not expect isolated plan path to own root-visible runtime state")
	}
}

func TestRunExplainsStalledPlanProgress(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "analyze", Type: config.StepTypeClaude, Prompt: "analyze {{ .Task.ID }}"},
				},
			},
		},
	}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: &fakeAgent{planPath: planPath},
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to fail when the plan task does not change")
	}

	message := err.Error()
	if !strings.Contains(message, "made no task progress") {
		t.Fatalf("expected plan stall explanation, got %q", message)
	}
	if !strings.Contains(message, "status") {
		t.Fatalf("expected plan stall error to mention status, got %q", message)
	}
}

func TestRunDispatchesCodexPlanStepsWithoutChangingWorkflowNames(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeCodex, Prompt: "finish task {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeClaude, Prompt: "review {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(codexAgent.prompts, "\n"); got != "finish task scaffold" {
		t.Fatalf("unexpected codex prompts:\n%s", got)
	}
	if got := strings.Join(claudeAgent.prompts, "\n"); got != "review scaffold" {
		t.Fatalf("unexpected claude prompts:\n%s", got)
	}
}

func TestRunStepLogsCodexPromptPreview(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer

	r := &Runner{
		cfg: &config.Config{
			Workspace: dir,
		},
		stdout: &stdout,
		stderr: io.Discard,
		codex:  &fakeCodex{},
	}

	err := r.runStep(context.Background(), nil, nil, config.Step{
		Name:   "review",
		Type:   config.StepTypeCodex,
		Prompt: "review {{ .Task.ID }}\ncheck acceptance criteria",
	}, TemplateData{
		Task: plan.Task{ID: "scaffold"},
	})
	if err != nil {
		t.Fatalf("runStep returned error: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, "\n--> codex step: review\n") {
		t.Fatalf("expected codex step header, got %q", got)
	}
	if !strings.Contains(got, "    review scaffold\n") {
		t.Fatalf("expected first prompt line in preview, got %q", got)
	}
	if !strings.Contains(got, "    check acceptance criteria\n") {
		t.Fatalf("expected second prompt line in preview, got %q", got)
	}
}

func TestRunStepExecErrorIncludesCommandOutput(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer

	r := &Runner{
		cfg: &config.Config{
			Workspace: dir,
		},
		stdout: io.Discard,
		stderr: &stderr,
	}

	err := r.runStep(context.Background(), nil, nil, config.Step{
		Name:    "fail",
		Type:    config.StepTypeExec,
		Command: []string{"sh", "-c", "printf 'verify failed\\n' >&2; exit 7"},
	}, TemplateData{
		Task: plan.Task{ID: "scaffold"},
	})
	if err == nil {
		t.Fatal("expected runStep to fail")
	}
	if !strings.Contains(err.Error(), "command output:") || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("expected error to include command output, got %v", err)
	}
	if !strings.Contains(stderr.String(), "verify failed") {
		t.Fatalf("expected stderr to receive command output, got %q", stderr.String())
	}
}

func TestRunStepExecIgnoresLiveOutputWriteErrors(t *testing.T) {
	dir := t.TempDir()
	stdout := &failingDisplayWriter{}
	stderr := &failingDisplayWriter{}

	r := &Runner{
		cfg: &config.Config{
			Workspace: dir,
		},
		stdout: stdout,
		stderr: stderr,
	}

	err := r.runStep(context.Background(), nil, nil, config.Step{
		Name:    "execute",
		Type:    config.StepTypeExec,
		Command: []string{"sh", "-c", "printf 'stdout-ok\\n'; printf 'stderr-ok\\n' >&2"},
	}, TemplateData{
		Task: plan.Task{ID: "scaffold"},
	})
	if err != nil {
		t.Fatalf("runStep returned error: %v", err)
	}
	if stdout.writes == 0 {
		t.Fatal("expected stdout display writer to receive output")
	}
	if stderr.writes == 0 {
		t.Fatal("expected stderr display writer to receive output")
	}
}

func TestRunStepExecFailureCapturesBoundedDiagnostics(t *testing.T) {
	dir := t.TempDir()
	loops := diagnostics.ExecOutputLimit/1024 + 8
	chunk := strings.Repeat("x", 1024)
	script := fmt.Sprintf(`chunk='%s'
printf 'STDOUT-BEGIN\n'
i=0
while [ "$i" -lt %d ]; do
  printf '%%s' "$chunk"
  i=$((i + 1))
done
printf 'STDOUT-TAIL\n'
printf 'STDERR-BEGIN\n' >&2
i=0
while [ "$i" -lt %d ]; do
  printf '%%s' "$chunk" >&2
  i=$((i + 1))
done
printf 'STDERR-TAIL\n' >&2
exit 7
`, chunk, loops, loops)

	r := &Runner{
		cfg: &config.Config{
			Workspace: dir,
		},
		stdout: io.Discard,
		stderr: io.Discard,
	}

	err := r.runStep(context.Background(), nil, nil, config.Step{
		Name:    "execute",
		Type:    config.StepTypeExec,
		Command: []string{"sh", "-c", script},
		Env: map[string]string{
			"API_TOKEN": "secret",
			"MODE":      "failure",
		},
	}, TemplateData{
		Task: plan.Task{ID: "big-output"},
	})
	if err == nil {
		t.Fatal("expected runStep to fail")
	}
	if !strings.Contains(err.Error(), "run command: exit status 7\ncommand output:\n") {
		t.Fatalf("expected existing command error format, got %v", err)
	}
	if (!strings.Contains(err.Error(), "STDOUT-TAIL") && !strings.Contains(err.Error(), "STDERR-TAIL")) ||
		strings.Contains(err.Error(), "STDOUT-BEGIN") ||
		strings.Contains(err.Error(), "STDERR-BEGIN") {
		t.Fatalf("expected visible command output to use the bounded tail, got %v", err)
	}

	stepDir, err := diagnostics.New(dir).StepDir(diagnostics.Identity{
		RunID:    r.runStateID,
		TaskID:   "big-output",
		StepName: "execute",
	})
	if err != nil {
		t.Fatalf("StepDir returned error: %v", err)
	}

	stdoutTail := mustReadRunnerBytes(t, filepath.Join(stepDir, "exec", "stdout-tail.txt"))
	if len(stdoutTail) != diagnostics.ExecOutputLimit {
		t.Fatalf("stdout tail length = %d, want %d", len(stdoutTail), diagnostics.ExecOutputLimit)
	}
	if bytes.Contains(stdoutTail, []byte("STDOUT-BEGIN")) || !bytes.HasSuffix(stdoutTail, []byte("STDOUT-TAIL\n")) {
		t.Fatalf("stdout diagnostics did not preserve only the tail")
	}

	stderrTail := mustReadRunnerBytes(t, filepath.Join(stepDir, "exec", "stderr-tail.txt"))
	if len(stderrTail) != diagnostics.ExecOutputLimit {
		t.Fatalf("stderr tail length = %d, want %d", len(stderrTail), diagnostics.ExecOutputLimit)
	}
	if bytes.Contains(stderrTail, []byte("STDERR-BEGIN")) || !bytes.HasSuffix(stderrTail, []byte("STDERR-TAIL\n")) {
		t.Fatalf("stderr diagnostics did not preserve only the tail")
	}

	combinedTail := mustReadRunnerBytes(t, filepath.Join(stepDir, "exec", "combined-tail.txt"))
	if len(combinedTail) != diagnostics.ExecOutputLimit {
		t.Fatalf("combined tail length = %d, want %d", len(combinedTail), diagnostics.ExecOutputLimit)
	}
	if bytes.Contains(combinedTail, []byte("STDOUT-BEGIN")) ||
		bytes.Contains(combinedTail, []byte("STDERR-BEGIN")) ||
		(!bytes.Contains(combinedTail, []byte("STDOUT-TAIL\n")) && !bytes.Contains(combinedTail, []byte("STDERR-TAIL\n"))) {
		t.Fatalf("combined diagnostics did not preserve the observed output tail")
	}

	var command diagnostics.ExecCommand
	readRunnerJSONFile(t, filepath.Join(stepDir, "exec", "command.json"), &command)
	if strings.Join(command.Argv, "\x00") != strings.Join([]string{"sh", "-c", script}, "\x00") {
		t.Fatalf("command argv was not captured: %#v", command.Argv)
	}
	if command.WorkingDir != dir {
		t.Fatalf("working_dir = %q, want %q", command.WorkingDir, dir)
	}
	if command.ExitCode == nil || *command.ExitCode != 7 {
		t.Fatalf("exit_code = %#v, want 7", command.ExitCode)
	}
	if command.Env.Step["MODE"] != "failure" {
		t.Fatalf("MODE env was not captured: %#v", command.Env.Step)
	}
	if command.Env.Step["API_TOKEN"] != "[REDACTED]" {
		t.Fatalf("API_TOKEN env was not redacted: %#v", command.Env.Step)
	}
	if len(command.Env.InheritedKeys) == 0 {
		t.Fatal("expected inherited environment keys in command metadata")
	}

	var manifest diagnostics.Manifest
	readRunnerJSONFile(t, filepath.Join(stepDir, "manifest.json"), &manifest)
	if manifest.Failure.Path != "exec_step_non_zero_exit" || manifest.Transport.Kind != "exec" {
		t.Fatalf("unexpected diagnostics manifest: %#v", manifest)
	}
	entry := runnerArtifactEntry(t, manifest, diagnostics.ArtifactExecStdout)
	if !entry.Truncated || entry.LimitBytes != diagnostics.ExecOutputLimit || entry.OriginalBytes <= diagnostics.ExecOutputLimit || entry.SHA256 == "" {
		t.Fatalf("stdout manifest entry missing bounded tail metadata: %#v", entry)
	}
}

func TestRunStepCodexExecTransportRunsRenderedPrompt(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex.py")
	capturePath := filepath.Join(dir, "capture.json")
	script := `#!/usr/bin/env python3
import json
import os
import sys

prompt = sys.stdin.read()
with open(os.environ["CODEX_CAPTURE"], "w") as capture:
    json.dump({"args": sys.argv[1:], "prompt": prompt}, capture)
sys.stdout.write("codex exec complete\n")
sys.stdout.flush()
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	t.Setenv("CODEX_CAPTURE", capturePath)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	codexAgent, err := codex.New(scriptPath, []string{"--model", "test"}, dir, codex.TransportExec, "1s", &stdout, &stderr)
	if err != nil {
		t.Fatalf("codex.New returned error: %v", err)
	}
	r := &Runner{
		cfg: &config.Config{
			Workspace: dir,
		},
		stdout: &stdout,
		stderr: &stderr,
		codex:  codexAgent,
	}

	err = r.runStep(context.Background(), nil, &codex.Session{}, config.Step{
		Name:   "implement",
		Type:   config.StepTypeCodex,
		Prompt: "implement {{ .Task.ID }} in {{ .Workspace }}",
	}, TemplateData{
		Task:      plan.Task{ID: "scaffold"},
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("runStep returned error: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	var capture struct {
		Args   []string `json:"args"`
		Prompt string   `json:"prompt"`
	}
	readRunnerJSONFile(t, capturePath, &capture)
	if strings.Join(capture.Args, "\x00") != strings.Join([]string{"exec", "--model", "test"}, "\x00") {
		t.Fatalf("unexpected codex argv: %#v", capture.Args)
	}
	wantPrompt := "implement scaffold in " + dir
	if capture.Prompt != wantPrompt {
		t.Fatalf("prompt = %q, want %q", capture.Prompt, wantPrompt)
	}
	if strings.Contains(stdout.String(), "\x1b]0;") {
		t.Fatalf("expected non-TUI exec output, got terminal title sequence in %q", stdout.String())
	}
}

func TestParallelBatchNeedsTmuxOnlyForFullscreenAgents(t *testing.T) {
	cfg := &config.Config{
		Workspace:       t.TempDir(),
		DefaultWorkflow: "implement",
		Coder:           config.AgentCodex,
		Reviewer:        config.AgentClaude,
		Codex: config.CodexConfig{
			Transport: config.CodexTransportExec,
		},
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "implement", Type: config.StepTypeCodex, Prompt: "implement"},
				},
			},
		},
	}
	task := plan.Task{ID: "scaffold", Workflow: "implement"}

	r := &Runner{cfg: cfg, codex: &fakeCodex{}}
	needsTmux, err := r.parallelBatchNeedsTmux([]plan.Task{task})
	if err != nil {
		t.Fatalf("parallelBatchNeedsTmux returned error: %v", err)
	}
	if needsTmux {
		t.Fatal("expected codex exec transport not to require parallel tmux")
	}

	r.codex = &fakeCodex{fullscreen: true}
	needsTmux, err = r.parallelBatchNeedsTmux([]plan.Task{task})
	if err != nil {
		t.Fatalf("parallelBatchNeedsTmux returned error: %v", err)
	}
	if !needsTmux {
		t.Fatal("expected fullscreen codex transport to require parallel tmux")
	}
}

func TestRunPreparesPlanArtifactDirectories(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "write {{ .ReviewPath }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	reviewPath := automation.ReviewPath(dir, "scaffold")
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatalf("expected review artifact %s to exist, stat err=%v", reviewPath, err)
	}
}

func TestTaskExecutionPathsUseMainWorkspaceByDefault(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	paths := taskExecutionPaths(&config.Config{
		Workspace: dir,
		PlanFile:  planPath,
	}, "scaffold", 1, false)

	if paths.Isolated {
		t.Fatal("expected default execution paths not to be isolated")
	}
	if paths.Workspace != dir {
		t.Fatalf("expected workspace %q, got %q", dir, paths.Workspace)
	}
	if paths.PlanFile != planPath {
		t.Fatalf("expected plan file %q, got %q", planPath, paths.PlanFile)
	}
	if paths.TaskResultPath != automation.ResultPath(dir, "scaffold") {
		t.Fatalf("expected result path %q, got %q", automation.ResultPath(dir, "scaffold"), paths.TaskResultPath)
	}
	if paths.ReviewPath != automation.ReviewPath(dir, "scaffold") {
		t.Fatalf("expected review path %q, got %q", automation.ReviewPath(dir, "scaffold"), paths.ReviewPath)
	}
	if paths.TaskNotesPath != tasknotes.Path(dir) {
		t.Fatalf("expected task notes path %q, got %q", tasknotes.Path(dir), paths.TaskNotesPath)
	}
}

func TestTaskExecutionPathsBuildDeterministicIsolatedPaths(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plans", "vibedrive-plan.yaml")
	cfg := &config.Config{
		Workspace: dir,
		PlanFile:  planPath,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 4,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
	}

	paths := taskExecutionPaths(cfg, "api/db", 7, true)
	again := taskExecutionPaths(cfg, "api/db", 7, true)
	if paths.Name != again.Name {
		t.Fatalf("expected deterministic workspace name, got %q then %q", paths.Name, again.Name)
	}
	if !regexp.MustCompile(`^007-api-db-[a-f0-9]{12}$`).MatchString(paths.Name) {
		t.Fatalf("unexpected workspace name %q", paths.Name)
	}

	sameSlug := taskExecutionPaths(cfg, "api:db", 7, true)
	if sameSlug.Name == paths.Name {
		t.Fatalf("expected hash suffix to distinguish task IDs with the same slug, got %q", paths.Name)
	}

	wantWorkspace := filepath.Join(cfg.Parallel.WorktreeRoot, paths.Name)
	if paths.Workspace != wantWorkspace {
		t.Fatalf("expected workspace %q, got %q", wantWorkspace, paths.Workspace)
	}
	if paths.GitWorktree != wantWorkspace {
		t.Fatalf("expected git worktree %q, got %q", wantWorkspace, paths.GitWorktree)
	}
	wantPlanFile := filepath.Join(paths.Workspace, "plans", "vibedrive-plan.yaml")
	if paths.PlanFile != wantPlanFile {
		t.Fatalf("expected isolated plan file %q, got %q", wantPlanFile, paths.PlanFile)
	}
	wantArtifactRoot := filepath.Join(cfg.Parallel.ArtifactRoot, paths.Name)
	if paths.ArtifactRoot != wantArtifactRoot {
		t.Fatalf("expected artifact root %q, got %q", wantArtifactRoot, paths.ArtifactRoot)
	}
	if paths.ArtifactBaseRoot != cfg.Parallel.ArtifactRoot {
		t.Fatalf("expected artifact base root %q, got %q", cfg.Parallel.ArtifactRoot, paths.ArtifactBaseRoot)
	}
	wantResultPath := filepath.Join(wantArtifactRoot, "task-results", "api_db.json")
	if paths.TaskResultPath != wantResultPath {
		t.Fatalf("expected result path %q, got %q", wantResultPath, paths.TaskResultPath)
	}
	wantReviewPath := filepath.Join(wantArtifactRoot, "reviews", "api_db.json")
	if paths.ReviewPath != wantReviewPath {
		t.Fatalf("expected review path %q, got %q", wantReviewPath, paths.ReviewPath)
	}
	wantNotesPath := filepath.Join(wantArtifactRoot, "task-notes.yaml")
	if paths.TaskNotesPath != wantNotesPath {
		t.Fatalf("expected task notes path %q, got %q", wantNotesPath, paths.TaskNotesPath)
	}
}

func TestTaskExecutionPathsPreserveNestedGitWorkspaceLayout(t *testing.T) {
	repo := t.TempDir()
	workspace := filepath.Join(repo, "TetherGame")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	initRunnerGitRepo(t, repo)

	planPath := filepath.Join(workspace, "vibedrive-plan.yaml")
	cfg := &config.Config{
		Workspace: workspace,
		PlanFile:  planPath,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 4,
			WorktreeRoot:   filepath.Join(workspace, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(workspace, config.DefaultParallelArtifactRoot),
		},
	}

	paths := taskExecutionPaths(cfg, "api", 1, true)
	wantGitWorktree := filepath.Join(cfg.Parallel.WorktreeRoot, paths.Name)
	if paths.GitWorktree != wantGitWorktree {
		t.Fatalf("expected git worktree %q, got %q", wantGitWorktree, paths.GitWorktree)
	}
	wantWorkspace := filepath.Join(wantGitWorktree, "TetherGame")
	if paths.Workspace != wantWorkspace {
		t.Fatalf("expected nested workspace %q, got %q", wantWorkspace, paths.Workspace)
	}
	wantPlanFile := filepath.Join(wantWorkspace, "vibedrive-plan.yaml")
	if paths.PlanFile != wantPlanFile {
		t.Fatalf("expected nested plan file %q, got %q", wantPlanFile, paths.PlanFile)
	}
}

func TestPrepareParallelTaskWorkspacePreservesNestedPlanRelativeContracts(t *testing.T) {
	repo := t.TempDir()
	workspace := filepath.Join(repo, "TetherGame")
	if err := os.MkdirAll(filepath.Join(repo, "docs", "api"), 0o755); err != nil {
		t.Fatalf("MkdirAll docs returned error: %v", err)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll workspace returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "api", "build-foundation.md"), []byte("contract\n"), 0o644); err != nil {
		t.Fatalf("WriteFile contract returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "vibedrive-plan.yaml"), []byte("project:\n  name: demo\ntasks:\n  - id: first\n    title: First\n"), 0o644); err != nil {
		t.Fatalf("WriteFile plan returned error: %v", err)
	}
	initRunnerGitRepo(t, repo)

	cfg := &config.Config{
		Workspace: workspace,
		PlanFile:  filepath.Join(workspace, "vibedrive-plan.yaml"),
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 4,
			WorktreeRoot:   filepath.Join(workspace, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(workspace, config.DefaultParallelArtifactRoot),
		},
	}
	currentPlan := &plan.File{
		Path: cfg.PlanFile,
		Project: plan.Project{
			Name: "demo",
			Components: []plan.Component{
				{
					ID:             "contracts",
					ReadsContracts: plan.StringList{"../docs/api/build-foundation.md"},
				},
			},
		},
		Tasks: []plan.Task{
			{
				ID:             "first",
				Title:          "First",
				Status:         plan.StatusTodo,
				Component:      "contracts",
				ReadsContracts: plan.StringList{"../docs/api/build-foundation.md"},
			},
		},
	}
	paths := taskExecutionPaths(cfg, "first", 1, true)
	r := &Runner{cfg: cfg, stdout: io.Discard, stderr: io.Discard}

	if err := r.prepareParallelTaskWorkspace(context.Background(), currentPlan, paths); err != nil {
		t.Fatalf("prepareParallelTaskWorkspace returned error: %v", err)
	}
	if _, err := plan.Load(paths.PlanFile); err != nil {
		t.Fatalf("Load isolated plan returned error: %v", err)
	}
	if !strings.HasSuffix(paths.PlanFile, filepath.Join(paths.Name, "TetherGame", "vibedrive-plan.yaml")) {
		t.Fatalf("expected isolated plan under nested workspace, got %q", paths.PlanFile)
	}
}

func TestCleanupTaskExecutionPathsRemovesOnlyOwnedChildren(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := filepath.Join(dir, "worktrees")
	artifactRoot := filepath.Join(dir, "task-runs")
	paths := TaskExecutionPaths{
		Isolated:         true,
		WorktreeRoot:     workspaceRoot,
		Workspace:        filepath.Join(workspaceRoot, "001-task-abcdef123456"),
		ArtifactBaseRoot: artifactRoot,
		ArtifactRoot:     filepath.Join(artifactRoot, "001-task-abcdef123456"),
	}
	if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.MkdirAll(paths.ArtifactRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	if err := cleanupTaskExecutionPaths(paths); err != nil {
		t.Fatalf("cleanupTaskExecutionPaths returned error: %v", err)
	}
	if _, err := os.Stat(paths.Workspace); !os.IsNotExist(err) {
		t.Fatalf("expected workspace to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(paths.ArtifactRoot); !os.IsNotExist(err) {
		t.Fatalf("expected artifact root to be removed, stat err=%v", err)
	}

	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	err := cleanupTaskExecutionPaths(TaskExecutionPaths{
		Isolated:         true,
		WorktreeRoot:     workspaceRoot,
		Workspace:        outside,
		ArtifactBaseRoot: artifactRoot,
		ArtifactRoot:     filepath.Join(artifactRoot, "002-task-abcdef123456"),
	})
	if err == nil {
		t.Fatal("expected cleanup to reject workspace outside owned root")
	}
	if !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("unexpected cleanup error: %v", err)
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("expected outside directory to remain, stat err=%v", statErr)
	}

	outsideArtifact := filepath.Join(dir, "outside-artifact")
	ownedWorkspace := filepath.Join(workspaceRoot, "003-task-abcdef123456")
	if err := os.MkdirAll(ownedWorkspace, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.MkdirAll(outsideArtifact, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	err = cleanupTaskExecutionPaths(TaskExecutionPaths{
		Isolated:         true,
		WorktreeRoot:     workspaceRoot,
		Workspace:        ownedWorkspace,
		ArtifactBaseRoot: artifactRoot,
		ArtifactRoot:     outsideArtifact,
	})
	if err == nil {
		t.Fatal("expected cleanup to reject artifact path outside owned root")
	}
	if _, statErr := os.Stat(outsideArtifact); statErr != nil {
		t.Fatalf("expected outside artifact directory to remain, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(ownedWorkspace); statErr != nil {
		t.Fatalf("expected owned workspace to remain when artifact path is unsafe, stat err=%v", statErr)
	}
}

func TestWorkerIntegrationExcludesTargetArtifacts(t *testing.T) {
	excludes := workerIntegrationExcludes(TaskExecutionPaths{})
	for _, exclude := range excludes {
		if exclude == ":(exclude)target/**" {
			return
		}
	}
	t.Fatalf("expected worker integration excludes to include target artifacts, got %#v", excludes)
}

func TestParallelWorkerPatchCollectsNewFilesWithIgnoredArtifactsPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(strings.Join([]string{
		".vibedrive/task-results/",
		".vibedrive/reviews/",
		".vibedrive/task-runs/",
		".vibedrive/worktrees/",
		"target/",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile .gitignore returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)
	base := strings.TrimSpace(runRunnerCmd(t, dir, "git", "-C", dir, "rev-parse", "HEAD"))

	worker := filepath.Join(t.TempDir(), "worker")
	runRunnerCmd(t, dir, "git", "-C", dir, "worktree", "add", "--detach", worker, "HEAD")
	for path, content := range map[string]string{
		filepath.Join(worker, "src", "new.txt"):                                 "worker source\n",
		filepath.Join(worker, "target", "cache.txt"):                            "ignored artifact\n",
		filepath.Join(worker, ".vibedrive", "task-runs", "scaffold", "log.txt"): "ignored run log\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll returned error: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s returned error: %v", path, err)
		}
	}

	r := &Runner{}
	patch, err := r.parallelWorkerPatch(context.Background(), parallelTaskResult{
		Task:         plan.Task{ID: "scaffold"},
		Paths:        TaskExecutionPaths{Workspace: worker},
		BaseRevision: base,
	})
	if err != nil {
		t.Fatalf("parallelWorkerPatch returned error: %v", err)
	}
	if !bytes.Contains(patch, []byte("src/new.txt")) {
		t.Fatalf("expected patch to include worker source file, got:\n%s", patch)
	}
	for _, unwanted := range [][]byte{[]byte("target/cache.txt"), []byte(".vibedrive/task-runs/scaffold/log.txt")} {
		if bytes.Contains(patch, unwanted) {
			t.Fatalf("expected patch to exclude ignored artifact %s, got:\n%s", unwanted, patch)
		}
	}
}

func TestIntegrateParallelResultFallsBackForOversizedPatch(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	content := `project:
  name: demo
tasks:
  - id: big
    title: Big Patch
    workflow: implement
    status: todo
    owns_paths:
      - big.txt
    verify_commands:
      - grep -q worker big.txt
    commit_message: "feat: finish big patch"
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile big returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)
	base := strings.TrimSpace(runRunnerCmd(t, dir, "git", "-C", dir, "rev-parse", "HEAD"))

	worker := filepath.Join(t.TempDir(), "worker")
	runRunnerCmd(t, dir, "git", "-C", dir, "worktree", "add", "--detach", worker, "HEAD")
	if err := os.WriteFile(filepath.Join(worker, "big.txt"), []byte("worker\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worker big returned error: %v", err)
	}

	oldApply := gitApplyPatchFunc
	gitApplyPatchFunc = func(context.Context, string, []byte, bool, bool) error {
		return fmt.Errorf("git apply: exit status 128: error: patch too large")
	}
	defer func() {
		gitApplyPatchFunc = oldApply
	}()

	cfg := &config.Config{
		Workspace: dir,
		PlanFile:  planPath,
		Parallel: config.ParallelConfig{
			ArtifactRoot: filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
	}
	r := &Runner{cfg: cfg, stdout: io.Discard, stderr: io.Discard}
	err := r.integrateParallelResult(context.Background(), parallelTaskResult{
		Task:         plan.Task{ID: "big", CommitMessage: "feat: finish big patch"},
		Paths:        TaskExecutionPaths{Workspace: worker},
		BaseRevision: base,
		Status:       plan.StatusDone,
		Notes:        "worker complete",
	})
	if err != nil {
		t.Fatalf("integrateParallelResult returned error: %v", err)
	}

	if got := mustReadRunnerFile(t, filepath.Join(dir, "big.txt")); got != "worker\n" {
		t.Fatalf("expected file-sync fallback to copy worker content, got %q", got)
	}
	loaded, loadErr := plan.Load(planPath)
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	task, _ := loaded.FindTask("big")
	if task.Status != plan.StatusDone {
		t.Fatalf("expected big task to be done, got %q", task.Status)
	}
	notesFile, notesErr := tasknotes.Load(tasknotes.Path(dir))
	if notesErr != nil {
		t.Fatalf("Load task notes returned error: %v", notesErr)
	}
	note, ok := notesFile.Find("big")
	if !ok {
		t.Fatal("expected big task note")
	}
	if !strings.Contains(note.Notes, "safe file-sync fallback") {
		t.Fatalf("expected note to mention file-sync fallback, got %q", note.Notes)
	}
}

func TestApplyParallelWorkerFilesRefusesChangedRootPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile shared returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)
	base := strings.TrimSpace(runRunnerCmd(t, dir, "git", "-C", dir, "rev-parse", "HEAD"))

	worker := filepath.Join(t.TempDir(), "worker")
	runRunnerCmd(t, dir, "git", "-C", dir, "worktree", "add", "--detach", worker, "HEAD")
	if err := os.WriteFile(filepath.Join(worker, "shared.txt"), []byte("worker\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worker shared returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root shared returned error: %v", err)
	}

	r := &Runner{cfg: &config.Config{Workspace: dir}}
	_, err := r.applyParallelWorkerPatchFallback(context.Background(), parallelTaskResult{
		Task:         plan.Task{ID: "shared"},
		Paths:        TaskExecutionPaths{Workspace: worker},
		BaseRevision: base,
	}, fmt.Errorf("git apply: exit status 128: error: patch too large"))
	if err == nil {
		t.Fatal("expected changed root path to block file-sync fallback")
	}
	if !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("expected changed-since error, got %v", err)
	}
	if got := mustReadRunnerFile(t, filepath.Join(dir, "shared.txt")); got != "root\n" {
		t.Fatalf("expected root file to remain unchanged, got %q", got)
	}
}

func TestRollbackParallelWorkerFileSyncRestoresBaseState(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"changed.txt": "base\n",
		"delete.txt":  "delete\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s returned error: %v", name, err)
		}
	}
	initRunnerGitRepo(t, dir)
	base := strings.TrimSpace(runRunnerCmd(t, dir, "git", "-C", dir, "rev-parse", "HEAD"))

	worker := filepath.Join(t.TempDir(), "worker")
	runRunnerCmd(t, dir, "git", "-C", dir, "worktree", "add", "--detach", worker, "HEAD")
	if err := os.WriteFile(filepath.Join(worker, "changed.txt"), []byte("worker\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worker changed returned error: %v", err)
	}
	if err := os.Remove(filepath.Join(worker, "delete.txt")); err != nil {
		t.Fatalf("Remove worker delete returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worker, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worker new returned error: %v", err)
	}

	r := &Runner{cfg: &config.Config{Workspace: dir}}
	application, err := r.applyParallelWorkerPatchFallback(context.Background(), parallelTaskResult{
		Task:         plan.Task{ID: "files"},
		Paths:        TaskExecutionPaths{Workspace: worker},
		BaseRevision: base,
	}, fmt.Errorf("git apply: exit status 128: error: patch too large"))
	if err != nil {
		t.Fatalf("applyParallelWorkerPatchFallback returned error: %v", err)
	}
	if !application.FileSync || len(application.FileSyncEntries) != 3 {
		t.Fatalf("expected three file-sync entries, got %#v", application)
	}
	if got := mustReadRunnerFile(t, filepath.Join(dir, "changed.txt")); got != "worker\n" {
		t.Fatalf("expected changed file to be synced, got %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "delete.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("expected delete.txt to be removed, stat err=%v", statErr)
	}
	if got := mustReadRunnerFile(t, filepath.Join(dir, "new.txt")); got != "new\n" {
		t.Fatalf("expected new file to be synced, got %q", got)
	}

	if err := rollbackParallelWorkerFileSync(context.Background(), dir, base, application.FileSyncEntries); err != nil {
		t.Fatalf("rollbackParallelWorkerFileSync returned error: %v", err)
	}
	if got := mustReadRunnerFile(t, filepath.Join(dir, "changed.txt")); got != "base\n" {
		t.Fatalf("expected changed file to roll back to base, got %q", got)
	}
	if got := mustReadRunnerFile(t, filepath.Join(dir, "delete.txt")); got != "delete\n" {
		t.Fatalf("expected deleted file to be restored, got %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("expected added file to be removed on rollback, stat err=%v", statErr)
	}
}

func TestRunIntegratesIndependentParallelTasksInDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	trackDir := filepath.Join(t.TempDir(), "parallel-track")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: todo
    owns_paths:
      - api.txt
    verify_commands:
      - test -f api.txt
    commit_message: "feat: finish api"
  - id: ui
    title: UI
    workflow: implement
    status: todo
    owns_paths:
      - ui.txt
    verify_commands:
      - test -f ui.txt
    commit_message: "feat: finish ui"
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)

	script := `set -eu
track_dir=` + strconv.Quote(trackDir) + `
mkdir -p "$track_dir"
touch "$track_dir/{{ .Task.ID }}.started"
while [ ! -f "$track_dir/api.started" ] || [ ! -f "$track_dir/ui.started" ]; do
  sleep 0.01
done
printf '{{ .Task.ID }}\n' > "{{ .Task.ID }}.txt"
printf '{"status":"done","notes":"{{ .Task.ID }} complete"}' > "{{ .TaskResultPath }}"
`

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 2,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "execute",
						Type:            config.StepTypeExec,
						Command:         []string{"sh", "-c", script},
						RequiredOutputs: []string{"{{ .TaskResultPath }}"},
						Timeout:         "2s",
					},
				},
			},
		},
	}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	loaded, err := plan.Load(planPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	for _, task := range loaded.Tasks {
		if task.Status != plan.StatusDone {
			t.Fatalf("expected root task %q to be done after integration, got %q", task.ID, task.Status)
		}
	}

	notesFile, err := tasknotes.Load(tasknotes.Path(dir))
	if err != nil {
		t.Fatalf("Load task notes returned error: %v", err)
	}
	for _, taskID := range []string{"api", "ui"} {
		note, ok := notesFile.Find(taskID)
		if !ok {
			t.Fatalf("expected task notes for %s", taskID)
		}
		if note.Status != plan.StatusDone || note.Notes != taskID+" complete" {
			t.Fatalf("unexpected note for %s: %#v", taskID, note)
		}
	}

	log := strings.TrimSpace(runRunnerCmd(t, dir, "git", "-C", dir, "log", "--reverse", "--pretty=%s"))
	wantLog := strings.Join([]string{"initial", "feat: finish api", "feat: finish ui"}, "\n")
	if log != wantLog {
		t.Fatalf("unexpected commit order:\n%s", log)
	}
	tree := runRunnerCmd(t, dir, "git", "-C", dir, "ls-tree", "-r", "HEAD", "--name-only")
	for _, transient := range []string{
		".vibedrive/task-results",
		".vibedrive/reviews",
		".vibedrive/task-runs",
		".vibedrive/worktrees",
	} {
		if strings.Contains(tree, transient) {
			t.Fatalf("expected commit tree not to contain transient %s; tree:\n%s", transient, tree)
		}
	}
}

func TestRunRequiresTmuxForParallelAgentSteps(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: todo
    owns_paths:
      - api.txt
  - id: ui
    title: UI
    workflow: implement
    status: todo
    owns_paths:
      - ui.txt
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 2,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "finish", Type: config.StepTypeClaude, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	agent := &fakeAgent{planPath: planPath, fullscreen: true}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: agent,
		tmux: tmuxagent.NewController(tmuxagent.Options{
			LookPath: func(string) (string, error) {
				return "", os.ErrNotExist
			},
		}),
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{Strategy: config.SessionStrategySessionID, ID: "session-1"}, nil
		},
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to fail when tmux is unavailable")
	}
	if !strings.Contains(err.Error(), "tmux is required for vibedrive TUI execution") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agent.prompts) != 0 {
		t.Fatalf("expected no serial fallback prompts, got %q", strings.Join(agent.prompts, "\n"))
	}
}

func TestRunDoesNotStartTmuxWhenPlanIsComplete(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: done
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:      filepath.Join(dir, "vibedrive.yaml"),
		Workspace: dir,
		PlanFile:  planPath,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "finish", Type: config.StepTypeClaude, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	var stdout bytes.Buffer
	agent := &fakeAgent{planPath: planPath, fullscreen: true}
	r := &Runner{
		cfg:    cfg,
		stdout: &stdout,
		stderr: io.Discard,
		claude: agent,
		tmux: tmuxagent.NewController(tmuxagent.Options{
			LookPath: func(string) (string, error) {
				return "", os.ErrNotExist
			},
		}),
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(agent.prompts) != 0 {
		t.Fatalf("expected no agent prompts for complete plan, got %q", strings.Join(agent.prompts, "\n"))
	}
	if !strings.Contains(stdout.String(), "All runnable plan tasks are complete.") {
		t.Fatalf("expected completion message, got %q", stdout.String())
	}
}

func TestTmuxStatusScriptUsesActiveOnlyView(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{
		cfg: &config.Config{
			Path:      filepath.Join(dir, "vibedrive.yaml"),
			Workspace: filepath.Join(dir, "work space"),
			PlanFile:  filepath.Join(dir, "work space", "vibedrive-plan.yaml"),
		},
		executablePath: filepath.Join(dir, "vibedrive"),
	}

	script := r.tmuxStatusScript()
	for _, want := range []string{
		"view --active-only",
		"--workspace",
		"--plan",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected tmux status script to contain %q, got %q", want, script)
		}
	}
}

func TestRunIntegratesSuccessfulSiblingWhenParallelWorkerFails(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: todo
    owns_paths:
      - api.txt
    verify_commands:
      - test -f api.txt
    commit_message: "feat: finish api"
  - id: ui
    title: UI
    workflow: implement
    status: todo
    owns_paths:
      - ui.txt
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)

	script := `set -eu
if [ "{{ .Task.ID }}" = "ui" ]; then
  exit 23
fi
printf 'api\n' > api.txt
printf '{"status":"done","notes":"api complete"}' > "{{ .TaskResultPath }}"
`

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 2,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeExec, Command: []string{"sh", "-c", script}},
				},
			},
		},
	}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to report the failed worker")
	}
	if !strings.Contains(err.Error(), "ui") {
		t.Fatalf("expected worker error to name ui, got %v", err)
	}

	loaded, loadErr := plan.Load(planPath)
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	api, _ := loaded.FindTask("api")
	ui, _ := loaded.FindTask("ui")
	if api.Status != plan.StatusDone {
		t.Fatalf("expected api to be done, got %q", api.Status)
	}
	if ui.Status != plan.StatusInProgress {
		t.Fatalf("expected ui to be marked for follow-up, got %q", ui.Status)
	}

	notesFile, notesErr := tasknotes.Load(tasknotes.Path(dir))
	if notesErr != nil {
		t.Fatalf("Load task notes returned error: %v", notesErr)
	}
	uiNote, ok := notesFile.Find("ui")
	if !ok {
		t.Fatal("expected ui task note")
	}
	if !strings.Contains(uiNote.Notes, "Parallel worker failed") {
		t.Fatalf("expected worker failure note, got %q", uiNote.Notes)
	}

	uiPaths := taskExecutionPaths(cfg, "ui", 1, true)
	if _, statErr := os.Stat(uiPaths.Workspace); statErr != nil {
		t.Fatalf("expected failed worker workspace to be preserved, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(uiPaths.ArtifactRoot); statErr != nil {
		t.Fatalf("expected failed worker artifacts to be preserved, stat err=%v", statErr)
	}
}

func TestRunTreatsBlockedParallelTaskAsTerminalProgress(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: todo
    owns_paths:
      - api.txt
    verify_commands:
      - test -f api.txt
    commit_message: "feat: finish api"
  - id: ui
    title: UI
    workflow: implement
    status: todo
    owns_paths:
      - ui.txt
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)

	script := `set -eu
if [ "{{ .Task.ID }}" = "ui" ]; then
  printf '{"status":"blocked","notes":"waiting on external dependency"}' > "{{ .TaskResultPath }}"
  exit 0
fi
printf 'api\n' > api.txt
printf '{"status":"done","notes":"api complete"}' > "{{ .TaskResultPath }}"
`
	cfg := parallelRunnerConfig(dir, planPath, script)
	r := &Runner{cfg: cfg, stdout: io.Discard, stderr: io.Discard}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	loaded, loadErr := plan.Load(planPath)
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	api, _ := loaded.FindTask("api")
	ui, _ := loaded.FindTask("ui")
	if api.Status != plan.StatusDone {
		t.Fatalf("expected api to be done, got %q", api.Status)
	}
	if ui.Status != plan.StatusBlocked {
		t.Fatalf("expected ui to be blocked, got %q", ui.Status)
	}

	notesFile, notesErr := tasknotes.Load(tasknotes.Path(dir))
	if notesErr != nil {
		t.Fatalf("Load task notes returned error: %v", notesErr)
	}
	uiNote, ok := notesFile.Find("ui")
	if !ok {
		t.Fatal("expected ui task note")
	}
	if uiNote.Status != plan.StatusBlocked || !strings.Contains(uiNote.Notes, "waiting on external dependency") {
		t.Fatalf("unexpected ui note: %#v", uiNote)
	}
}

func TestRunMarksOnlyConflictedParallelTaskForFollowUp(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: first
    title: First
    workflow: implement
    status: todo
    owns_paths:
      - first.txt
    verify_commands:
      - grep -q first shared.txt
    commit_message: "feat: first shared change"
  - id: second
    title: Second
    workflow: implement
    status: todo
    owns_paths:
      - second.txt
    verify_commands:
      - grep -q second shared.txt
    commit_message: "feat: second shared change"
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile shared returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)

	base := strings.TrimSpace(runRunnerCmd(t, dir, "git", "-C", dir, "rev-parse", "HEAD"))
	firstWorker := filepath.Join(t.TempDir(), "first")
	secondWorker := filepath.Join(t.TempDir(), "second")
	runRunnerCmd(t, dir, "git", "-C", dir, "worktree", "add", "--detach", firstWorker, "HEAD")
	runRunnerCmd(t, dir, "git", "-C", dir, "worktree", "add", "--detach", secondWorker, "HEAD")
	if err := os.WriteFile(filepath.Join(firstWorker, "shared.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatalf("WriteFile first worker shared returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secondWorker, "shared.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatalf("WriteFile second worker shared returned error: %v", err)
	}

	initialPlan, loadErr := plan.Load(planPath)
	if loadErr != nil {
		t.Fatalf("Load initial plan returned error: %v", loadErr)
	}
	firstTask, _ := initialPlan.FindTask("first")
	secondTask, _ := initialPlan.FindTask("second")

	cfg := &config.Config{
		Workspace: dir,
		PlanFile:  planPath,
		Parallel: config.ParallelConfig{
			ArtifactRoot: filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
	}
	r := &Runner{cfg: cfg, stdout: io.Discard, stderr: io.Discard}

	err := r.integrateParallelBatch(context.Background(), []parallelTaskResult{
		{
			Task:         firstTask,
			Paths:        TaskExecutionPaths{Workspace: firstWorker},
			BaseRevision: base,
			Status:       plan.StatusDone,
			Notes:        "first complete",
		},
		{
			Task:         secondTask,
			Paths:        TaskExecutionPaths{Workspace: secondWorker},
			BaseRevision: base,
			Status:       plan.StatusDone,
			Notes:        "second complete",
		},
	})
	if err != nil {
		t.Fatalf("integrateParallelBatch returned error for recorded follow-up: %v", err)
	}

	loaded, loadErr := plan.Load(planPath)
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	first, _ := loaded.FindTask("first")
	second, _ := loaded.FindTask("second")
	if first.Status != plan.StatusDone {
		t.Fatalf("expected first to be done, got %q", first.Status)
	}
	if second.Status != plan.StatusInProgress {
		t.Fatalf("expected second to be in progress, got %q", second.Status)
	}
	if got := strings.TrimSpace(mustReadRunnerFile(t, filepath.Join(dir, "shared.txt"))); got != "first" {
		t.Fatalf("expected first change to be preserved, got %q", got)
	}
	notesFile, notesErr := tasknotes.Load(tasknotes.Path(dir))
	if notesErr != nil {
		t.Fatalf("Load task notes returned error: %v", notesErr)
	}
	secondNote, ok := notesFile.Find("second")
	if !ok {
		t.Fatal("expected second task note")
	}
	if !strings.Contains(secondNote.Notes, "did not apply cleanly") {
		t.Fatalf("expected merge conflict note, got %q", secondNote.Notes)
	}
	recovery, ok := r.parallelRecoveryArtifact("second")
	if !ok {
		t.Fatal("expected rejected worker patch to be preserved for recovery")
	}
	patch := mustReadRunnerFile(t, recovery.PatchPath)
	if !strings.Contains(patch, "second") || !strings.Contains(patch, "shared.txt") {
		t.Fatalf("expected preserved patch to contain rejected worker change, got:\n%s", patch)
	}
	if _, statErr := os.Stat(recovery.MetadataPath); statErr != nil {
		t.Fatalf("expected recovery metadata to be preserved, stat err=%v", statErr)
	}
	if !strings.Contains(secondNote.Notes, recovery.PatchPath) {
		t.Fatalf("expected task note to mention recovery patch %q, got %q", recovery.PatchPath, secondNote.Notes)
	}
}

func TestParallelRecoveryPromptIsPrependedForCoder(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Workspace: dir,
		Parallel: config.ParallelConfig{
			ArtifactRoot: filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
	}
	r := &Runner{cfg: cfg}
	recovery := parallelRecoveryArtifactForTask(cfg, "api")
	if err := os.MkdirAll(recovery.Dir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(recovery.PatchPath, []byte("diff --git a/api.txt b/api.txt\n"), 0o644); err != nil {
		t.Fatalf("WriteFile patch returned error: %v", err)
	}
	if err := os.WriteFile(recovery.MetadataPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile metadata returned error: %v", err)
	}

	data := TemplateData{
		Task:          plan.Task{ID: "api"},
		TaskNotesPath: filepath.Join(dir, ".vibedrive", "task-notes.yaml"),
	}
	prompt := r.withParallelRecoveryPrompt("Execute task api.", data, config.AgentCodex, config.Step{Actor: config.StepActorCoder})
	for _, want := range []string{"Parallel recovery context", recovery.PatchPath, recovery.MetadataPath, "Execute task api."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected recovery prompt to contain %q, got:\n%s", want, prompt)
		}
	}

	reviewerPrompt := r.withParallelRecoveryPrompt("Review task api.", data, config.AgentClaude, config.Step{Actor: config.StepActorReviewer})
	if strings.Contains(reviewerPrompt, "Parallel recovery context") {
		t.Fatalf("expected reviewer prompt not to include recovery context, got:\n%s", reviewerPrompt)
	}
}

func TestRunRetriesVerificationFailureFollowUp(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: todo
    owns_paths:
      - api.txt
    verify_commands:
      - test -f api.txt
    commit_message: "feat: finish api"
  - id: bad
    title: Bad
    workflow: implement
    status: todo
    owns_paths:
      - bad.txt
    verify_commands:
      - test ! -f bad.txt
    commit_message: "feat: finish bad"
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	initRunnerGitRepo(t, dir)

	script := `set -eu
if [ "{{ .Task.ID }}" = "bad" ] && grep -q 'status: in_progress' "{{ .PlanFile }}"; then
  rm -f bad.txt
  cat > "{{ .PlanFile }}" <<EOF
project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: done
    owns_paths:
      - api.txt
    verify_commands:
      - test -f api.txt
    commit_message: "feat: finish api"
  - id: bad
    title: Bad
    workflow: implement
    status: done
    owns_paths:
      - bad.txt
    verify_commands:
      - test ! -f bad.txt
    commit_message: "feat: finish bad"
EOF
  cat > "{{ .TaskNotesPath }}" <<EOF
tasks:
  - id: bad
    status: done
    notes: repaired verification failure
EOF
  printf '{"status":"done","notes":"bad repaired"}' > "{{ .TaskResultPath }}"
  exit 0
fi
printf '{{ .Task.ID }}\n' > "{{ .Task.ID }}.txt"
printf '{"status":"done","notes":"{{ .Task.ID }} complete"}' > "{{ .TaskResultPath }}"
`
	cfg := parallelRunnerConfig(dir, planPath, script)
	r := &Runner{cfg: cfg, stdout: io.Discard, stderr: io.Discard}

	err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	loaded, loadErr := plan.Load(planPath)
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	api, _ := loaded.FindTask("api")
	bad, _ := loaded.FindTask("bad")
	if api.Status != plan.StatusDone {
		t.Fatalf("expected api to be done, got %q", api.Status)
	}
	if bad.Status != plan.StatusDone {
		t.Fatalf("expected bad to be done after follow-up, got %q", bad.Status)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "api.txt")); statErr != nil {
		t.Fatalf("expected api.txt to remain, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "bad.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed task changes to be rolled back, stat err=%v", statErr)
	}
	notesFile, notesErr := tasknotes.Load(tasknotes.Path(dir))
	if notesErr != nil {
		t.Fatalf("Load task notes returned error: %v", notesErr)
	}
	badNote, ok := notesFile.Find("bad")
	if !ok {
		t.Fatal("expected bad task note")
	}
	if !strings.Contains(badNote.Notes, "repaired verification failure") {
		t.Fatalf("expected repaired verification note, got %q", badNote.Notes)
	}

	badPaths := taskExecutionPaths(cfg, "bad", 1, true)
	if _, statErr := os.Stat(badPaths.Workspace); statErr != nil {
		t.Fatalf("expected unverified worker workspace to be preserved, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(badPaths.ArtifactRoot); statErr != nil {
		t.Fatalf("expected unverified worker artifacts to be preserved, stat err=%v", statErr)
	}
}

func TestRunKeepsConflictingReadyTasksSerialWithParallelismConfigured(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: api
    title: API
    workflow: implement
    status: todo
    owns_paths:
      - internal/api/**
  - id: docs
    title: Docs
    workflow: implement
    status: todo
    owns_paths:
      - internal/api/docs.md
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 2,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "finish", Type: config.StepTypeClaude, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	var mu sync.Mutex
	active := 0
	maxActive := 0
	agent := &fakeAgent{
		planPath: planPath,
		onRun: func(_ string) error {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
			return nil
		},
	}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: agent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if maxActive != 1 {
		t.Fatalf("expected conflicting tasks to remain serial, observed %d active prompts", maxActive)
	}
	if got := strings.Join(agent.prompts, "\n"); got != "finish task api\nfinish task docs" {
		t.Fatalf("unexpected prompt order:\n%s", got)
	}
}

func TestRunReportsFailedRequiredOutputRepair(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "review",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorReviewer,
						Prompt:          "review {{ .Task.ID }}",
						RequiredOutputs: []string{"artifacts/custom-review.json"},
					},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to fail when a step does not create its required output")
	}
	if !strings.Contains(err.Error(), `step "review" failed: step "review" still has missing or invalid required outputs after codex was asked to repair them`) {
		t.Fatalf("expected repair-attempted required output error, got %q", err)
	}
	if strings.Contains(err.Error(), `step "review" did not produce required output`) {
		t.Fatalf("expected final agent-step error to mention the failed repair, got %q", err)
	}
	if len(codexAgent.prompts) != 2 {
		t.Fatalf("expected original and repair prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if codexAgent.prompts[0] != "review scaffold" {
		t.Fatalf("expected first prompt to review, got %q", codexAgent.prompts[0])
	}
	if !strings.Contains(codexAgent.prompts[1], "finished without creating these required output files") {
		t.Fatalf("expected repair prompt to explain missing output, got %q", codexAgent.prompts[1])
	}
	if !strings.Contains(codexAgent.prompts[1], "artifacts/custom-review.json") {
		t.Fatalf("expected repair prompt to name missing artifact, got %q", codexAgent.prompts[1])
	}
	if got := codexAgent.prompts[len(codexAgent.prompts)-1]; got == "finish task scaffold" {
		t.Fatalf("expected runner to stop before later steps, got prompts:\n%s", got)
	}
}

func TestRunAsksAgentToRepairMissingRequiredOutput(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "review",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorReviewer,
						Prompt:          "review {{ .Task.ID }}",
						RequiredOutputs: []string{"{{ .ReviewPath }}"},
					},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	reviewPath := automation.ReviewPath(dir, "scaffold")
	codexAgent := &fakeCodex{
		planPath: planPath,
		onRun: func(prompt string) error {
			if !strings.Contains(prompt, "finished without creating these required output files") {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(reviewPath), 0o755); err != nil {
				return err
			}
			return os.WriteFile(reviewPath, []byte(`{"decision":"approved","summary":"ok","findings":[]}`+"\n"), 0o644)
		},
	}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(codexAgent.prompts) != 3 {
		t.Fatalf("expected original, repair, and finish prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if codexAgent.prompts[0] != "review scaffold" {
		t.Fatalf("expected first prompt to review, got %q", codexAgent.prompts[0])
	}
	if !strings.Contains(codexAgent.prompts[1], reviewPath) {
		t.Fatalf("expected repair prompt to name %s, got %q", reviewPath, codexAgent.prompts[1])
	}
	if codexAgent.prompts[2] != "finish task scaffold" {
		t.Fatalf("expected runner to continue after required output repair, got %q", codexAgent.prompts[2])
	}
}

func TestRunAsksClaudeReviewerToRepairMissingRequiredOutput(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "peer-review",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorReviewer,
						Prompt:          "review {{ .Task.ID }}",
						RequiredOutputs: []string{"{{ .ReviewPath }}"},
					},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	reviewPath := automation.ReviewPath(dir, "scaffold")
	claudeAgent := &fakeAgent{
		planPath: planPath,
		onRun: func(prompt string) error {
			if !strings.Contains(prompt, "finished without creating these required output files") {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(reviewPath), 0o755); err != nil {
				return err
			}
			return os.WriteFile(reviewPath, []byte(`{"decision":"approved","summary":"ok","findings":[]}`+"\n"), 0o644)
		},
	}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(claudeAgent.prompts) != 2 {
		t.Fatalf("expected review and repair prompts, got:\n%s", strings.Join(claudeAgent.prompts, "\n---\n"))
	}
	if claudeAgent.prompts[0] != "review scaffold" {
		t.Fatalf("expected first Claude prompt to review, got %q", claudeAgent.prompts[0])
	}
	if !strings.Contains(claudeAgent.prompts[1], reviewPath) {
		t.Fatalf("expected Claude repair prompt to name %s, got %q", reviewPath, claudeAgent.prompts[1])
	}
	if strings.Join(claudeAgent.sessionIDs, ",") != "session-1,session-1" {
		t.Fatalf("expected review and repair to use the same Claude session, got %q", strings.Join(claudeAgent.sessionIDs, ","))
	}
	if got := strings.Join(codexAgent.prompts, "\n"); got != "finish task scaffold" {
		t.Fatalf("expected runner to continue after Claude repair, got coder prompts:\n%s", got)
	}
}

func TestRunSynthesizesReviewFallbackAfterFailedRepair(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	resultPath := automation.ResultPath(dir, "scaffold")
	reviewPath := automation.ReviewPath(dir, "scaffold")

	cfg := &config.Config{
		Path:      filepath.Join(dir, "vibedrive.yaml"),
		Workspace: dir,
		PlanFile:  planPath,
		Coder:     config.AgentCodex,
		Reviewer:  config.AgentClaude,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
	}
	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	err := r.runSteps(context.Background(), []config.Step{
		{
			Name:            "peer-review",
			Type:            config.StepTypeAgent,
			Actor:           config.StepActorReviewer,
			Prompt:          "review {{ .Task.ID }}",
			RequiredOutputs: []string{"{{ .ReviewPath }}"},
		},
		{
			Name:   "address-peer-review",
			Type:   config.StepTypeAgent,
			Actor:  config.StepActorCoder,
			Prompt: "address {{ .Task.ID }}",
		},
	}, TemplateData{
		Workspace:      dir,
		PlanFile:       planPath,
		Task:           plan.Task{ID: "scaffold"},
		TaskResultPath: resultPath,
		ReviewPath:     reviewPath,
	})
	if err != nil {
		t.Fatalf("runSteps returned error: %v", err)
	}
	if len(claudeAgent.prompts) != 2 {
		t.Fatalf("expected original and repair prompts, got:\n%s", strings.Join(claudeAgent.prompts, "\n---\n"))
	}
	if !strings.Contains(claudeAgent.prompts[1], "finished without creating these required output files") {
		t.Fatalf("expected Claude repair prompt, got %q", claudeAgent.prompts[1])
	}
	if got := strings.Join(codexAgent.prompts, "\n"); got != "address scaffold" {
		t.Fatalf("expected runner to continue to address step, got coder prompts:\n%s", got)
	}

	var review reviewOutput
	readRunnerJSONFile(t, reviewPath, &review)
	if review.Decision != "changes_requested" {
		t.Fatalf("expected fallback review to request changes, got %q", review.Decision)
	}
	if len(review.Findings) != 1 || !strings.Contains(review.Findings[0], "not peer-reviewed") {
		t.Fatalf("expected fallback finding to prevent finalizing as reviewed, got %#v", review.Findings)
	}

	var result automation.TaskResult
	readRunnerJSONFile(t, resultPath, &result)
	if result.Status != plan.StatusInProgress {
		t.Fatalf("expected fallback task result status %q, got %q", plan.StatusInProgress, result.Status)
	}
	if !strings.Contains(result.Notes, "peer review can be retried") {
		t.Fatalf("expected fallback task result notes to explain retry, got %q", result.Notes)
	}
}

func TestRunStepSynthesizesInProgressTaskResultWhenAgentOmitsResultAfterRepair(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	resultPath := automation.ResultPath(dir, "scaffold")

	cfg := &config.Config{
		Path:      filepath.Join(dir, "vibedrive.yaml"),
		Workspace: dir,
		PlanFile:  planPath,
		Coder:     config.AgentCodex,
	}
	codexAgent := &fakeCodex{planPath: planPath}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	err := r.runStep(context.Background(), nil, &codex.Session{}, config.Step{
		Name:            "execute-task",
		Type:            config.StepTypeAgent,
		Actor:           config.StepActorCoder,
		Prompt:          "execute {{ .Task.ID }}",
		RequiredOutputs: []string{"{{ .TaskResultPath }}"},
	}, TemplateData{
		Workspace:      dir,
		PlanFile:       planPath,
		Task:           plan.Task{ID: "scaffold"},
		TaskResultPath: resultPath,
	})
	if err != nil {
		t.Fatalf("runStep returned error: %v", err)
	}
	if len(codexAgent.prompts) != 2 {
		t.Fatalf("expected original and repair prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}

	var result automation.TaskResult
	readRunnerJSONFile(t, resultPath, &result)
	if result.Status != plan.StatusInProgress {
		t.Fatalf("expected synthesized status %q, got %q", plan.StatusInProgress, result.Status)
	}
	for _, want := range []string{"execute-task", "repair prompt", "later run can continue"} {
		if !strings.Contains(result.Notes, want) {
			t.Fatalf("expected synthesized notes to contain %q, got %q", want, result.Notes)
		}
	}
}

func TestRunStepsStopsIsolatedWorkflowAfterSynthesizedTaskResult(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	resultPath := automation.ResultPath(dir, "scaffold")

	cfg := &config.Config{
		Path:      filepath.Join(dir, "vibedrive.yaml"),
		Workspace: dir,
		PlanFile:  planPath,
		Coder:     config.AgentCodex,
	}
	codexAgent := &fakeCodex{planPath: planPath}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	err := r.runSteps(context.Background(), []config.Step{
		{
			Name:            "execute-task",
			Type:            config.StepTypeAgent,
			Actor:           config.StepActorCoder,
			Prompt:          "execute {{ .Task.ID }}",
			RequiredOutputs: []string{"{{ .TaskResultPath }}"},
		},
		{
			Name:   "peer-review",
			Type:   config.StepTypeAgent,
			Actor:  config.StepActorReviewer,
			Prompt: "review {{ .Task.ID }}",
		},
	}, TemplateData{
		Isolated:       true,
		Workspace:      dir,
		PlanFile:       planPath,
		Task:           plan.Task{ID: "scaffold"},
		TaskResultPath: resultPath,
	})
	if err != nil {
		t.Fatalf("runSteps returned error: %v", err)
	}
	if len(codexAgent.prompts) != 2 {
		t.Fatalf("expected execute and repair prompts only, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if codexAgent.prompts[0] != "execute scaffold" {
		t.Fatalf("expected execute prompt first, got %q", codexAgent.prompts[0])
	}
	if !strings.Contains(codexAgent.prompts[1], "finished without creating these required output files") {
		t.Fatalf("expected repair prompt second, got %q", codexAgent.prompts[1])
	}

	var result automation.TaskResult
	readRunnerJSONFile(t, resultPath, &result)
	if result.Status != plan.StatusInProgress {
		t.Fatalf("expected synthesized status %q, got %q", plan.StatusInProgress, result.Status)
	}
}

func TestRunStepsSynthesizesInProgressWhenIsolatedReviewOutputMissing(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")
	resultPath := automation.ResultPath(dir, "scaffold")
	reviewPath := automation.ReviewPath(dir, "scaffold")

	cfg := &config.Config{
		Path:      filepath.Join(dir, "vibedrive.yaml"),
		Workspace: dir,
		PlanFile:  planPath,
		Coder:     config.AgentCodex,
		Reviewer:  config.AgentCodex,
	}
	codexAgent := &fakeCodex{planPath: planPath}
	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	err := r.runSteps(context.Background(), []config.Step{
		{
			Name:            "peer-review",
			Type:            config.StepTypeAgent,
			Actor:           config.StepActorReviewer,
			Prompt:          "review {{ .Task.ID }}",
			RequiredOutputs: []string{"{{ .ReviewPath }}"},
		},
		{
			Name:   "address-peer-review",
			Type:   config.StepTypeAgent,
			Actor:  config.StepActorCoder,
			Prompt: "address {{ .Task.ID }}",
		},
	}, TemplateData{
		Isolated:       true,
		Workspace:      dir,
		PlanFile:       planPath,
		Task:           plan.Task{ID: "scaffold"},
		TaskResultPath: resultPath,
		ReviewPath:     reviewPath,
	})
	if err != nil {
		t.Fatalf("runSteps returned error: %v", err)
	}
	if len(codexAgent.prompts) != 2 {
		t.Fatalf("expected review and repair prompts only, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if codexAgent.prompts[0] != "review scaffold" {
		t.Fatalf("expected review prompt first, got %q", codexAgent.prompts[0])
	}
	if !strings.Contains(codexAgent.prompts[1], "finished without creating these required output files") {
		t.Fatalf("expected repair prompt second, got %q", codexAgent.prompts[1])
	}

	var result automation.TaskResult
	readRunnerJSONFile(t, resultPath, &result)
	if result.Status != plan.StatusInProgress {
		t.Fatalf("expected synthesized status %q, got %q", plan.StatusInProgress, result.Status)
	}
	for _, want := range []string{"peer-review", "required output was missing", filepath.Base(reviewPath)} {
		if !strings.Contains(result.Notes, want) {
			t.Fatalf("expected synthesized notes to contain %q, got %q", want, result.Notes)
		}
	}
}

func mustParallelTaskProgressSignatures(t *testing.T, file *plan.File, selected []plan.Task, notesFile *tasknotes.File) map[string]string {
	t.Helper()
	signatures, err := parallelTaskProgressSignatures(file, selected, notesFile)
	if err != nil {
		t.Fatalf("parallelTaskProgressSignatures returned error: %v", err)
	}
	return signatures
}

func TestParallelBatchProgressTrackerStopsRepeatedNonTerminalBatch(t *testing.T) {
	file := &plan.File{
		Path: "vibedrive-plan.yaml",
		Tasks: []plan.Task{
			{ID: "api", Status: plan.StatusInProgress},
			{ID: "ui", Status: plan.StatusInProgress},
		},
	}
	selected := append([]plan.Task(nil), file.Tasks...)
	tracker := &parallelBatchProgressTracker{}
	initialProgress := mustParallelTaskProgressSignatures(t, file, selected, nil)

	if err := tracker.observe(file, selected, initialProgress, nil, 2); err != nil {
		t.Fatalf("first observe returned error: %v", err)
	}
	err := tracker.observe(file, selected, initialProgress, nil, 2)
	if err == nil {
		t.Fatal("expected repeated non-terminal batch to stop")
	}
	for _, want := range []string{"api,ui", "2 consecutive iterations", "retry loop"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %v", want, err)
		}
	}
}

func TestParallelBatchProgressTrackerResetsAfterTerminalProgress(t *testing.T) {
	file := &plan.File{
		Path: "vibedrive-plan.yaml",
		Tasks: []plan.Task{
			{ID: "api", Status: plan.StatusInProgress},
			{ID: "ui", Status: plan.StatusInProgress},
		},
	}
	selected := append([]plan.Task(nil), file.Tasks...)
	tracker := &parallelBatchProgressTracker{}
	initialProgress := mustParallelTaskProgressSignatures(t, file, selected, nil)

	if err := tracker.observe(file, selected, initialProgress, nil, 2); err != nil {
		t.Fatalf("first observe returned error: %v", err)
	}
	file.Tasks[0].Status = plan.StatusDone
	if err := tracker.observe(file, selected, initialProgress, nil, 2); err != nil {
		t.Fatalf("terminal observe returned error: %v", err)
	}
	file.Tasks[0].Status = plan.StatusInProgress
	initialProgress = mustParallelTaskProgressSignatures(t, file, selected, nil)
	if err := tracker.observe(file, selected, initialProgress, nil, 2); err != nil {
		t.Fatalf("expected tracker to reset after terminal progress, got %v", err)
	}
}

func TestParallelBatchProgressTrackerResetsAfterTaskNoteProgress(t *testing.T) {
	file := &plan.File{
		Path: "vibedrive-plan.yaml",
		Tasks: []plan.Task{
			{ID: "api", Status: plan.StatusInProgress},
			{ID: "ui", Status: plan.StatusInProgress},
		},
	}
	selected := append([]plan.Task(nil), file.Tasks...)
	tracker := &parallelBatchProgressTracker{}
	initialProgress := mustParallelTaskProgressSignatures(t, file, selected, nil)
	notes := &tasknotes.File{
		Tasks: []tasknotes.Task{
			{ID: "api", Notes: "still making implementation progress"},
		},
	}

	if err := tracker.observe(file, selected, initialProgress, notes, 1); err != nil {
		t.Fatalf("expected task-note progress to reset stalled batch tracking, got %v", err)
	}
	initialProgress = mustParallelTaskProgressSignatures(t, file, selected, notes)
	if err := tracker.observe(file, selected, initialProgress, notes, 2); err != nil {
		t.Fatalf("expected first no-progress observe after reset to be allowed, got %v", err)
	}
}

func TestRunAsksAgentToRepairMalformedTaskResultJSON(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "execute",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorCoder,
						Prompt:          "produce bad task result {{ .Task.ID }}",
						RequiredOutputs: []string{"{{ .TaskResultPath }}"},
					},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	resultPath := automation.ResultPath(dir, "scaffold")
	codexAgent := &fakeCodex{
		planPath: planPath,
		onRun: func(prompt string) error {
			if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
				return err
			}
			switch {
			case prompt == "produce bad task result scaffold":
				return os.WriteFile(resultPath, []byte(`{"status":`), 0o644)
			case strings.Contains(prompt, "malformed task result JSON"):
				return os.WriteFile(resultPath, []byte(`{"status":"done","notes":"repaired"}`+"\n"), 0o644)
			default:
				return nil
			}
		},
	}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(codexAgent.prompts) != 3 {
		t.Fatalf("expected original, repair, and finish prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if !strings.Contains(codexAgent.prompts[1], "created these required output files, but they are invalid") {
		t.Fatalf("expected repair prompt to explain invalid output, got %q", codexAgent.prompts[1])
	}
	if !strings.Contains(codexAgent.prompts[1], "malformed task result JSON") {
		t.Fatalf("expected repair prompt to explain malformed JSON, got %q", codexAgent.prompts[1])
	}
	if !strings.Contains(codexAgent.prompts[1], resultPath) {
		t.Fatalf("expected repair prompt to name %s, got %q", resultPath, codexAgent.prompts[1])
	}
	if codexAgent.prompts[2] != "finish task scaffold" {
		t.Fatalf("expected runner to continue after task result repair, got %q", codexAgent.prompts[2])
	}
}

func TestRunAsksAgentToRepairMalformedReviewJSON(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "review",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorReviewer,
						Prompt:          "produce bad review {{ .Task.ID }}",
						RequiredOutputs: []string{"{{ .ReviewPath }}"},
					},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	reviewPath := automation.ReviewPath(dir, "scaffold")
	codexAgent := &fakeCodex{
		planPath: planPath,
		onRun: func(prompt string) error {
			if err := os.MkdirAll(filepath.Dir(reviewPath), 0o755); err != nil {
				return err
			}
			switch {
			case prompt == "produce bad review scaffold":
				return os.WriteFile(reviewPath, []byte(`{"decision":`), 0o644)
			case strings.Contains(prompt, "malformed peer review JSON"):
				return os.WriteFile(reviewPath, []byte(`{"decision":"approved","summary":"ok","findings":[]}`+"\n"), 0o644)
			default:
				return nil
			}
		},
	}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(codexAgent.prompts) != 3 {
		t.Fatalf("expected original, repair, and finish prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if !strings.Contains(codexAgent.prompts[1], "malformed peer review JSON") {
		t.Fatalf("expected repair prompt to explain malformed review JSON, got %q", codexAgent.prompts[1])
	}
	if !strings.Contains(codexAgent.prompts[1], reviewPath) {
		t.Fatalf("expected repair prompt to name %s, got %q", reviewPath, codexAgent.prompts[1])
	}
	if codexAgent.prompts[2] != "finish task scaffold" {
		t.Fatalf("expected runner to continue after review repair, got %q", codexAgent.prompts[2])
	}
}

func TestRunAsksAgentToRepairInvalidTaskNotesYAML(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "break-notes", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "break task notes {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		codex:  codexAgent,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(codexAgent.prompts) != 3 {
		t.Fatalf("expected original, repair, and finish prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if codexAgent.prompts[0] != "break task notes scaffold" {
		t.Fatalf("expected first prompt to break task notes, got %q", codexAgent.prompts[0])
	}
	if !strings.Contains(codexAgent.prompts[1], "does not parse") {
		t.Fatalf("expected repair prompt to explain parse failure, got %q", codexAgent.prompts[1])
	}
	if !strings.Contains(codexAgent.prompts[1], "task-notes.yaml") {
		t.Fatalf("expected repair prompt to name task notes path, got %q", codexAgent.prompts[1])
	}
	if codexAgent.prompts[2] != "finish task scaffold" {
		t.Fatalf("expected runner to continue after notes repair, got %q", codexAgent.prompts[2])
	}

	notesFile, err := tasknotes.Load(tasknotes.Path(dir))
	if err != nil {
		t.Fatalf("expected repaired task notes to parse, got %v", err)
	}
	note, ok := notesFile.Find("scaffold")
	if !ok {
		t.Fatal("expected scaffold task note")
	}
	if note.Status != plan.StatusDone {
		t.Fatalf("expected task note status %q, got %q", plan.StatusDone, note.Status)
	}
}

func TestRunContinuesAfterFinalizeVerificationFailure(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
    verify_commands:
      - test -f fixed.txt
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	finalizePath := filepath.Join(dir, "fake-vibedrive")
	finalizeScript := `#!/bin/sh
set -eu
if [ "$1" != "task" ] || [ "$2" != "finalize" ]; then
  echo "unexpected args: $*" >&2
  exit 2
fi
shift 2
workspace=""
plan=""
task=""
result=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --workspace)
      workspace="$2"
      shift 2
      ;;
    --plan)
      plan="$2"
      shift 2
      ;;
    --task)
      task="$2"
      shift 2
      ;;
    --result)
      result="$2"
      shift 2
      ;;
    --message)
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
count_file="$workspace/finalize-count"
count=0
if [ -f "$count_file" ]; then
  count=$(cat "$count_file")
fi
printf '%s\n' "$((count + 1))" > "$count_file"
mkdir -p "$workspace/.vibedrive"
if [ "$count" -eq 0 ]; then
  cat > "$plan" <<EOF
project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: in_progress
    verify_commands:
      - test -f fixed.txt
EOF
  cat > "$workspace/.vibedrive/task-notes.yaml" <<EOF
tasks:
  - id: scaffold
    status: in_progress
    notes: Verification failed while running "test -f fixed.txt".
EOF
  rm -f "$result"
  exit 1
fi
cat > "$plan" <<EOF
project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: done
    verify_commands:
      - test -f fixed.txt
EOF
cat > "$workspace/.vibedrive/task-notes.yaml" <<EOF
tasks:
  - id: scaffold
    status: done
    notes: fixed verification
EOF
rm -f "$result"
`
	if err := os.WriteFile(finalizePath, []byte(finalizeScript), 0o755); err != nil {
		t.Fatalf("WriteFile fake finalizer returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 2,
		DefaultWorkflow:      "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "execute",
						Type:            config.StepTypeAgent,
						Actor:           config.StepActorCoder,
						Prompt:          "implement {{ .Task.ID }}",
						RequiredOutputs: []string{"{{ .TaskResultPath }}"},
					},
					{
						Name: "finalize-task",
						Type: config.StepTypeExec,
						Command: []string{
							"{{ .ExecutablePath }}",
							"task",
							"finalize",
							"--workspace",
							"{{ .Workspace }}",
							"--plan",
							"{{ .PlanFile }}",
							"--task",
							"{{ .Task.ID }}",
							"--result",
							"{{ .TaskResultPath }}",
							"--message",
							"{{ .Task.Title }}",
						},
					},
				},
			},
		},
	}

	resultPath := automation.ResultPath(dir, "scaffold")
	codexAgent := &fakeCodex{
		planPath: planPath,
		onRun: func(prompt string) error {
			if strings.Contains(prompt, "Verification follow-up context") {
				if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("fixed\n"), 0o644); err != nil {
					return err
				}
			}
			if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
				return err
			}
			return os.WriteFile(resultPath, []byte(`{"status":"done","notes":"implementation complete"}`+"\n"), 0o644)
		},
	}

	r := &Runner{
		cfg:            cfg,
		stdout:         io.Discard,
		stderr:         io.Discard,
		codex:          codexAgent,
		executablePath: finalizePath,
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(codexAgent.prompts) != 2 {
		t.Fatalf("expected original and verification follow-up prompts, got:\n%s", strings.Join(codexAgent.prompts, "\n---\n"))
	}
	if codexAgent.prompts[0] != "implement scaffold" {
		t.Fatalf("expected first prompt to implement, got %q", codexAgent.prompts[0])
	}
	if !strings.Contains(codexAgent.prompts[1], "Verification follow-up context") {
		t.Fatalf("expected second prompt to include verification context, got %q", codexAgent.prompts[1])
	}
	if !strings.Contains(codexAgent.prompts[1], "test -f fixed.txt") {
		t.Fatalf("expected second prompt to include failing command, got %q", codexAgent.prompts[1])
	}

	loaded, err := plan.Load(planPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	task, ok := loaded.FindTask("scaffold")
	if !ok {
		t.Fatal("expected scaffold task")
	}
	if task.Status != plan.StatusDone {
		t.Fatalf("expected task status %q, got %q", plan.StatusDone, task.Status)
	}
	if got := strings.TrimSpace(mustReadRunnerFile(t, filepath.Join(dir, "finalize-count"))); got != "2" {
		t.Fatalf("expected finalizer to run twice, got %q", got)
	}
}

func TestRunDispatchesCoderAndReviewerStepsWithClaudeReviewer(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(codexAgent.prompts, "\n"); got != "analyze scaffold\nfinish task scaffold" {
		t.Fatalf("unexpected coder prompts:\n%s", got)
	}
	if got := strings.Join(claudeAgent.prompts, "\n"); got != "review scaffold" {
		t.Fatalf("unexpected reviewer prompts:\n%s", got)
	}
}

func TestRunClosesSharedSessionsInReverseCreationOrder(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	closeEvents := []string{}
	claudeAgent := &fakeAgent{planPath: planPath, closeEvents: &closeEvents}
	codexAgent := &fakeCodex{planPath: planPath, closeEvents: &closeEvents}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
		newCodexSession: func() (*codex.Session, error) {
			return &codex.Session{}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(closeEvents, ","); got != "claude,codex" {
		t.Fatalf("expected shared sessions to close in reverse creation order, got %q", got)
	}
}

func TestRunDispatchesCoderAndReviewerStepsWithCodexReviewer(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentClaude,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(claudeAgent.prompts, "\n"); got != "analyze scaffold\nfinish task scaffold" {
		t.Fatalf("unexpected coder prompts:\n%s", got)
	}
	if got := strings.Join(codexAgent.prompts, "\n"); got != "review scaffold" {
		t.Fatalf("unexpected reviewer prompts:\n%s", got)
	}
}

func TestRunAllowsSameAgentForCoderAndReviewer(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentCodex,
		Reviewer:             config.AgentCodex,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "progress task {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	codexAgent := &fakeCodex{planPath: planPath}

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		codex:  codexAgent,
		newSession: func(_ string) (*claude.Session, error) {
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-1",
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := strings.Join(codexAgent.prompts, "\n"); got != "analyze scaffold\nprogress task scaffold\nfinish task scaffold" {
		t.Fatalf("unexpected codex prompts when coder and reviewer match:\n%s", got)
	}
	if got := strings.Join(claudeAgent.prompts, "\n"); got != "" {
		t.Fatalf("expected claude to stay unused, got prompts:\n%s", got)
	}
}

func TestRunReusesSingleClaudeSessionWhenCoderAndReviewerMatch(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "vibedrive-plan.yaml")

	content := `project:
  name: demo
tasks:
  - id: scaffold
    title: Scaffold repo
    workflow: implement
    status: todo
`
	if err := os.WriteFile(planPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		Coder:                config.AgentClaude,
		Reviewer:             config.AgentClaude,
		MaxStalledIterations: 1,
		Claude: config.ClaudeConfig{
			SessionStrategy: config.SessionStrategySessionID,
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{Name: "execute", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "analyze {{ .Task.ID }}"},
					{Name: "review", Type: config.StepTypeAgent, Actor: config.StepActorReviewer, Prompt: "review {{ .Task.ID }}"},
					{Name: "finish", Type: config.StepTypeAgent, Actor: config.StepActorCoder, Prompt: "finish task {{ .Task.ID }}"},
				},
			},
		},
	}

	claudeAgent := &fakeAgent{planPath: planPath}
	sessionCount := 0

	r := &Runner{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		claude: claudeAgent,
		newSession: func(_ string) (*claude.Session, error) {
			sessionCount++
			return &claude.Session{
				Strategy: config.SessionStrategySessionID,
				ID:       "session-" + string(rune('0'+sessionCount)),
			}, nil
		},
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if sessionCount != 1 {
		t.Fatalf("expected 1 shared claude session, got %d", sessionCount)
	}

	wantPromptSessions := []string{"session-1", "session-1", "session-1"}
	if strings.Join(claudeAgent.sessionIDs, ",") != strings.Join(wantPromptSessions, ",") {
		t.Fatalf("expected prompt session IDs %v, got %v", wantPromptSessions, claudeAgent.sessionIDs)
	}

	wantClosedSessions := []string{"session-1"}
	if strings.Join(claudeAgent.closedSessionID, ",") != strings.Join(wantClosedSessions, ",") {
		t.Fatalf("expected closed session IDs %v, got %v", wantClosedSessions, claudeAgent.closedSessionID)
	}
}

func parallelRunnerConfig(dir, planPath, script string) *config.Config {
	return &config.Config{
		Path:                 filepath.Join(dir, "vibedrive.yaml"),
		Workspace:            dir,
		PlanFile:             planPath,
		MaxStalledIterations: 1,
		Parallel: config.ParallelConfig{
			Enabled:        true,
			MaxParallelism: 2,
			WorktreeRoot:   filepath.Join(dir, config.DefaultParallelWorktreeRoot),
			ArtifactRoot:   filepath.Join(dir, config.DefaultParallelArtifactRoot),
		},
		DefaultWorkflow: "implement",
		Workflows: map[string]config.Workflow{
			"implement": {
				Steps: []config.Step{
					{
						Name:            "execute",
						Type:            config.StepTypeExec,
						Command:         []string{"sh", "-c", script},
						RequiredOutputs: []string{"{{ .TaskResultPath }}"},
						Timeout:         "2s",
					},
				},
			},
		},
	}
}

func initRunnerGitRepo(t *testing.T, dir string) {
	t.Helper()

	runRunnerCmd(t, dir, "git", "-C", dir, "init")
	runRunnerCmd(t, dir, "git", "-C", dir, "config", "user.email", "test@example.com")
	runRunnerCmd(t, dir, "git", "-C", dir, "config", "user.name", "Test User")
	runRunnerCmd(t, dir, "git", "-C", dir, "add", "-A")
	runRunnerCmd(t, dir, "git", "-C", dir, "commit", "-m", "initial")
}

func runRunnerCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func mustReadRunnerFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned error: %v", path, err)
	}
	return string(data)
}

func mustReadRunnerBytes(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned error: %v", path, err)
	}
	return data
}

func readRunnerJSONFile(t *testing.T, path string, target any) {
	t.Helper()

	data := mustReadRunnerBytes(t, path)
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("Unmarshal(%q) returned error: %v", path, err)
	}
}

func runnerArtifactEntry(t *testing.T, manifest diagnostics.Manifest, kind string) diagnostics.ArtifactEntry {
	t.Helper()

	for _, entry := range manifest.Artifacts {
		if entry.Kind == kind {
			return entry
		}
	}
	t.Fatalf("artifact %q not found in manifest: %#v", kind, manifest.Artifacts)
	return diagnostics.ArtifactEntry{}
}

func writeRunnerVersionCommand(t *testing.T, dir, name, version string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat <<'VIBEDRIVE_VERSION'\n%s\nVIBEDRIVE_VERSION\n", version)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	return path
}

func updateTask(path, taskID, status, notes string) error {
	file, err := plan.Load(path)
	if err != nil {
		return err
	}

	for i := range file.Tasks {
		if file.Tasks[i].ID == taskID {
			file.Tasks[i].Status = status
			file.Tasks[i].Notes = notes
			if err := file.Save(); err != nil {
				return err
			}

			notesFile, err := tasknotes.Load(tasknotes.Path(filepath.Dir(path)))
			if err != nil {
				return err
			}
			if err := notesFile.Upsert(taskID, status, notes); err != nil {
				return err
			}
			return notesFile.Save()
		}
	}

	return os.ErrNotExist
}

func handleFakePrompt(prompt, planPath string) error {
	if strings.HasPrefix(prompt, "write ") {
		return writeOutput(strings.TrimPrefix(prompt, "write "))
	}
	if strings.HasPrefix(prompt, "finish task ") {
		taskID := strings.TrimPrefix(prompt, "finish task ")
		return updateTask(planPath, taskID, plan.StatusDone, "done")
	}
	if strings.HasPrefix(prompt, "progress task ") {
		taskID := strings.TrimPrefix(prompt, "progress task ")
		return updateTask(planPath, taskID, plan.StatusInProgress, "still working")
	}
	if strings.HasPrefix(prompt, "break task notes ") {
		taskID := strings.TrimPrefix(prompt, "break task notes ")
		return writeTaskNotes(filepath.Dir(planPath), "tasks:\n  - id: "+taskID+"\n    notes: [broken\n")
	}
	if strings.Contains(prompt, "does not parse") && strings.Contains(prompt, "task-notes.yaml") {
		return writeTaskNotes(filepath.Dir(planPath), "tasks:\n  - id: scaffold\n    status: in_progress\n    notes: repaired\n")
	}

	return nil
}

func writeTaskNotes(workspace, content string) error {
	path := tasknotes.Path(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func writeOutput(path string) error {
	return os.WriteFile(path, []byte("{}\n"), 0o644)
}
