package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"ghost_claude/internal/automation"
	"ghost_claude/internal/bootstrap"
	"ghost_claude/internal/config"
	"ghost_claude/internal/runner"
)

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	if len(args) == 0 {
		return runCommand(ctx, []string{})
	}

	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:])
	case "init":
		return initCommand(ctx, args[1:])
	case "task":
		return taskCommand(ctx, args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return runCommand(ctx, args)
	}
}

func runCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)

	configPath := fs.String("config", "ghost-claude.yaml", "Path to the workflow config file")
	workspace := fs.String("workspace", "", "Workspace directory containing the workflow config")
	dryRun := fs.Bool("dry-run", false, "Render prompts and commands without executing them")
	coder := fs.String("coder", "", "Coder agent to use at runtime: claude or codex (default: codex)")
	reviewer := fs.String("reviewer", "", "Peer reviewer agent to use at runtime: claude or codex (default: claude)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	resolvedConfigPath, err := resolveConfigPath(*configPath, *workspace)
	if err != nil {
		return err
	}

	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		return err
	}
	cfg.DryRun = cfg.DryRun || *dryRun
	if err := applyRuntimeAgentRoles(cfg, *coder, *reviewer); err != nil {
		return err
	}

	app, err := runner.New(cfg, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	return app.Run(ctx)
}

func applyRuntimeAgentRoles(cfg *config.Config, coder, reviewer string) error {
	cfg.Coder = config.AgentCodex
	cfg.Reviewer = config.AgentClaude

	if value := strings.TrimSpace(coder); value != "" {
		cfg.Coder = value
	}
	if value := strings.TrimSpace(reviewer); value != "" {
		cfg.Reviewer = value
	}

	return cfg.Validate()
}

func initCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)

	configPath := fs.String("config", "ghost-claude.yaml", "Path to write the workflow config file")
	workspace := fs.String("workspace", "", "Workspace directory where the workflow config should be created")
	source := fs.String("source", "", "Source file or directory to use when generating the initial plan")
	force := fs.Bool("force", false, "Overwrite existing files")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	sourceArg, err := resolveInitSourceArg(*source, fs.Args())
	if err != nil {
		return err
	}

	absConfig, err := resolveConfigPath(*configPath, *workspace)
	if err != nil {
		return err
	}

	return bootstrap.New(os.Stdout, os.Stderr).Run(ctx, absConfig, sourceArg, *force)
}

func printUsage() {
	fmt.Println(`ghost-claude

Usage:
  ghost-claude run [-config ghost-claude.yaml] [-workspace /path/to/repo] [-dry-run] [-coder claude|codex] [-reviewer claude|codex]
  ghost-claude init [-config ghost-claude.yaml] [-workspace /path/to/repo] [-source PATH] [-force] [SOURCE]
  ghost-claude task finalize --workspace DIR --plan PATH --task TASK_ID --result PATH [--message MSG]

If no subcommand is provided, ghost-claude behaves like "run".`)
}

func taskCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("task requires a subcommand")
	}

	switch args[0] {
	case "finalize":
		return finalizeTaskCommand(ctx, args[1:])
	default:
		return fmt.Errorf("unsupported task subcommand %q", args[0])
	}
}

func finalizeTaskCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task finalize", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)

	workspace := fs.String("workspace", "", "Workspace directory")
	planPath := fs.String("plan", "", "Path to ghost-plan.yaml")
	taskID := fs.String("task", "", "Task ID to update")
	resultPath := fs.String("result", "", "Path to the task result JSON file")
	message := fs.String("message", "", "Commit message to use when changes are present")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if strings.TrimSpace(*workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	if strings.TrimSpace(*planPath) == "" {
		return fmt.Errorf("--plan is required")
	}
	if strings.TrimSpace(*taskID) == "" {
		return fmt.Errorf("--task is required")
	}
	if strings.TrimSpace(*resultPath) == "" {
		return fmt.Errorf("--result is required")
	}

	return automation.Finalize(ctx, automation.FinalizeOptions{
		Workspace:     *workspace,
		PlanFile:      *planPath,
		TaskID:        *taskID,
		ResultPath:    *resultPath,
		CommitMessage: *message,
	}, os.Stdout, os.Stderr)
}

func resolveInitSourceArg(flagValue string, args []string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)

	switch {
	case len(args) > 1:
		return "", fmt.Errorf("init accepts at most one source argument")
	case flagValue != "" && len(args) == 1:
		return "", fmt.Errorf("init source cannot be set with both -source and a positional argument")
	case flagValue != "":
		return flagValue, nil
	case len(args) == 1:
		return strings.TrimSpace(args[0]), nil
	default:
		return "", nil
	}
}

func resolveConfigPath(configPath, workspace string) (string, error) {
	if filepath.IsAbs(configPath) {
		return filepath.Clean(configPath), nil
	}

	if workspace == "" {
		return filepath.Abs(configPath)
	}

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}

	return filepath.Join(absWorkspace, configPath), nil
}
