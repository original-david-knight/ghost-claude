package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"vibedrive/internal/config"
	"vibedrive/internal/plan"
	"vibedrive/internal/view"
)

func viewCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	fs.SetOutput(stdout)

	configPath := fs.String("config", "vibedrive.yaml", "Path to the workflow config file")
	workspace := fs.String("workspace", "", "Workspace directory containing the workflow config or plan")
	planPath := fs.String("plan", "", "Path to vibedrive-plan.yaml; overrides config plan_file")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if len(fs.Args()) != 0 {
		return fmt.Errorf("view does not accept positional arguments")
	}

	cfg, file, err := loadViewInputs(*configPath, *workspace, *planPath)
	if err != nil {
		return err
	}

	return view.Render(stdout, file, cfg)
}

func loadViewInputs(configPath, workspace, planPath string) (*config.Config, *plan.File, error) {
	resolvedConfigPath, err := resolveConfigPath(configPath, workspace)
	if err != nil {
		return nil, nil, err
	}

	cfg, configErr := config.Load(resolvedConfigPath)
	if configErr != nil && (strings.TrimSpace(planPath) == "" || !os.IsNotExist(configErr)) {
		return nil, nil, configErr
	}

	resolvedPlanPath, err := resolveViewPlanPath(planPath, workspace, cfg)
	if err != nil {
		return nil, nil, err
	}

	file, err := plan.Load(resolvedPlanPath)
	if err != nil {
		return nil, nil, err
	}

	return cfg, file, nil
}

func resolveViewPlanPath(planPath, workspace string, cfg *config.Config) (string, error) {
	if strings.TrimSpace(planPath) == "" {
		if cfg == nil {
			return "", fmt.Errorf("config is required when --plan is not provided")
		}
		return cfg.PlanFile, nil
	}

	baseWorkspace := workspace
	if strings.TrimSpace(baseWorkspace) == "" && cfg != nil {
		baseWorkspace = cfg.Workspace
	}
	return resolveWorkspaceRelativePath(planPath, baseWorkspace)
}
