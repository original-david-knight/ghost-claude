package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ghost_claude/internal/claude"
	"ghost_claude/internal/config"
	"ghost_claude/internal/scaffold"
)

type Initializer struct {
	stdout io.Writer
	stderr io.Writer

	newClient  func(*config.Config, io.Writer, io.Writer) (promptClient, error)
	newSession func(string) (*claude.Session, error)
}

type promptClient interface {
	RunPrompt(ctx context.Context, session *claude.Session, prompt string) error
	Close(session *claude.Session) error
}

type sourceSpec struct {
	Files []string
}

func New(stdout, stderr io.Writer) *Initializer {
	return &Initializer{
		stdout: stdout,
		stderr: stderr,
		newClient: func(cfg *config.Config, stdout, stderr io.Writer) (promptClient, error) {
			return claude.New(
				cfg.Claude.Command,
				cfg.Claude.Args,
				cfg.Workspace,
				cfg.Claude.Transport,
				cfg.Claude.StartupTimeout,
				stdout,
				stderr,
			)
		},
		newSession: claude.NewSession,
	}
}

func (i *Initializer) Run(ctx context.Context, configPath, sourceArg string, force bool) error {
	if err := scaffold.Write(configPath, force); err != nil {
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	if strings.TrimSpace(cfg.PlanFile) == "" {
		return fmt.Errorf("plan_file must be set in %s for init bootstrap", configPath)
	}

	if !force {
		if _, err := os.Stat(cfg.PlanFile); err == nil {
			fmt.Fprintf(i.stdout, "Skipped %s (already exists)\n", cfg.PlanFile)
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	} else {
		if err := os.Remove(cfg.PlanFile); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	source, err := resolveSource(cfg.Workspace, sourceArg, cfg.Path, cfg.PlanFile)
	if err != nil {
		return err
	}

	client, err := i.newClient(cfg, i.stdout, i.stderr)
	if err != nil {
		return err
	}

	session, err := i.newSession(config.SessionStrategySessionID)
	if err != nil {
		return err
	}

	defer func() {
		_ = client.Close(session)
	}()

	prompts := []string{
		renderCreatePlanPrompt(cfg, source),
		renderReviewPlanPrompt(cfg, source),
	}

	for _, prompt := range prompts {
		if err := client.RunPrompt(ctx, session, prompt); err != nil {
			return err
		}
	}

	return nil
}

func renderCreatePlanPrompt(cfg *config.Config, source sourceSpec) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)

	return strings.TrimSpace(fmt.Sprintf(`
Bootstrap ghost-claude plan mode for this repository.

Use these project source inputs as the primary requirements, constraints, design, and success-criteria materials:
%s

Read every listed file completely. Follow any referenced local docs that materially define the project, scope, tests, constraints, checkpoints, or success criteria.

Create %s as a machine-readable execution plan for the whole project.

Write valid YAML with this structure:

project:
  name: <short project name>
  objective: <one-sentence project objective>
  source_docs:
    - <repo-relative path>
  constraint_files:
    - <repo-relative path>

tasks:
  - id: <stable-kebab-id>
    title: <short task title>
    details: <implementation notes>
    workflow: <implement or checkpoint>
    status: todo
    deps:
      - <task id>
    context_files:
      - <repo-relative path>
    acceptance:
      - <acceptance criterion>
    verify_commands:
      - <shell command to run from the repo root>
    commit_message: <clear commit message>

Requirements for the plan:
- source_docs must include every listed source input and any repo docs referenced from them that are necessary to execute the project correctly
- constraint_files must include the subset of source_docs that define hard requirements, constraints, checkpoints, or success criteria
- preserve every explicit requirement, constraint, checkpoint, success gate, and verification demand from the listed source inputs
- decompose the project into executable tasks that are sized for one focused implementation iteration and one coherent commit when practical
- use workflow implement for coding work and workflow checkpoint for explicit full-suite or milestone verification gates
- keep all generated tasks at status todo
- do not silently drop manual, machine-specific, or external-dependency work; represent it in tasks/details so the execution plan remains faithful to the source requirements
- include explicit checkpoint tasks wherever the requirements call for them
- include tasks that keep testing and verification work attached to implementation instead of deferring all tests to the end
- include verify_commands for each task whenever there is a concrete automated check or test command that should run before the task can be considered done
- quote any string list item that contains a colon followed by a space so the YAML stays valid

After writing %s, quickly check that the YAML parses and that dependency ordering is coherent.
`, sourceRefs, planRef, planRef))
}

func renderReviewPlanPrompt(cfg *config.Config, source sourceSpec) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)

	return strings.TrimSpace(fmt.Sprintf(`
Review the generated execution plan in %s against these source inputs:
%s

Also inspect any source docs they reference when checking plan coverage and fidelity.

Perform a critical review of the plan. Focus on:
- missing constraints or success criteria
- incorrect or weak task decomposition
- missing checkpoints or verification work
- missing or weak automated verification commands
- bad dependency ordering
- tasks that are too large, too vague, or not committable
- requirements from the listed source inputs that were omitted or weakened

Incorporate any actionable review feedback directly into %s.

Keep the YAML valid. Keep task statuses at todo. Do not weaken or remove constraints from the source requirements.
`, planRef, sourceRefs, planRef))
}

func resolveSource(workspace, sourceArg string, excludedPaths ...string) (sourceSpec, error) {
	target := strings.TrimSpace(sourceArg)
	if target == "" {
		target = workspace
	} else if !filepath.IsAbs(target) {
		target = filepath.Join(workspace, target)
	}
	target = filepath.Clean(target)

	info, err := os.Stat(target)
	if err != nil {
		return sourceSpec{}, fmt.Errorf("resolve init source %q: %w", target, err)
	}
	if info.Mode().IsRegular() {
		return sourceSpec{Files: []string{target}}, nil
	}
	if !info.IsDir() {
		return sourceSpec{}, fmt.Errorf("init source %q must be a regular file or directory", target)
	}

	excluded := make(map[string]struct{}, len(excludedPaths))
	for _, path := range excludedPaths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		excluded[filepath.Clean(path)] = struct{}{}
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		return sourceSpec{}, err
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return sourceSpec{}, err
		}
		if !info.Mode().IsRegular() {
			continue
		}

		path := filepath.Join(target, entry.Name())
		if _, skip := excluded[filepath.Clean(path)]; skip {
			continue
		}
		files = append(files, path)
	}

	sort.Strings(files)
	if len(files) == 0 {
		return sourceSpec{}, fmt.Errorf("init source directory %q does not contain any regular files", target)
	}

	return sourceSpec{Files: files}, nil
}

func renderSourceRefs(workspace string, files []string) string {
	refs := make([]string, 0, len(files))
	for _, path := range files {
		refs = append(refs, "- "+repoRelative(workspace, path))
	}
	return strings.Join(refs, "\n")
}

func repoRelative(workspace, path string) string {
	rel, err := filepath.Rel(workspace, path)
	if err != nil {
		return path
	}
	if rel == "." {
		return path
	}
	return filepath.ToSlash(rel)
}
