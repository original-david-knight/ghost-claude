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

	"vibedrive/internal/automation"
	"vibedrive/internal/bootstrap"
	"vibedrive/internal/config"
	"vibedrive/internal/runner"
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
	case "restart":
		return restartCommand(ctx, args[1:])
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

	configPath := fs.String("config", "vibedrive.yaml", "Path to the workflow config file")
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
	resolvedCoder, err := config.ResolveAgent(coder, config.AgentCodex, "coder")
	if err != nil {
		return err
	}
	resolvedReviewer, err := config.ResolveAgent(reviewer, config.AgentClaude, "reviewer")
	if err != nil {
		return err
	}

	cfg.Coder = resolvedCoder
	cfg.Reviewer = resolvedReviewer

	return cfg.Validate()
}

func initCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprint(out, `Usage:
  vibedrive init [-config vibedrive.yaml] [-workspace /path/to/repo] [--source PATH ...] [--author claude|codex] [--print-sources] [-force] [SOURCE]

Init source selection:
  Repeat --source to add files or directories. A single positional SOURCE is accepted as one extra source.
  Omit both --source and SOURCE to fall back to the workspace's top-level regular files.
  --print-sources resolves, dedupes, sorts, and prints that source set without writing config or starting the author.

Plan authoring:
  --author selects the bootstrap author and defaults to codex. Choosing it does not change runtime --coder/--reviewer defaults.
  The generated plan keeps routine testing and cleanup inline by default and only adds standalone tech-debt follow-up when risk triggers justify it.

Flags:
`)
		fs.PrintDefaults()
	}

	configPath := fs.String("config", "vibedrive.yaml", "Path to write the workflow config file")
	workspace := fs.String("workspace", "", "Workspace directory where the workflow config should be created")
	var sources stringListFlag
	fs.Var(&sources, "source", "Source file or directory to use when generating the initial plan (repeatable)")
	author := fs.String("author", config.AgentCodex, "Bootstrap author to use during init: claude or codex")
	force := fs.Bool("force", false, "Overwrite existing files")
	printSources := fs.Bool("print-sources", false, "Resolve init sources, print them, and exit without writing config")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	resolvedAuthor, err := resolveInitAuthor(*author)
	if err != nil {
		return err
	}

	sourceArgs, err := resolveInitSourceArgs(sources, fs.Args())
	if err != nil {
		return err
	}

	absConfig, err := resolveConfigPath(*configPath, *workspace)
	if err != nil {
		return err
	}

	init := bootstrap.New(os.Stdout, os.Stderr)
	if *printSources {
		return init.PrintSources(absConfig, sourceArgs)
	}

	return init.Run(ctx, absConfig, sourceArgs, *force, resolvedAuthor)
}

func resolveInitAuthor(value string) (string, error) {
	return config.ResolveAgent(value, config.AgentCodex, "author")
}

func printUsage() {
	fmt.Println(`vibedrive

Usage:
  vibedrive run [-config vibedrive.yaml] [-workspace /path/to/repo] [-dry-run] [-coder claude|codex] [-reviewer claude|codex]
  vibedrive init [-config vibedrive.yaml] [-workspace /path/to/repo] [--source PATH ...] [--author claude|codex] [--print-sources] [-force] [SOURCE]
  vibedrive restart [-config vibedrive.yaml] [-workspace /path/to/repo]
  vibedrive task finalize --workspace DIR --plan PATH --task TASK_ID --result PATH [--message MSG]

If no subcommand is provided, vibedrive behaves like "run".

Init notes:
  Repeat --source to add files or directories. A single positional SOURCE adds one extra source.
  Omit sources to scan the workspace's top-level regular files. Use --print-sources to preview that resolved source set.
  Use --author claude|codex to pick the bootstrap author. This does not change runtime --coder/--reviewer defaults.
  Bootstrap planning keeps routine testing and cleanup inline by default and only adds standalone tech-debt follow-up when risk triggers justify it.`)
}

func restartCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)

	configPath := fs.String("config", "vibedrive.yaml", "Path to the workflow config file")
	workspace := fs.String("workspace", "", "Workspace directory containing the workflow config")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if len(fs.Args()) != 0 {
		return fmt.Errorf("restart does not accept positional arguments")
	}

	resolvedConfigPath, err := resolveConfigPath(*configPath, *workspace)
	if err != nil {
		return err
	}

	return bootstrap.New(os.Stdout, os.Stderr).Restart(ctx, resolvedConfigPath)
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
	planPath := fs.String("plan", "", "Path to vibedrive-plan.yaml")
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

type stringListFlag []string

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func resolveInitSourceArgs(flagValues []string, args []string) ([]string, error) {
	sources := make([]string, 0, len(flagValues)+len(args))

	for _, value := range flagValues {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, fmt.Errorf("init source must not be empty")
		}
		sources = append(sources, trimmed)
	}

	switch len(args) {
	case 0:
	case 1:
		trimmed := strings.TrimSpace(args[0])
		if trimmed == "" {
			return nil, fmt.Errorf("init source must not be empty")
		}
		sources = append(sources, trimmed)
	default:
		return nil, fmt.Errorf("init accepts at most one positional source argument")
	}

	return sources, nil
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
