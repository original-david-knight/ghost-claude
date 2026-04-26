package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"vibedrive/internal/agentlaunch"
	"vibedrive/internal/bootstrap"
	"vibedrive/internal/config"
	createpkg "vibedrive/internal/create"
)

type createStageRunner interface {
	Run(ctx context.Context, stage createpkg.Stage, authorAgent, criticAgent string) error
}

type createStageRunnerFactory func(configPath, workspace string, stdout, stderr io.Writer, confirm createpkg.ConfirmFunc) (createStageRunner, error)

type createPlanningRunner func(ctx context.Context, configPath string, sourceArgs []string, force bool, author, critic string) error

type createMenuInput interface {
	Choose(ctx context.Context, state createpkg.State, entries []createMenuEntry) (createMenuEntry, error)
}

type createConfirmer interface {
	Confirm(ctx context.Context, prompt string) (bool, error)
}

type createPromptInput interface {
	createMenuInput
	createConfirmer
}

type createCommandDeps struct {
	input              createPromptInput
	stageRunnerFactory createStageRunnerFactory
	planningRunner     createPlanningRunner
	stdout             io.Writer
	stderr             io.Writer
}

type createMenuEntry struct {
	Label    string
	Stage    createpkg.Stage
	Planning bool
	Stop     bool
}

func createCommand(ctx context.Context, args []string) error {
	input := newTerminalCreateInput(os.Stdin, os.Stdout)
	return runCreateCommand(ctx, args, createCommandDeps{
		input:              input,
		stageRunnerFactory: defaultCreateStageRunnerFactory,
		planningRunner: func(ctx context.Context, configPath string, sourceArgs []string, force bool, author, critic string) error {
			return bootstrap.New(os.Stdout, os.Stderr).Run(ctx, configPath, sourceArgs, force, author, critic)
		},
		stdout: os.Stdout,
		stderr: os.Stderr,
	})
}

func runCreateCommand(ctx context.Context, args []string, deps createCommandDeps) error {
	deps = deps.withDefaults()

	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(deps.stdout)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprint(out, `Usage:
  vibedrive create [-workspace DIR] [--author claude|codex] [--critic claude|codex]

Create stages:
  Product Definition interviews the user and writes or updates DESIGN.md.
  UX Review improves product and user-experience coverage in DESIGN.md.
  Technical Review adds implementation guidance without creating a task-by-task plan.
  Planning appears only when DESIGN.md exists and runs init with DESIGN.md as the only source.
  Stop exits without changing DESIGN.md or create state.

Notes:
  The author defaults to codex. The critic defaults to claude.
  After each author stage, vibedrive can ask whether to run a fresh critic instance for a second opinion.
  --dry-run, --resume, and a positional idea argument are intentionally not supported.

Flags:
`)
		fs.PrintDefaults()
	}

	workspaceFlag := fs.String("workspace", "", "Workspace directory where DESIGN.md and create state live")
	author := fs.String("author", config.AgentCodex, "Create-stage author to use: claude or codex")
	critic := fs.String("critic", config.AgentClaude, "Create-stage critic to use: claude or codex")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("create does not accept positional arguments")
	}

	resolvedAuthor, err := resolveInitAuthor(*author)
	if err != nil {
		return err
	}
	resolvedCritic, err := resolveInitCritic(*critic)
	if err != nil {
		return err
	}

	configPath, err := resolveConfigPath("vibedrive.yaml", *workspaceFlag)
	if err != nil {
		return err
	}
	workspace := filepath.Dir(configPath)

	for {
		state, err := createpkg.Read(workspace)
		if err != nil {
			return err
		}

		hasDesign, err := createDesignExists(workspace)
		if err != nil {
			return err
		}

		choice, err := deps.input.Choose(ctx, state, createMenuEntries(hasDesign))
		if err != nil {
			return err
		}

		switch {
		case choice.Stop:
			return nil
		case choice.Planning:
			if !hasDesign {
				return fmt.Errorf("planning requires DESIGN.md at the workspace root")
			}
			return deps.planningRunner(ctx, configPath, []string{createpkg.DesignFileName()}, false, resolvedAuthor, resolvedCritic)
		default:
			runner, err := deps.stageRunnerFactory(configPath, workspace, deps.stdout, deps.stderr, deps.input.Confirm)
			if err != nil {
				return err
			}
			if err := runner.Run(ctx, choice.Stage, resolvedAuthor, resolvedCritic); err != nil {
				return err
			}
		}
	}
}

func (d createCommandDeps) withDefaults() createCommandDeps {
	if d.stdout == nil {
		d.stdout = io.Discard
	}
	if d.stderr == nil {
		d.stderr = io.Discard
	}
	if d.input == nil {
		input := newTerminalCreateInput(os.Stdin, d.stdout)
		d.input = input
	}
	if d.stageRunnerFactory == nil {
		d.stageRunnerFactory = defaultCreateStageRunnerFactory
	}
	if d.planningRunner == nil {
		d.planningRunner = func(ctx context.Context, configPath string, sourceArgs []string, force bool, author, critic string) error {
			return bootstrap.New(d.stdout, d.stderr).Run(ctx, configPath, sourceArgs, force, author, critic)
		}
	}
	return d
}

func defaultCreateStageRunnerFactory(configPath, workspace string, stdout, stderr io.Writer, confirm createpkg.ConfirmFunc) (createStageRunner, error) {
	cfg, err := loadCreateAgentConfig(configPath, workspace)
	if err != nil {
		return nil, err
	}

	launcher := func(agentType, role string, phaseStdout, phaseStderr io.Writer) (agentlaunch.Runner, error) {
		return agentlaunch.LaunchAgent(cfg, agentType, role, phaseStdout, phaseStderr)
	}

	return createpkg.NewStageRunner(workspace, launcher, confirm, stdout), nil
}

func loadCreateAgentConfig(configPath, workspace string) (*config.Config, error) {
	cfg, err := config.Load(configPath)
	if err == nil {
		cfg.Workspace = workspace
		return cfg, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return config.DefaultAgentLaunchConfig(configPath)
}

func createMenuEntries(hasDesign bool) []createMenuEntry {
	entries := []createMenuEntry{
		{Label: "Product Definition", Stage: createpkg.StageProductDefinition},
		{Label: "UX Review", Stage: createpkg.StageUXReview},
		{Label: "Technical Review", Stage: createpkg.StageTechnicalReview},
	}
	if hasDesign {
		entries = append(entries, createMenuEntry{Label: "Planning", Planning: true})
	}
	return append(entries, createMenuEntry{Label: "Stop", Stop: true})
}

func createDesignExists(workspace string) (bool, error) {
	info, err := os.Stat(filepath.Join(workspace, createpkg.DesignFileName()))
	if err == nil {
		return info.Mode().IsRegular(), nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

type terminalCreateInput struct {
	reader *bufio.Reader
	output io.Writer
}

func newTerminalCreateInput(reader io.Reader, output io.Writer) *terminalCreateInput {
	if output == nil {
		output = io.Discard
	}
	return &terminalCreateInput{
		reader: bufio.NewReader(reader),
		output: output,
	}
}

func (i *terminalCreateInput) Choose(ctx context.Context, state createpkg.State, entries []createMenuEntry) (createMenuEntry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return createMenuEntry{}, err
		}

		fmt.Fprintln(i.output)
		fmt.Fprintln(i.output, "vibedrive create")
		if state.LastStage != "" {
			label, err := createStageLabel(state.LastStage)
			if err != nil {
				return createMenuEntry{}, err
			}
			fmt.Fprintf(i.output, "Last completed: %s\n", label)
		}
		for idx, entry := range entries {
			fmt.Fprintf(i.output, "%d. %s\n", idx+1, entry.Label)
		}
		fmt.Fprint(i.output, "Choose: ")

		line, err := i.readLine(ctx)
		if err != nil {
			return createMenuEntry{}, err
		}
		choice, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || choice < 1 || choice > len(entries) {
			fmt.Fprintln(i.output, "Enter one of the listed numbers.")
			continue
		}
		return entries[choice-1], nil
	}
}

func (i *terminalCreateInput) Confirm(ctx context.Context, prompt string) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		fmt.Fprintf(i.output, "%s [y/N]: ", prompt)
		line, err := i.readLine(ctx)
		if err != nil {
			return false, err
		}

		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "n", "no":
			return false, nil
		case "y", "yes":
			return true, nil
		default:
			fmt.Fprintln(i.output, "Enter y or n.")
		}
	}
}

func (i *terminalCreateInput) readLine(ctx context.Context) (string, error) {
	type lineResult struct {
		line string
		err  error
	}
	result := make(chan lineResult, 1)
	go func() {
		line, err := i.reader.ReadString('\n')
		result <- lineResult{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-result:
		if res.err != nil {
			if errors.Is(res.err, io.EOF) && res.line != "" {
				return res.line, nil
			}
			return "", res.err
		}
		return res.line, nil
	}
}

func createStageLabel(stage createpkg.Stage) (string, error) {
	switch stage {
	case createpkg.StageProductDefinition:
		return "Product Definition", nil
	case createpkg.StageUXReview:
		return "UX Review", nil
	case createpkg.StageTechnicalReview:
		return "Technical Review", nil
	default:
		return "", fmt.Errorf("unsupported create stage %q", stage)
	}
}
