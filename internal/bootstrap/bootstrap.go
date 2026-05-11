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
	"vibedrive/internal/plan"
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

const (
	defaultPlanFile       = "vibedrive-plan.yaml"
	defaultComponentsFile = "COMPONENTS.md"
)

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

	componentsPath := initComponentsPath(cfg.Workspace)
	if force {
		if err := os.Remove(componentsPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	source, err := resolveSources(cfg.Workspace, sourceArgs, cfg.Path, cfg.PlanFile, componentsPath)
	if err != nil {
		return err
	}

	componentsFeedbackPath := initComponentsCriticFeedbackPath(cfg.Workspace)
	planFeedbackPath := initCriticFeedbackPath(cfg.Workspace)
	if err := os.MkdirAll(filepath.Dir(planFeedbackPath), 0o755); err != nil {
		return err
	}
	for _, path := range []string{componentsFeedbackPath, planFeedbackPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	defer func() {
		_ = os.Remove(componentsFeedbackPath)
		_ = os.Remove(planFeedbackPath)
	}()

	if err := i.runBootstrapPhase(ctx, cfg, author, "components", renderComponentsPrompt(cfg, source, componentsPath)); err != nil {
		return err
	}
	if err := requireNonEmptyFile(componentsPath, "component breakdown phase"); err != nil {
		return err
	}
	if err := i.runBootstrapPhase(ctx, cfg, critic, "component-critic", renderCriticComponentsPrompt(cfg, source, componentsPath, componentsFeedbackPath)); err != nil {
		return err
	}
	if err := requireNonEmptyFile(componentsFeedbackPath, "component critic phase"); err != nil {
		return err
	}
	if err := i.runBootstrapPhase(ctx, cfg, author, "components", renderReviseComponentsPrompt(cfg, source, componentsPath, componentsFeedbackPath)); err != nil {
		return err
	}
	if err := requireNonEmptyFile(componentsPath, "component revision phase"); err != nil {
		return err
	}
	if err := i.runBootstrapPhase(ctx, cfg, author, "author", renderCreatePlanPrompt(cfg, source, componentsPath)); err != nil {
		return err
	}
	if err := requireValidPlan(cfg.PlanFile, "plan author phase"); err != nil {
		return err
	}
	if err := i.runBootstrapPhase(ctx, cfg, critic, "critic", renderCriticPlanPrompt(cfg, source, componentsPath, planFeedbackPath)); err != nil {
		return err
	}
	if err := requireNonEmptyFile(planFeedbackPath, "critic phase"); err != nil {
		return err
	}
	if err := i.runBootstrapPhase(ctx, cfg, author, "author", renderRevisePlanPrompt(cfg, source, componentsPath, planFeedbackPath)); err != nil {
		return err
	}
	if err := requireValidPlan(cfg.PlanFile, "plan revision phase"); err != nil {
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

	source, err := resolveSources(cfg.Workspace, sourceArgs, cfg.Path, cfg.PlanFile, initComponentsPath(cfg.Workspace))
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(i.stdout, renderSourceRefs(cfg.Workspace, source.Files))
	return err
}

func renderComponentsPrompt(cfg *config.Config, source sourceSpec, componentsPath string) string {
	componentsRef := repoRelative(cfg.Workspace, componentsPath)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)

	return strings.TrimSpace(fmt.Sprintf(`
Bootstrap vibedrive component discovery for this repository.

Use these project source inputs as the primary requirements, constraints, design, and success-criteria materials:
%s

Read every listed file completely. Follow any referenced local docs that materially define the project, scope, tests, constraints, checkpoints, or success criteria.

Before a task plan is generated, break the project down into independently workable components and write the result to %s.

Write Markdown with this structure:

# Components

## <stable-kebab-component-id>: <Human-readable component name>
- Owned paths:
  - <repo-relative path or glob owned by this component>
- Public interfaces and contracts:
  - <repo-relative contract/interface doc, package, API, command, or data shape>
- Reads from:
  - <other component id or contract this component consumes>
- Provides:
  - <contract, artifact, command, package, or service this component owns>
- Parallelization notes:
  - <where this component can be worked independently, and what conflicts or dependencies must be respected>

Requirements:
- choose stable kebab-case component ids that can be copied into vibedrive-plan.yaml project.components and task component fields
- make components narrow enough to support parallel development with clear ownership, but not so tiny that every task becomes cross-cutting
- identify owned_paths using repo-relative paths or globs wherever practical
- identify shared contracts, public interfaces, generated artifacts, data files, commands, packages, or service boundaries each component owns or consumes
- call out integration checkpoints and component dependency ordering needed before independent work can proceed safely
- call out shared paths, shared contracts, or unclear ownership that should force conflicts_with or dependency metadata in the generated plan
- include any foundation or contract-first components needed to establish interfaces before cross-component implementation work starts
- do not create vibedrive-plan.yaml in this phase
- keep %s concise but concrete enough that the later plan can populate project.components, task component, owns_paths, reads_contracts, provides_contracts, and conflicts_with metadata from it

After writing %s, quickly re-read it and confirm every component has a stable id, owned paths or explicit ownership notes, and parallelization guidance.
`, sourceRefs, componentsRef, componentsRef, componentsRef))
}

func renderCriticComponentsPrompt(cfg *config.Config, source sourceSpec, componentsPath, feedbackPath string) string {
	componentsRef := repoRelative(cfg.Workspace, componentsPath)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)
	feedbackRef := repoRelative(cfg.Workspace, feedbackPath)

	return strings.TrimSpace(fmt.Sprintf(`
Review the component breakdown in %s against these source inputs:
%s

Also inspect any source docs they reference when checking component coverage and fidelity.

Perform a critical review of %s. You are the critic only: do not change %s, do not create vibedrive-plan.yaml, and do not change any source document.

Focus on:
- missing project components, owned paths, public interfaces, shared contracts, generated artifacts, commands, packages, data boundaries, or service boundaries
- components that are too broad to support independent work or too tiny to avoid constant cross-cutting tasks
- unstable or unclear component ids that would be hard to copy into project.components and task component fields
- unclear edit ownership, shared paths, shared contracts, or integration checkpoints that would weaken safe parallelization
- missing component dependency ordering before independent work can proceed safely
- missing foundation or contract-first components needed before cross-component implementation starts
- missing or weak parallelization notes, conflicts, or dependency guidance
- requirements from the listed source inputs that were omitted or weakened

Write concise, actionable critic feedback to %s. Use Markdown bullets grouped by severity. Include an explicit "No actionable feedback" note if %s already satisfies the source inputs and parallelization goals.
`, componentsRef, sourceRefs, componentsRef, componentsRef, feedbackRef, componentsRef))
}

func renderReviseComponentsPrompt(cfg *config.Config, source sourceSpec, componentsPath, feedbackPath string) string {
	componentsRef := repoRelative(cfg.Workspace, componentsPath)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)
	feedbackRef := repoRelative(cfg.Workspace, feedbackPath)

	return strings.TrimSpace(fmt.Sprintf(`
Revise the component breakdown in %s using the transient critic feedback at %s.

Use these project source inputs as the authority when deciding which critic feedback is actionable:
%s

Read %s, %s, and every listed source input completely before making changes.

Apply actionable critic feedback directly to %s. Ignore feedback that would weaken or contradict the listed source inputs.

When revising:
- keep stable kebab-case component ids unless critic feedback or source requirements show the id is wrong
- preserve or improve repo-relative owned paths, public interfaces, shared contracts, generated artifacts, commands, packages, data boundaries, and service boundaries
- make components narrow enough to support parallel development with clear ownership, but not so tiny that every task becomes cross-cutting
- clarify dependency ordering, integration checkpoints, shared paths, shared contracts, and conflicts that the generated plan must reflect
- preserve or add foundation and contract-first components needed before cross-component implementation starts
- strengthen parallelization notes so the later plan can populate project.components, task component, owns_paths, reads_contracts, provides_contracts, and conflicts_with metadata from %s
- do not create vibedrive-plan.yaml in this phase

After writing %s, quickly re-read it and confirm every component has a stable id, owned paths or explicit ownership notes, and parallelization guidance.
`, componentsRef, feedbackRef, sourceRefs, componentsRef, feedbackRef, componentsRef, componentsRef, componentsRef))
}

func renderCreatePlanPrompt(cfg *config.Config, source sourceSpec, componentsPath string) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	componentsRef := repoRelative(cfg.Workspace, componentsPath)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)
	shapeRules := renderPlanShapeRules(planRef)

	return strings.TrimSpace(fmt.Sprintf(`
Bootstrap vibedrive plan mode for this repository.

Use these project source inputs as the primary requirements, constraints, design, and success-criteria materials:
%s

Also read the component breakdown in %s completely and use it as the authority for component ids, owned paths, public interfaces, shared contracts, integration checkpoints, and parallelization boundaries.

Read every listed file completely. Follow any referenced local docs that materially define the project, scope, tests, constraints, checkpoints, or success criteria.

Create %s as a machine-readable execution plan for the whole project.

%s

Write valid YAML with this structure:

project:
  name: <short project name>
  objective: <one-sentence project objective>
  source_docs:
    - <repo-relative path>
  constraint_files:
    - <repo-relative path>
  components:
    - id: <stable-kebab-component-id>
      name: <human-readable component name>
      owned_paths:
        - <repo-relative path or glob owned by this component>
      reads_contracts:
        - <repo-relative contract/interface doc this component consumes>
      provides_contracts:
        - <repo-relative contract/interface doc this component provides or owns>

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
    component: <component id from project.components>
    owns_paths:
      - <repo-relative path or glob this task is allowed to edit>
    reads_contracts:
      - <repo-relative contract/interface doc this task consumes>
    provides_contracts:
      - <repo-relative contract/interface doc this task creates, updates, or owns>
    conflicts_with:
      - <task id that should not run in parallel with this task>
    acceptance:
      - <acceptance criterion>
    verify_commands:
      - <shell command to run from the repo root>
    commit_message: <clear commit message>

Requirements for the plan:
- source_docs must include %s, every listed source input, and any repo docs referenced from them that are necessary to execute the project correctly
- constraint_files must include the subset of source_docs that define hard requirements, constraints, checkpoints, or success criteria
- preserve every explicit requirement, constraint, checkpoint, success gate, and verification demand from the listed source inputs
- honor the component ids and boundaries from %s when populating project.components and task component fields; if a source input forces a different boundary, preserve the source requirement and make the reason explicit in task details or acceptance criteria
- before generating tasks, first identify the repository components, public interfaces, shared contracts, owned paths, integration checkpoints, and the minimum read context each task should need using %s
- optimize decomposition for context reduction, not merely speed; tasks should be assignable with narrow read context and explicit edit authority
- populate project.components whenever useful component boundaries can be identified, using repo-relative owned_paths and reads_contracts/provides_contracts metadata to document ownership and interfaces
- for each task, declare component, owns_paths, reads_contracts, provides_contracts, and conflicts_with whenever that metadata is known or needed to keep edit authority and integration boundaries explicit
- keep context_files to the smallest source, contract, and local implementation references needed for the task; avoid making agents read broad areas of the repository just to infer ownership or interfaces
- split work by component and contract boundary when practical; any cross-cutting implementation task must depend on a preceding contract or foundation task that establishes the shared interface, ownership model, or integration checkpoint
- decompose the project into executable tasks that are sized for one focused implementation iteration and one coherent commit when practical
- use workflow implement for coding work and workflow checkpoint for explicit full-suite or milestone verification gates
- by default, keep testing, verification, and cleanup work attached to the implementation task that introduces the change instead of deferring it to a later cleanup pass
- design every task so agents can verify their own work without manual help; if existing checks are insufficient, include the needed automated checks, harnesses, fixtures, instrumentation, or artifact capture in the task or an earlier dependency
- for UI, visual, or interactive behavior, include deterministic browser automation, screenshot capture, DOM assertions, accessibility checks, or equivalent evidence wherever those artifacts are needed to verify the work
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
- include preparatory implementation work before any verify_command that depends on newly required verification tooling, such as screenshot instrumentation or seeded test data
- quote any string list item that contains a colon followed by a space so the YAML stays valid

After writing %s, quickly check that the YAML parses and that dependency ordering is coherent.
`, sourceRefs, componentsRef, planRef, shapeRules, componentsRef, componentsRef, componentsRef, planRef))
}

func renderCriticPlanPrompt(cfg *config.Config, source sourceSpec, componentsPath, feedbackPath string) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	componentsRef := repoRelative(cfg.Workspace, componentsPath)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)
	feedbackRef := repoRelative(cfg.Workspace, feedbackPath)

	return strings.TrimSpace(fmt.Sprintf(`
Review the generated execution plan in %s against these source inputs:
%s

Also read %s and inspect any source docs they reference when checking plan coverage and fidelity.

Perform a critical review of the plan. You are the critic only: do not change %s, and do not change any source document.

Focus on:
- missing or malformed required top-level YAML sibling sections, especially a missing top-level tasks sequence
- task lists incorrectly nested under project, components, milestones, phases, plan, or any other wrapper instead of top-level tasks
- missing constraints or success criteria
- missing, stale, or contradicted use of the component breakdown in %s
- missing component, ownership, contract, or integration-boundary analysis before task generation
- incorrect or weak task decomposition
- tasks with excessive context requirements, unclear read scope, or context_files that are broad because interfaces or ownership were not declared
- missing interfaces, shared contracts, component metadata, owns_paths, reads_contracts, provides_contracts, or conflicts_with entries needed to make edit authority explicit
- ambiguous ownership or unsafe parallel assumptions, including tasks that touch shared paths/contracts without dependency or conflict metadata
- reject tasks that are cross-cutting without a preceding contract or foundation task that establishes the shared interface, ownership model, or integration checkpoint
- missing checkpoints or verification work
- missing or weak automated verification commands
- tasks that lack a self-verification path agents can run without manual help
- missing preparatory harnesses, fixtures, instrumentation, screenshot capture, or artifact generation needed to make verification executable
- tasks that defer routine testing, verification, or cleanup work that should stay attached to implementation
- missing trigger-justified standalone tech-debt tasks for work expected to introduce a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface
- wording that claims plan-time knowledge of actual changed-file counts or other finalize-time facts instead of framing them as expected breadth or discovered risk
- standalone tech-debt tasks that lack an explicit trigger, duplicate routine inline work, or still assume a fixed cadence instead of a risk-based follow-up
- tasks that do not end by capturing phase learnings for future replanning and fresh reruns
- bad dependency ordering
- tasks that are too large, too vague, or not committable
- requirements from the listed source inputs that were omitted or weakened

Write concise, actionable critic feedback to %s. Use Markdown bullets grouped by severity. Include an explicit "No actionable feedback" note if the plan already satisfies the source inputs.
`, planRef, sourceRefs, componentsRef, planRef, componentsRef, feedbackRef))
}

func renderRevisePlanPrompt(cfg *config.Config, source sourceSpec, componentsPath, feedbackPath string) string {
	planRef := repoRelative(cfg.Workspace, cfg.PlanFile)
	componentsRef := repoRelative(cfg.Workspace, componentsPath)
	sourceRefs := renderSourceRefs(cfg.Workspace, source.Files)
	feedbackRef := repoRelative(cfg.Workspace, feedbackPath)
	shapeRules := renderPlanShapeRules(planRef)

	return strings.TrimSpace(fmt.Sprintf(`
Revise the generated execution plan in %s using the transient critic feedback at %s.

Use these project source inputs as the authority when deciding which critic feedback is actionable:
%s

Read %s, %s, %s, and every listed source input completely before making changes.

Apply actionable critic feedback directly to %s. Ignore feedback that would weaken or contradict the listed source inputs.

Keep the YAML valid. Keep task statuses at todo. Do not weaken or remove constraints from the source requirements.

%s

When revising:
- preserve every explicit requirement, constraint, checkpoint, success gate, and verification demand from the listed source inputs
- preserve %s in source_docs and honor its component ids and boundaries when populating project.components and task component fields; if a source input forces a different boundary, preserve the source requirement and make the reason explicit
- ensure the plan uses %s to analyze components, public interfaces, shared contracts, owned paths, integration checkpoints, and the minimum read context each task should need before finalizing task generation
- optimize revised tasks for context reduction, not merely speed; tasks should be assignable with narrow read context and explicit edit authority
- populate or correct project.components, task component, owns_paths, reads_contracts, provides_contracts, and conflicts_with metadata wherever that information is known or needed for safe bounded work
- keep context_files narrow and contract-oriented so agents do not need broad repository context to infer interfaces or ownership
- split work by component and contract boundary when practical; reject or rewrite any cross-cutting implementation task that lacks a preceding contract or foundation task establishing the shared interface, ownership model, or integration checkpoint
- keep testing, verification, and cleanup work attached to the implementation task that introduces the change by default
- ensure every task has a self-verification path agents can run without manual help; add automated checks, harnesses, fixtures, instrumentation, or artifact capture where existing checks are insufficient
- include deterministic screenshot capture, browser automation, DOM assertions, accessibility checks, or equivalent evidence for UI, visual, or interactive behavior when needed
- add standalone implement tech-debt tasks only when expected new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or broad expected implementation surface justifies dedicated follow-up
- describe standalone tech-debt triggers as planning-time heuristics about expected breadth and discovered risk, not as actual changed-file counts or other finalize-time facts before execution
- do not add standalone tech-debt tasks on a fixed schedule or as generic placeholders
- make every standalone tech-debt trigger explicit in the task details or acceptance criteria
- keep all generated tasks at status todo
- keep every task's last acceptance item focused on short phase notes about what was learned, including discoveries or plan adjustments that matter if the project is re-planned and rerun in a fresh environment
- keep verify_commands wherever there is a concrete automated check or test command
- keep preparatory implementation work before any verify_command that depends on newly required verification tooling, such as screenshot instrumentation or seeded test data
- quote any string list item that contains a colon followed by a space so the YAML stays valid

After writing %s, quickly check that the YAML parses and that dependency ordering is coherent.
`, planRef, feedbackRef, sourceRefs, planRef, feedbackRef, componentsRef, planRef, shapeRules, componentsRef, componentsRef, planRef))
}

func renderPlanShapeRules(planRef string) string {
	return fmt.Sprintf(`Plan shape rules:
- %s must be YAML only; do not wrap it in Markdown fences or add explanatory prose.
- The required top-level sibling sections are project: and tasks:, both at indentation zero.
- tasks: must be a top-level YAML sequence with at least one task item. Never put tasks under project, components, milestones, phases, plan, task_groups, or any other wrapper.
- project.components is the component catalog; it is not the task list.
- Every executable item must be a top-level tasks entry with id, title, status: todo, workflow, acceptance, and any known dependency or boundary metadata.`, planRef)
}

func initComponentsPath(workspace string) string {
	return filepath.Join(workspace, defaultComponentsFile)
}

func initComponentsCriticFeedbackPath(workspace string) string {
	return filepath.Join(workspace, ".vibedrive", "init-components-feedback.md")
}

func initCriticFeedbackPath(workspace string) string {
	return filepath.Join(workspace, ".vibedrive", "init-critic-feedback.md")
}

func requireNonEmptyFile(path, phase string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s did not write required artifact %s", phase, path)
		}
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s wrote empty required artifact %s", phase, path)
	}
	return nil
}

func requireValidPlan(path, phase string) error {
	if err := requireNonEmptyFile(path, phase); err != nil {
		return err
	}
	if _, err := plan.Load(path); err != nil {
		return fmt.Errorf("%s wrote invalid plan %s: %w", phase, path, err)
	}
	return nil
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
