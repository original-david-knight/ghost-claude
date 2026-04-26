package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vibedrive/internal/agentlaunch"
	"vibedrive/internal/config"
	"vibedrive/internal/scaffold"
	"vibedrive/pkg/agentcli/claude"
)

type Initializer struct {
	stdout io.Writer
	stderr io.Writer

	launchAgent func(*config.Config, string, string, io.Writer, io.Writer) (agentlaunch.Runner, error)
	newClient   func(*config.Config, io.Writer, io.Writer) (promptClient, error)
	newSession  func(string) (*claude.Session, error)
}

type promptClient interface {
	RunPrompt(ctx context.Context, session *claude.Session, prompt string) error
	Close(session *claude.Session) error
}

type sourceSpec struct {
	Files []string
}

const defaultPlanFile = "vibedrive-plan.yaml"

func New(stdout, stderr io.Writer) *Initializer {
	return &Initializer{
		stdout:      stdout,
		stderr:      stderr,
		launchAgent: agentlaunch.LaunchAgent,
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

func (i *Initializer) Run(ctx context.Context, configPath string, sourceArgs []string, force bool, author, critic string) error {
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

	source, err := resolveSources(cfg.Workspace, sourceArgs, cfg.Path, cfg.PlanFile)
	if err != nil {
		return err
	}

	feedbackPath := initCriticFeedbackPath(cfg.Workspace)
	if err := os.MkdirAll(filepath.Dir(feedbackPath), 0o755); err != nil {
		return err
	}
	if err := os.Remove(feedbackPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	defer func() {
		_ = os.Remove(feedbackPath)
	}()

	if err := i.runBootstrapPhase(ctx, cfg, author, "author", renderCreatePlanPrompt(cfg, source)); err != nil {
		return err
	}
	if err := i.runBootstrapPhase(ctx, cfg, critic, "critic", renderCriticPlanPrompt(cfg, source, feedbackPath)); err != nil {
		return err
	}
	if _, err := os.Stat(feedbackPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("critic phase did not write feedback artifact %s", feedbackPath)
		}
		return err
	}
	if err := i.runBootstrapPhase(ctx, cfg, author, "author", renderRevisePlanPrompt(cfg, source, feedbackPath)); err != nil {
		return err
	}

	return nil
}

func (i *Initializer) runBootstrapPhase(ctx context.Context, cfg *config.Config, agentType, role, prompt string) error {
	runner, err := i.launchAgent(cfg, agentType, role, i.stdout, i.stderr)
	if err != nil {
		return err
	}

	runErr := runner.RunPrompt(ctx, prompt)
	closeErr := runner.Close()
	if runErr != nil {
		if closeErr != nil {
			return fmt.Errorf("%w; also failed to close %s phase: %v", runErr, role, closeErr)
		}
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}

	return nil
}

func (i *Initializer) PrintSources(configPath string, sourceArgs []string) error {
	cfg, err := resolvePreviewConfig(configPath)
	if err != nil {
		return err
	}

	source, err := resolveSources(cfg.Workspace, sourceArgs, cfg.Path, cfg.PlanFile)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(i.stdout, renderSourceRefs(cfg.Workspace, source.Files))
	return err
}

func renderCreatePlanPrompt(cfg *config.Config, source sourceSpec) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)

	return strings.TrimSpace(fmt.Sprintf(`
Bootstrap vibedrive plan mode for this repository.

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
- by default, keep testing, verification, and cleanup work attached to the implementation task that introduces the change instead of deferring it to a later cleanup pass
- create a standalone implement tech-debt task only when the implementation task is expected to introduce a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface that justifies dedicated follow-up work
- describe those triggers as planning-time heuristics about expected breadth and discovered risk; do not claim the plan can know actual changed-file counts or other finalize-time facts before execution
- when a standalone tech-debt task is justified, make the trigger explicit in the task details or acceptance criteria and scope the task to the follow-up testing, cleanup, hardening, or rollback-safety work that the risk requires
- do not add standalone tech-debt tasks on a fixed schedule or as generic placeholders when the work can stay inside the implementation task
- keep all generated tasks at status todo
- do not silently drop manual, machine-specific, or external-dependency work; represent it in tasks/details so the execution plan remains faithful to the source requirements
- include explicit checkpoint tasks wherever the requirements call for them
- include tasks that keep testing, verification, and cleanup expectations inline with implementation by default instead of deferring them to the end or to scheduled debt-review passes
- for every task, make the last acceptance item instruct the coding agent to leave short notes about what it learned in that phase, including discoveries or plan adjustments that matter if the project is re-planned and rerun in a fresh environment
- include verify_commands for each task whenever there is a concrete automated check or test command that should run before the task can be considered done
- quote any string list item that contains a colon followed by a space so the YAML stays valid

After writing %s, quickly check that the YAML parses and that dependency ordering is coherent.
`, sourceRefs, planRef, planRef))
}

func renderCriticPlanPrompt(cfg *config.Config, source sourceSpec, feedbackPath string) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)
	feedbackRef := repoRelative(cfg.Workspace, feedbackPath)

	return strings.TrimSpace(fmt.Sprintf(`
Review the generated execution plan in %s against these source inputs:
%s

Also inspect any source docs they reference when checking plan coverage and fidelity.

Perform a critical review of the plan. You are the critic only: do not change %s, and do not change any source document.

Focus on:
- missing constraints or success criteria
- incorrect or weak task decomposition
- missing checkpoints or verification work
- missing or weak automated verification commands
- tasks that defer routine testing, verification, or cleanup work that should stay attached to implementation
- missing trigger-justified standalone tech-debt tasks for work expected to introduce a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface
- wording that claims plan-time knowledge of actual changed-file counts or other finalize-time facts instead of framing them as expected breadth or discovered risk
- standalone tech-debt tasks that lack an explicit trigger, duplicate routine inline work, or still assume a fixed cadence instead of a risk-based follow-up
- tasks that do not end by capturing phase learnings for future replanning and fresh reruns
- bad dependency ordering
- tasks that are too large, too vague, or not committable
- requirements from the listed source inputs that were omitted or weakened

Write concise, actionable critic feedback to %s. Use Markdown bullets grouped by severity. Include an explicit "No actionable feedback" note if the plan already satisfies the source inputs.
`, planRef, sourceRefs, planRef, feedbackRef))
}

func renderRevisePlanPrompt(cfg *config.Config, source sourceSpec, feedbackPath string) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)
	feedbackRef := repoRelative(cfg.Workspace, feedbackPath)

	return strings.TrimSpace(fmt.Sprintf(`
Revise the generated execution plan in %s using the transient critic feedback at %s.

Use these project source inputs as the authority when deciding which critic feedback is actionable:
%s

Read %s, %s, and every listed source input completely before making changes.

Apply actionable critic feedback directly to %s. Ignore feedback that would weaken or contradict the listed source inputs.

Keep the YAML valid. Keep task statuses at todo. Do not weaken or remove constraints from the source requirements.

When revising:
- preserve every explicit requirement, constraint, checkpoint, success gate, and verification demand from the listed source inputs
- keep testing, verification, and cleanup work attached to the implementation task that introduces the change by default
- add standalone implement tech-debt tasks only when expected new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or broad expected implementation surface justifies dedicated follow-up
- describe standalone tech-debt triggers as planning-time heuristics about expected breadth and discovered risk, not as actual changed-file counts or other finalize-time facts before execution
- do not add standalone tech-debt tasks on a fixed schedule or as generic placeholders
- make every standalone tech-debt trigger explicit in the task details or acceptance criteria
- keep all generated tasks at status todo
- keep every task's last acceptance item focused on short phase notes about what was learned, including discoveries or plan adjustments that matter if the project is re-planned and rerun in a fresh environment
- keep verify_commands wherever there is a concrete automated check or test command
- quote any string list item that contains a colon followed by a space so the YAML stays valid

After writing %s, quickly check that the YAML parses and that dependency ordering is coherent.
`, planRef, feedbackRef, sourceRefs, planRef, feedbackRef, planRef, planRef))
}

func initCriticFeedbackPath(workspace string) string {
	return filepath.Join(workspace, ".vibedrive", "init-critic-feedback.md")
}

func resolvePreviewConfig(configPath string) (*config.Config, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}

	workspace := filepath.Dir(absConfig)
	return &config.Config{
		Path:      absConfig,
		Workspace: workspace,
		PlanFile:  filepath.Join(workspace, defaultPlanFile),
	}, nil
}

func resolveSources(workspace string, sourceArgs []string, excludedPaths ...string) (sourceSpec, error) {
	excluded := make(map[string]struct{}, len(excludedPaths))
	for _, path := range excludedPaths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		excluded[filepath.Clean(path)] = struct{}{}
	}

	if len(sourceArgs) == 0 {
		sourceArgs = []string{workspace}
	}

	files := make([]string, 0)
	seen := make(map[string]struct{})
	for _, sourceArg := range sourceArgs {
		if err := appendResolvedSources(workspace, sourceArg, excluded, seen, &files); err != nil {
			return sourceSpec{}, err
		}
	}

	sort.Strings(files)
	return sourceSpec{Files: files}, nil
}

func appendResolvedSources(workspace, sourceArg string, excluded map[string]struct{}, seen map[string]struct{}, files *[]string) error {
	target := strings.TrimSpace(sourceArg)
	if target == "" {
		return fmt.Errorf("init source must not be empty")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(workspace, target)
	}
	target = filepath.Clean(target)

	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("resolve init source %q: %w", target, err)
	}
	if info.Mode().IsRegular() {
		if _, ok := seen[target]; ok {
			return nil
		}
		seen[target] = struct{}{}
		*files = append(*files, target)
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("init source %q must be a regular file or directory", target)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}

	foundRegular := false
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}

		path := filepath.Join(target, entry.Name())
		if _, skip := excluded[filepath.Clean(path)]; skip {
			continue
		}
		foundRegular = true
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		*files = append(*files, path)
	}

	if !foundRegular {
		return fmt.Errorf("init source directory %q does not contain any usable regular files", target)
	}

	return nil
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
