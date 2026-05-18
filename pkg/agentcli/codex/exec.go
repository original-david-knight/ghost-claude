package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"vibedrive/internal/diagnostics"
)

const (
	execFailureNonZeroExit = "codex_exec_non_zero_exit"
	execFailureStartup     = "codex_exec_startup_error"
	execFailureStdin       = "codex_exec_stdin_write"
	execFailureTimeout     = "codex_exec_timeout"
	execFailureCanceled    = "codex_exec_canceled"
)

type Diagnostics struct {
	Identity     diagnostics.Identity
	ParentStdout diagnostics.ByteArtifact
	ParentStderr diagnostics.ByteArtifact
}

type execDiagnosticsContextKey struct{}

func WithDiagnostics(ctx context.Context, value Diagnostics) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, execDiagnosticsContextKey{}, value)
}

func (c *Client) runExecPrompt(ctx context.Context, prompt string) error {
	args := c.execArgs()
	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = c.workdir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("codex exec stdin pipe: %w", err)
	}

	stdoutTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
	stderrTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
	combinedTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
	cmd.Stdout = execOutputWriter(c.stdout, stdoutTail, combinedTail)
	cmd.Stderr = execOutputWriter(c.stderr, stderrTail, combinedTail)

	if err := cmd.Start(); err != nil {
		c.captureExecFailure(ctx, execFailureStartup, err, args, prompt, nil, "", false, stdoutTail, stderrTail, combinedTail)
		return fmt.Errorf("codex exec start: %w", err)
	}

	writeErr := writeExecPrompt(stdin, prompt)
	waitErr := cmd.Wait()
	if waitErr == nil && writeErr == nil {
		return nil
	}

	failureErr := execRunError(writeErr, waitErr)
	failurePath := execFailurePath(ctx, writeErr, waitErr)
	exitCode, signal := execFailureStatus(waitErr)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	c.captureExecFailure(ctx, failurePath, failureErr, args, prompt, exitCode, signal, timedOut, stdoutTail, stderrTail, combinedTail)

	if timedOut {
		return fmt.Errorf("codex exec timed out: %w", failureErr)
	}
	return fmt.Errorf("codex exec failed: %w", failureErr)
}

func (c *Client) execArgs() []string {
	args := []string{"exec"}
	args = append(args, c.args...)
	return args
}

func execOutputWriter(user io.Writer, tails ...io.Writer) io.Writer {
	return execOutputFanout{
		display: user,
		tails:   tails,
	}
}

type execOutputFanout struct {
	display io.Writer
	tails   []io.Writer
}

func (w execOutputFanout) Write(p []byte) (int, error) {
	for _, tail := range w.tails {
		if tail == nil {
			continue
		}
		n, err := tail.Write(p)
		if err != nil {
			return n, err
		}
		if n != len(p) {
			return n, io.ErrShortWrite
		}
	}

	if w.display != nil {
		_, _ = w.display.Write(p)
	}
	return len(p), nil
}

func writeExecPrompt(stdin io.WriteCloser, prompt string) error {
	_, writeErr := io.WriteString(stdin, prompt)
	closeErr := stdin.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func execRunError(writeErr, waitErr error) error {
	switch {
	case waitErr != nil && writeErr != nil:
		return fmt.Errorf("%w; also failed to write prompt to stdin: %v", waitErr, writeErr)
	case waitErr != nil:
		return waitErr
	default:
		return fmt.Errorf("write prompt to stdin: %w", writeErr)
	}
}

func execFailurePath(ctx context.Context, writeErr, waitErr error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return execFailureTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return execFailureCanceled
	case waitErr != nil:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return execFailureNonZeroExit
		}
		return execFailureStartup
	case writeErr != nil:
		return execFailureStdin
	default:
		return execFailureNonZeroExit
	}
}

func execFailureStatus(err error) (*int, string) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil, ""
	}
	exitCode := exitErr.ExitCode()
	if exitCode >= 0 {
		return &exitCode, ""
	}
	if exitErr.ProcessState == nil {
		return nil, ""
	}
	const signalPrefix = "signal: "
	state := exitErr.ProcessState.String()
	if strings.HasPrefix(state, signalPrefix) {
		return nil, strings.TrimPrefix(state, signalPrefix)
	}
	return nil, ""
}

func (c *Client) captureExecFailure(
	ctx context.Context,
	failurePath string,
	err error,
	args []string,
	prompt string,
	exitCode *int,
	signal string,
	timedOut bool,
	stdoutTail *diagnostics.TailBuffer,
	stderrTail *diagnostics.TailBuffer,
	combinedTail *diagnostics.TailBuffer,
) {
	if err == nil {
		return
	}
	value := execDiagnosticsFrom(ctx)
	parentStdout := value.ParentStdout
	if !parentStdout.Available {
		parentStdout = diagnostics.UnavailableBytes()
	}
	parentStderr := value.ParentStderr
	if !parentStderr.Available {
		parentStderr = diagnostics.UnavailableBytes()
	}

	command := diagnostics.ExecCommand{
		Argv:       append([]string{c.command}, args...),
		WorkingDir: c.workdir,
		ExitCode:   exitCode,
		Signal:     signal,
		TimedOut:   timedOut,
		Env: diagnostics.ExecEnvironment{
			InheritedKeys: execInheritedEnvKeys(os.Environ()),
		},
		Extra: map[string]any{
			"prompt_delivery":      "stdin",
			"transport_subcommand": "exec",
		},
	}
	if errors.Is(ctx.Err(), context.Canceled) && !timedOut {
		command.Extra["cancelled"] = true
	}

	_, captureErr := diagnostics.New(c.workdir).CaptureExec(diagnostics.ExecCapture{
		Identity: execIdentityOrDefault(value.Identity, failurePath),
		Failure: diagnostics.Failure{
			Path:       failurePath,
			Message:    err.Error(),
			CapturedAt: time.Now(),
		},
		Transport: diagnostics.Transport{
			Kind:        "codex_exec",
			Agent:       "codex",
			Interactive: false,
		},
		Command: command,
		Prompt: &diagnostics.PromptPayload{
			Raw:               diagnostics.Bytes([]byte(prompt)),
			NormalizationMode: "utf8_replacement_lf",
			BracketedPaste:    false,
			ExtraMetadata: map[string]any{
				"delivery":             "stdin",
				"transport_subcommand": "exec",
			},
		},
		Stdout:       stdoutTail.Snapshot().Bytes(),
		Stderr:       stderrTail.Snapshot().Bytes(),
		Combined:     combinedTail.Snapshot().Bytes(),
		ParentStdout: parentStdout,
		ParentStderr: parentStderr,
	})
	if captureErr != nil && c.stderr != nil {
		fmt.Fprintf(c.stderr, "warning: failed to capture codex exec diagnostics: %v\n", captureErr)
	}
}

func execDiagnosticsFrom(ctx context.Context) Diagnostics {
	if ctx == nil {
		return Diagnostics{}
	}
	value, _ := ctx.Value(execDiagnosticsContextKey{}).(Diagnostics)
	return value
}

func execIdentityOrDefault(id diagnostics.Identity, failurePath string) diagnostics.Identity {
	if strings.TrimSpace(id.RunID) != "" &&
		strings.TrimSpace(id.TaskID) != "" &&
		strings.TrimSpace(id.StepName) != "" {
		return id
	}
	if strings.TrimSpace(failurePath) == "" {
		failurePath = "codex_exec_failure"
	}
	return diagnostics.Identity{
		RunID:    "unknown-run",
		TaskID:   "codex-exec",
		StepName: failurePath,
	}
}

func execInheritedEnvKeys(env []string) []string {
	seen := make(map[string]struct{}, len(env))
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}
