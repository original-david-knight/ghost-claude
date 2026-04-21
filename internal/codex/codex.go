package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

type Client struct {
	command string
	args    []string
	workdir string
	stdout  io.Writer
	stderr  io.Writer
}

func New(command string, args []string, workdir string, stdout, stderr io.Writer) (*Client, error) {
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}

	baseArgs := append([]string{}, args...)
	if len(baseArgs) == 0 {
		baseArgs = []string{"exec"}
	}

	return &Client{
		command: command,
		args:    baseArgs,
		workdir: workdir,
		stdout:  stdout,
		stderr:  stderr,
	}, nil
}

func (c *Client) RunPrompt(ctx context.Context, prompt string) error {
	if c.shouldFilterExecOutput() {
		return c.runPromptJSON(ctx, prompt)
	}

	return c.runPromptPassthrough(ctx, prompt)
}

func (c *Client) runPromptPassthrough(ctx context.Context, prompt string) error {
	args := append([]string{}, c.args...)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = c.workdir
	cmd.Stdout = c.stdout
	cmd.Stderr = c.stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	return nil
}

func (c *Client) runPromptJSON(ctx context.Context, prompt string) error {
	args := append([]string{}, c.args...)
	args = append(args, "--json", prompt)

	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = c.workdir

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe for %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe for %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q with args %q: %w", c.command, strings.Join(args, " "), err)
	}

	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		_, _ = io.Copy(c.stderr, stderrPipe)
	}()

	renderErr := c.renderJSONStream(stdoutPipe)
	waitErr := cmd.Wait()
	stderrWG.Wait()

	if renderErr != nil {
		return fmt.Errorf("render %q json stream: %w", c.command, renderErr)
	}
	if waitErr != nil {
		return fmt.Errorf("run %q with args %q: %w", c.command, strings.Join(args, " "), waitErr)
	}

	return nil
}

func (c *Client) shouldFilterExecOutput() bool {
	return codexSubcommand(c.args) == "exec" && !containsArg(c.args, "--json")
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func codexSubcommand(args []string) string {
	for _, arg := range args {
		switch arg {
		case "exec", "review", "login", "logout", "mcp", "plugin", "mcp-server", "app-server", "completion", "sandbox", "debug", "apply", "resume", "fork", "cloud", "exec-server", "features", "help":
			return arg
		}
	}
	return ""
}

type event struct {
	Type string `json:"type"`
	Item *item  `json:"item,omitempty"`
}

type item struct {
	Type             string       `json:"type"`
	Text             string       `json:"text,omitempty"`
	Command          string       `json:"command,omitempty"`
	AggregatedOutput string       `json:"aggregated_output,omitempty"`
	ExitCode         *int         `json:"exit_code,omitempty"`
	Changes          []fileChange `json:"changes,omitempty"`
}

type fileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (c *Client) renderJSONStream(r io.Reader) error {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		if err := c.renderJSONLine(line); err != nil {
			return err
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (c *Client) renderJSONLine(line string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}

	var evt event
	if err := json.Unmarshal([]byte(trimmed), &evt); err != nil {
		return writeText(c.stdout, trimmed)
	}

	switch evt.Type {
	case "item.started":
		return c.renderItem(evt.Item, true)
	case "item.completed":
		return c.renderItem(evt.Item, false)
	default:
		return nil
	}
}

func (c *Client) renderItem(item *item, started bool) error {
	if item == nil {
		return nil
	}

	switch item.Type {
	case "agent_message":
		if started {
			return nil
		}
		return writeText(c.stdout, item.Text)
	case "command_execution":
		return c.renderCommand(item, started)
	case "file_change":
		if started {
			return nil
		}
		for _, change := range item.Changes {
			if err := writeText(c.stdout, fmt.Sprintf("%s %s", formatChangeVerb(change.Kind), change.Path)); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Client) renderCommand(item *item, started bool) error {
	if strings.TrimSpace(item.Command) == "" {
		return nil
	}

	if started {
		return writeText(c.stdout, "$ "+item.Command)
	}

	return nil
}

func writeText(w io.Writer, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	_, err := fmt.Fprintln(w, text)
	return err
}

func formatChangeVerb(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "add":
		return "added"
	case "update":
		return "updated"
	case "delete":
		return "deleted"
	default:
		return "changed"
	}
}
