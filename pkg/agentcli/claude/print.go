package claude

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
	printFailureNonZeroExit = "claude_print_non_zero_exit"
	printFailureStartup     = "claude_print_startup_error"
	printFailureStdin       = "claude_print_stdin_write"
	printFailureTimeout     = "claude_print_timeout"
	printFailureCanceled    = "claude_print_canceled"
)

type Diagnostics struct {
	Identity     diagnostics.Identity
	ParentStdout diagnostics.ByteArtifact
	ParentStderr diagnostics.ByteArtifact
}

type printDiagnosticsContextKey struct{}

func WithDiagnostics(ctx context.Context, value Diagnostics) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, printDiagnosticsContextKey{}, value)
}

func (c *Client) runPrintPrompt(ctx context.Context, prompt string) error {
	args := c.printArgs()
	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = c.workdir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("claude print stdin pipe: %w", err)
	}

	stdoutTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
	stderrTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
	combinedTail := diagnostics.NewTailBuffer(diagnostics.ExecOutputLimit)
	cmd.Stdout = printOutputWriter(c.stdout, stdoutTail, combinedTail)
	cmd.Stderr = printOutputWriter(c.stderr, stderrTail, combinedTail)

	if err := cmd.Start(); err != nil {
		c.capturePrintFailure(ctx, printFailureStartup, err, args, prompt, nil, "", false, stdoutTail, stderrTail, combinedTail)
		return fmt.Errorf("claude print start: %w", err)
	}

	writeErr := writePrintPrompt(stdin, prompt)
	waitErr := cmd.Wait()
	if waitErr == nil && writeErr == nil {
		return nil
	}

	failureErr := printRunError(writeErr, waitErr)
	failurePath := printFailurePath(ctx, writeErr, waitErr)
	exitCode, signal := printFailureStatus(waitErr)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	c.capturePrintFailure(ctx, failurePath, failureErr, args, prompt, exitCode, signal, timedOut, stdoutTail, stderrTail, combinedTail)

	if timedOut {
		return fmt.Errorf("claude print timed out: %w", failureErr)
	}
	return fmt.Errorf("claude print failed: %w", failureErr)
}

func (c *Client) printArgs() []string {
	args := append([]string{}, c.args...)
	return append(args, "--print")
}

func printOutputWriter(user io.Writer, tails ...io.Writer) io.Writer {
	writers := make([]io.Writer, 0, len(tails)+1)
	if user != nil {
		writers = append(writers, user)
	}
	writers = append(writers, tails...)
	return io.MultiWriter(writers...)
}

func writePrintPrompt(stdin io.WriteCloser, prompt string) error {
	_, writeErr := io.WriteString(stdin, prompt)
	closeErr := stdin.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func printRunError(writeErr, waitErr error) error {
	switch {
	case waitErr != nil && writeErr != nil:
		return fmt.Errorf("%w; also failed to write prompt to stdin: %v", waitErr, writeErr)
	case waitErr != nil:
		return waitErr
	default:
		return fmt.Errorf("write prompt to stdin: %w", writeErr)
	}
}

func printFailurePath(ctx context.Context, writeErr, waitErr error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return printFailureTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return printFailureCanceled
	case waitErr != nil:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return printFailureNonZeroExit
		}
		return printFailureStartup
	case writeErr != nil:
		return printFailureStdin
	default:
		return printFailureNonZeroExit
	}
}

func printFailureStatus(err error) (*int, string) {
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

func (c *Client) capturePrintFailure(
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
	value := printDiagnosticsFrom(ctx)
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
			InheritedKeys: inheritedEnvKeys(os.Environ()),
		},
		Extra: map[string]any{
			"prompt_delivery": "stdin",
			"transport_flag":  "--print",
		},
	}
	if errors.Is(ctx.Err(), context.Canceled) && !timedOut {
		command.Extra["cancelled"] = true
	}

	_, captureErr := diagnostics.New(c.workdir).CaptureExec(diagnostics.ExecCapture{
		Identity: identityOrDefault(value.Identity, failurePath),
		Failure: diagnostics.Failure{
			Path:       failurePath,
			Message:    err.Error(),
			CapturedAt: time.Now(),
		},
		Transport: diagnostics.Transport{
			Kind:        "claude_print",
			Agent:       "claude",
			Interactive: false,
		},
		Command: command,
		Prompt: &diagnostics.PromptPayload{
			Raw:               diagnostics.Bytes([]byte(prompt)),
			NormalizationMode: "utf8_replacement_lf",
			BracketedPaste:    false,
			ExtraMetadata: map[string]any{
				"delivery":       "stdin",
				"transport_flag": "--print",
			},
		},
		Stdout:       stdoutTail.Snapshot().Bytes(),
		Stderr:       stderrTail.Snapshot().Bytes(),
		Combined:     combinedTail.Snapshot().Bytes(),
		ParentStdout: parentStdout,
		ParentStderr: parentStderr,
	})
	if captureErr != nil && c.stderr != nil {
		fmt.Fprintf(c.stderr, "warning: failed to capture claude print diagnostics: %v\n", captureErr)
	}
}

func printDiagnosticsFrom(ctx context.Context) Diagnostics {
	if ctx == nil {
		return Diagnostics{}
	}
	value, _ := ctx.Value(printDiagnosticsContextKey{}).(Diagnostics)
	return value
}

func identityOrDefault(id diagnostics.Identity, failurePath string) diagnostics.Identity {
	if strings.TrimSpace(id.RunID) != "" &&
		strings.TrimSpace(id.TaskID) != "" &&
		strings.TrimSpace(id.StepName) != "" {
		return id
	}
	if strings.TrimSpace(failurePath) == "" {
		failurePath = "claude_print_failure"
	}
	return diagnostics.Identity{
		RunID:    "unknown-run",
		TaskID:   "claude-print",
		StepName: failurePath,
	}
}

func inheritedEnvKeys(env []string) []string {
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
