# Vibedrive Feature Specification

Vibedrive is a command-line app for driving AI coding agents through a machine-readable project plan until the planned work is complete. It supports two broad modes:

- design and planning, where agents help create or refine `DESIGN.md`, discover project components, and generate `vibedrive-plan.yaml`
- execution, where agents implement tasks, review each other, verify work, update plan state, commit progress, and continue to the next task

This document describes product behavior and user-facing contracts only.

## Core Product Concepts

### Workspace

- Every command operates against a workspace directory.
- By default, the current directory is the workspace.
- `--workspace DIR` lets the user run commands against another project without changing directories.
- When `--workspace` is provided, default workspace files such as `vibedrive.yaml`, `vibedrive-plan.yaml`, `DESIGN.md`, and `.vibedrive/*` are resolved under that directory.
- Relative paths in the configuration are resolved from the workspace unless a field defines a different base.

### Agents

- Vibedrive supports Claude and Codex as selectable agents.
- Agents can fill different roles depending on the command:
  - `author`: writes or revises product and planning documents
  - `critic`: reviews product and planning documents without owning edits
  - `coder`: implements plan tasks during execution
  - `reviewer`: reviews task changes during execution
- Defaults are:
  - create/init author: Codex
  - create/init critic: Claude
  - runtime coder: Codex
  - runtime reviewer: Claude
- Any role can be set to either `claude` or `codex`.
- The same agent type may be used for multiple roles in the same workflow.
- Runtime coder/reviewer selection is controlled by `vibedrive run --coder` and `--reviewer`; these choices do not rewrite the plan.
- Bootstrap author/critic selection is controlled by `vibedrive create --author`, `--critic`, `vibedrive init --author`, and `--critic`; these choices do not change runtime coder/reviewer defaults.

### Human-Watchable Agent Sessions

- Vibedrive can run Claude and Codex in their real fullscreen terminal interfaces.
- The user can watch agent work happen in live terminal sessions.
- A task normally reuses one agent session per agent type across that task's workflow steps.
- A workflow step can request a fresh agent session for isolation.
- Interactive create stages keep the author agent open until the user exits the agent interface after `DESIGN.md` has been updated.
- The app should detect when a fullscreen agent is ready for another prompt, when it is working, and when startup fails.
- The app should handle initial trust prompts so an unattended run can start in a fresh workspace.

### Non-Interactive Agent Sessions

- Claude can be configured to run one prompt at a time in print mode.
- Codex can be configured to run one prompt at a time in exec mode.
- Non-interactive modes receive the full rendered prompt on standard input.
- Non-interactive modes stream agent output to the user's terminal.
- Non-interactive modes do not provide a reusable agent session or an interactive design interview.
- Interactive create stages must reject non-interactive transports and explain that a fullscreen agent interface is required.

### Machine-Owned State

- `vibedrive-plan.yaml` is the execution queue and primary task status source.
- `.vibedrive/task-notes.yaml` stores short notes learned during task execution.
- Plan files should not accumulate task notes after save; notes belong in `.vibedrive/task-notes.yaml`.
- Task result and review artifacts are transient and removed by finalization.
- Runtime active-step state is transient and should be cleared when a run finishes.

## Command Surface

### Default Command

- Running `vibedrive` with no subcommand behaves like `vibedrive run`.
- Unknown subcommands are treated as arguments to `run`, preserving compatibility with direct run flags.
- `vibedrive help`, `vibedrive --help`, and `vibedrive -h` print top-level usage.

### `vibedrive start`

- `start` is an alias for `create`.
- It launches the config-free `DESIGN.md` authoring flow.
- It does not require `vibedrive.yaml`.
- Stopping from the create menu must not create `vibedrive.yaml`.

### `vibedrive create`

Usage:

```text
vibedrive create [-workspace DIR] [--author claude|codex] [--critic claude|codex]
```

Features:

- Opens a numbered interactive menu for authoring or improving `DESIGN.md`.
- Menu entries:
  - Product Definition
  - Feature/Refactor
  - UX Review
  - Technical Review
  - Planning, only when `DESIGN.md` exists at the workspace root
  - Stop
- Shows the last completed create stage when create state exists.
- Stores create state in `.vibedrive/create-state.json`.
- Create state stores only the last completed stage.
- Stop exits without modifying `DESIGN.md` or create state.
- Cancellation while the menu is waiting preserves existing `DESIGN.md` and create state.
- Cancellation during a stage preserves existing `DESIGN.md` and create state unless the agent itself already changed files.
- The command rejects positional idea text. Users explain ideas interactively to the author agent.
- The command intentionally does not support `--dry-run` or `--resume`.
- Invalid author or critic values are rejected unless they are `claude` or `codex`.

Product Definition stage:

- The author inspects the workspace before interviewing the user.
- If a root `DESIGN.md` exists, the author reads it before editing.
- The author interviews the user in the agent interface.
- The author asks questions one at a time until requirements are ready for later stages.
- The author challenges unclear users, product scope, contradictions, success criteria, and over-broad ideas.
- The author writes or updates root `DESIGN.md`.
- The design must include requirements that coding agents can verify without manual help.
- The design should name automated checks, fixtures, harnesses, instrumentation, or captured artifacts needed for verification.

Feature/Refactor stage:

- The author inspects the existing project and reads existing `DESIGN.md` before editing.
- The stage is for adding a feature or refactoring existing behavior.
- The author interviews the user about the requested change, current behavior to preserve, affected users and workflows, target outcome, non-goals, compatibility constraints, rollout or migration needs, and refactor boundaries.
- The author grounds the design in the current codebase by identifying relevant files, architecture, data flows, integration points, tests, fixtures, and risky coupling.
- The author challenges broad, underspecified, incompatible, or unverifiable requests.
- The author writes or updates root `DESIGN.md`.

UX Review stage:

- The author inspects the workspace and reads existing `DESIGN.md`.
- The author improves product and UX coverage in `DESIGN.md`.
- The stage may cover user journeys, workflows, interaction design, visual style, layout, responsive behavior, accessibility, empty/loading/error states, content, terminology, and scope tradeoffs.
- UI, visual, or interactive work should include deterministic verification evidence such as scripted screenshots, DOM assertions, accessibility checks, or equivalent artifacts.

Technical Review stage:

- The author inspects the workspace and reads existing `DESIGN.md`.
- The author improves implementation guidance in `DESIGN.md`.
- The stage may cover architecture, data model, API contracts, integration points, risks, edge cases, test strategy, verification instrumentation, rollout and migration notes, known unknowns, and a rough implementation approach.
- This stage must not create a detailed task-by-task plan.
- Detailed planning remains reserved for init/planning.

Critic flow after author stages:

- After each successful author stage, Vibedrive asks whether the user wants a second opinion.
- If the user declines, the command returns to the create menu.
- If the user accepts, Vibedrive launches a fresh critic instance.
- The critic reads the workspace and `DESIGN.md`.
- The critic gives a critical second opinion for the selected stage.
- The critic does not edit `DESIGN.md` or workspace files.
- Critic feedback is shown in the terminal.
- Vibedrive launches a fresh author instance and passes the critic feedback back to the author.
- The author owns the final document changes and applies only feedback that improves the design.
- After the follow-up author finishes, create state is updated and the menu is shown again.

Planning handoff:

- Planning is visible in the create menu only when root `DESIGN.md` exists.
- Choosing Planning runs the same bootstrap planning flow as:

```text
vibedrive init --source DESIGN.md --author <create-author> --critic <create-critic>
```

- `DESIGN.md` is the only source used by this handoff.
- The handoff does not force overwrite existing init outputs.

### `vibedrive init`

Usage:

```text
vibedrive init [-config vibedrive.yaml] [-workspace DIR] [--source PATH ...] [--author claude|codex] [--critic claude|codex] [--print-sources] [-force] [SOURCE]
```

Features:

- Creates a scaffolded `vibedrive.yaml` if needed.
- Creates or bootstraps `COMPONENTS.md`.
- Creates `vibedrive-plan.yaml`.
- Accepts repeated `--source PATH` flags.
- Accepts at most one positional source as an alias for one additional source.
- If no sources are provided, source selection falls back to top-level regular files in the workspace.
- Source paths may refer to files or directories.
- Directory sources resolve to contained regular files.
- Resolved sources are deduplicated and sorted deterministically.
- Empty source values are rejected.
- Empty source selections are rejected.
- `--print-sources` prints the resolved source set and exits without writing config, writing plan files, or launching agents.
- `-force` overwrites generated init outputs and removes stale plan/component outputs before regenerating.
- Without `-force`, an existing config is left in place.
- Without `-force`, an existing plan file causes init to skip plan generation.
- Init uses fresh agent instances for each phase, even when author and critic are the same agent type.
- Transient critic feedback files are removed after the init flow completes.

Init phases:

1. Write or keep `vibedrive.yaml`.
2. Author reads all resolved sources and writes `COMPONENTS.md`.
3. Critic reviews `COMPONENTS.md` against the resolved sources and writes transient feedback.
4. Author revises `COMPONENTS.md` from actionable feedback.
5. Author reads sources and `COMPONENTS.md` and creates `vibedrive-plan.yaml`.
6. Critic reviews `vibedrive-plan.yaml` without editing it and writes transient feedback.
7. Author revises `vibedrive-plan.yaml` from actionable feedback.

Component discovery requirements:

- Components must have stable kebab-case IDs.
- Components should be narrow enough for independent work but not so small that every task becomes cross-cutting.
- Component entries should identify owned paths, public interfaces, contracts, generated artifacts, commands, packages, service boundaries, reads, provides, integration checkpoints, dependency ordering, and parallelization risks.
- Component discovery should identify foundation or contract-first work needed before cross-component implementation.
- Component discovery should call out shared paths, shared contracts, unclear ownership, conflicts, and required sequencing.

Plan generation requirements:

- The plan must be valid YAML.
- The plan must describe the full project objective.
- The plan must include source documents and hard constraint files.
- `COMPONENTS.md` must be included in source coverage when it is part of init output.
- The plan must honor component IDs and boundaries from `COMPONENTS.md`.
- Tasks should be sized for one focused implementation iteration and one coherent commit when practical.
- Tasks should include acceptance criteria and verification commands where deterministic checks exist.
- Routine testing, verification, and cleanup should stay inside the implementation task that introduces the change.
- Standalone tech-debt tasks should appear only when risk triggers justify them, such as new abstractions, risky temporary coupling, destructive or stateful behavior, broad expected implementation surface, unresolved hardening, rollback safety, or similar discovered risk.
- Standalone tech-debt tasks must state the trigger that justifies them.
- Each task should include a final acceptance item requiring short phase-learning notes for future replanning.
- UI, visual, or interactive work should include deterministic verification evidence requirements.
- Plans should include preparatory work for new verification tooling before commands that depend on that tooling.
- Cross-cutting implementation tasks should depend on a contract or foundation task that establishes shared interfaces or ownership.

### `vibedrive restart`

Usage:

```text
vibedrive restart [-config PATH] [-workspace DIR]
```

Features:

- Replans an existing project from prior run learnings.
- Rejects positional arguments.
- Loads the current config, current plan, and `.vibedrive/task-notes.yaml`.
- Reads current source documents and constraint files recorded in the plan.
- If no source docs are recorded, the restart prompt tells the agent to use the current plan and referenced local docs it discovers.
- Runs an agent prompt to rewrite the plan in place from the existing plan, current sources, and prior task notes.
- Runs a second agent prompt to review and directly improve the restarted plan.
- Converts useful prior task notes into better tasks, dependencies, details, acceptance criteria, verification, ownership metadata, or checkpoints.
- Regenerates component, ownership, contract, and integration-boundary metadata from current sources and prior-run learnings rather than blindly preserving stale boundaries.
- Preserves source requirements and hard constraints.
- Resets every task status to `todo`.
- Clears stale task notes from the plan.
- Removes `.vibedrive/task-notes.yaml` after preparing the fresh restart.
- Keeps routine testing and cleanup attached to implementation tasks unless risk-triggered follow-up work is justified.
- Requires self-verification paths agents can run without manual help.

### `vibedrive run`

Usage:

```text
vibedrive run [-config PATH] [-workspace DIR] [-dry-run] [-coder claude|codex] [-reviewer claude|codex]
```

Features:

- Loads `vibedrive.yaml`.
- Checks configured agent version pins before starting work.
- Loads the configured plan file.
- Selects ready tasks from the plan.
- Executes each selected task's configured workflow steps.
- Stops when all runnable tasks are terminal.
- Stops with an error when unfinished tasks remain but no task is ready.
- Stops when `max_iterations` is reached.
- Detects repeated no-progress iterations and stops after `max_stalled_iterations`.
- A run iteration counts as progress when the selected task status changes or its task notes change.
- `--dry-run` renders prompts and commands without launching agents, running commands, writing task artifacts, or changing task state.
- Runtime `--coder` and `--reviewer` can flip roles without changing config or plan files.
- The same agent may be used as both coder and reviewer.
- Agent sessions opened for a task are closed after the task's workflow is complete.
- Shared agent sessions are closed even when a later step fails.
- If both coder and reviewer resolve to the same agent type, that agent session can be reused across role steps.
- A step with `fresh_session: true` gets a one-off session and is closed after that step.
- The run records currently executing task and step state for status views while execution is active.
- Runtime state is cleared after tasks or runs finish.

Serial task selection:

- `in_progress` tasks are selected before `todo` tasks.
- Dependencies must be terminal before a task is ready.
- Terminal dependency statuses are `done`, `blocked`, and `manual`.
- When all tasks are terminal, the run reports completion.
- When unfinished tasks remain but dependencies prevent all work, the run reports that no ready tasks remain and lists unfinished tasks.

Workflow execution:

- A workflow is an ordered list of steps.
- A task can name a workflow.
- If a task omits a workflow, `default_workflow` is used.
- If no default is set and only one workflow exists, that workflow may be used.
- A missing or unknown workflow is an error.
- Legacy top-level steps are supported when no workflows are configured.
- Disabled steps are skipped.
- Steps with `continue_on_error: true` log the failure and continue to the next step.
- Steps with `timeout` fail when the timeout is reached.
- Agent prompts and exec command fields are rendered from task, plan, workspace, artifact, and run context.
- Exec steps run in the workspace by default.
- Exec steps may specify a working directory relative to the workspace or absolute.
- Exec steps may specify templated environment variables.
- Exec step failures include a tail of command output in the reported error.

Default scaffolded runtime workflow:

1. Coder executes the selected task and writes task result JSON.
2. Reviewer reviews uncommitted changes and writes peer review JSON.
3. Coder addresses peer review findings and keeps task result JSON accurate.
4. Finalizer applies the task result to the plan, runs verification, writes task notes, removes transient artifacts, and commits when changes are staged.

Checkpoint workflow:

- Checkpoint tasks are intended for integrated verification after dependent component work.
- The checkpoint coder runs required verification, fixes regressions, and writes task result JSON.
- The reviewer reviews checkpoint changes.
- The coder addresses checkpoint review findings.
- The finalizer applies status, notes, verification, artifact cleanup, and commit behavior.

Verification failure follow-up:

- If finalization runs verification for a `done` task and verification fails, the task is reset to `in_progress`.
- A verification-failure note is written to `.vibedrive/task-notes.yaml`.
- The result and review artifacts are removed.
- The failure is treated as progress during `run`, so the next iteration can repair the task instead of aborting as a stall.
- The next coder prompt includes verification follow-up context from the task notes and asks the agent to fix the failure before marking the task done again.

### `vibedrive view`

Usage:

```text
vibedrive view [-config PATH] [-workspace DIR] [-plan vibedrive-plan.yaml] [--active-only]
```

Features:

- Loads the current plan and, when available, config.
- Can render a plan without config when `--plan` is supplied.
- Rejects positional arguments.
- Displays project name.
- Displays project objective when present.
- Displays plan path.
- Displays workspace when config is loaded.
- Displays effective parallelism as serial or enabled with maximum batch size.
- Displays progress as completed task count and percentage.
- Displays counts for `done`, `in_progress`, `blocked`, `manual`, and `todo`.
- Displays the next ready task or explains why no task is ready.
- Displays currently active task/step summaries while `vibedrive run` is executing.
- Displays a legend:
  - `[x]` done
  - `[~]` in progress
  - `[!]` blocked
  - `[?]` manual
  - `[ ]` todo
- Displays an ASCII dependency graph.
- Displays each task's configured workflow step pipeline.
- Step completion is inferred from the enclosing task status when no active runtime state exists.
- While a task is active, steps before the active step are shown done, the active step is shown in progress, and later steps are shown todo.
- Repeated graph nodes are marked as already shown.
- Dependency cycles in display are detected and called out instead of recursing forever.

`--active-only`:

- Prints completed task count.
- Prints only currently executing tasks and their active step summaries.
- Omits the full graph and inactive tasks.
- Prints `none` when no active tasks exist.

### `vibedrive plan ready-batch`

Usage:

```text
vibedrive plan ready-batch [-config PATH] [-workspace DIR] [-plan vibedrive-plan.yaml] [-limit N]
```

Features:

- Inspects the plan without launching agents.
- Can load the plan from config or directly from `--plan`.
- Rejects positional arguments.
- Requires `limit >= 1`.
- Prints the plan path.
- Prints the selected ready batch as `Ready batch (selected/limit)`.
- Prints selected task IDs and titles.
- Prints `none` when no task is selected.
- Prints not-selected tasks with reasons and useful details.
- Explains unmet dependencies with dependency IDs.
- Explains conflicts with the selected task that caused exclusion.

Selection rules:

- Considers `in_progress` tasks before `todo` tasks.
- Excludes tasks with unmet dependencies.
- Stops selecting once the limit is reached.
- Allows independent tasks when write boundaries are compatible.
- Uses task `owns_paths` when present.
- Falls back to component `owned_paths` when a task has a component and no task-level `owns_paths`.
- Keeps tasks serial when ownership metadata is missing and another task is already selected.
- Excludes tasks with explicit `conflicts_with` relationships.
- Excludes tasks whose ownership paths overlap with already selected tasks.
- Excludes tasks that provide the same contract file as an already selected task.
- Reports reasons as:
  - `unmet_dependencies`
  - `limit_reached`
  - `explicit_conflict`
  - `missing_ownership_metadata`
  - `ownership_conflict`
  - `contract_writer_conflict`

### `vibedrive task finalize`

Usage:

```text
vibedrive task finalize --workspace DIR --plan PATH --task TASK_ID --result PATH [--message MSG]
```

Features:

- Requires `--workspace`, `--plan`, `--task`, and `--result`.
- Loads the plan and finds the target task.
- Reads task result JSON from `--result`.
- Supports result statuses `done`, `in_progress`, `blocked`, and `manual`.
- Rejects unsupported statuses.
- Applies the resulting status to the target task in `vibedrive-plan.yaml`.
- Saves result notes to `.vibedrive/task-notes.yaml`.
- Migrates legacy notes already present in the plan into task notes when task notes are missing.
- Keeps task notes out of the saved plan file.
- Removes the task result file after applying it.
- Removes the default peer review artifact for the task after applying it.
- Runs the task's `verify_commands` before a `done` result is allowed to remain done.
- Runs verification commands from the workspace.
- Skips empty verification commands.
- If verification fails:
  - changes the task result status to `in_progress`
  - appends a verification-failure note naming the failed command
  - saves status and notes
  - removes transient result and review artifacts
  - does not commit the unverified change as done
  - returns a verification error
- If verification succeeds or no verification commands are configured, finalization commits staged changes when needed.
- The commit message is `--message` when supplied.
- The scaffolded finalizer passes the task's `commit_message` when present, otherwise the task title.
- If no changes are staged, no commit is created.
- Finalization stages source changes while excluding transient or generated artifact directories that should not be committed by default, including `.vibedrive/**`, build caches, dependency directories, and bytecode caches.
- Excluded artifacts remain in the working tree.

## Plan File Features

### Plan Structure

`vibedrive-plan.yaml` has two top-level sections:

- `project`
- `tasks`

The plan must contain at least one task.

### Project Fields

- `name`: short project name.
- `objective`: one-sentence project objective.
- `source_docs`: source requirements and design documents used to generate the plan.
- `constraint_files`: source files that define hard requirements, gates, or limits.
- `components`: optional component catalog.

### Component Fields

- `id`: required stable component ID.
- `name`: optional human-readable name.
- `owned_paths`: repo-relative path globs owned by the component.
- `reads_contracts`: repo-relative contracts or interfaces the component consumes.
- `provides_contracts`: repo-relative contracts or interfaces the component owns or provides.

Component validation:

- Component IDs must be non-empty.
- Component IDs must be unique.
- Path metadata must be repo-relative.
- Path metadata must not be absolute.
- Path metadata must not contain parent path traversal.

### Task Fields

- `id`: required stable task ID.
- `title`: required human-readable task title.
- `details`: optional extra context.
- `status`: task state. Empty status defaults to `todo` when loaded.
- `workflow`: optional workflow name.
- `kind`: optional planning metadata preserved in the plan.
- `deps`: task IDs that must be terminal before this task can run.
- `context_files`: repo-relative files the task should consider.
- `component`: optional component ID.
- `owns_paths`: repo-relative paths the task may edit.
- `reads_contracts`: repo-relative contracts or interfaces the task consumes.
- `provides_contracts`: repo-relative contracts or interfaces the task creates, updates, owns, or provides.
- `conflicts_with`: task IDs that must not run in the same parallel batch.
- `acceptance`: task acceptance criteria.
- `verify_commands`: shell commands run by finalization before `done` is accepted.
- `commit_message`: optional commit message for default finalization.
- `notes`: legacy or transient notes read for migration but not saved back into the plan.

Task validation:

- Task IDs must be non-empty.
- Task IDs must be unique.
- Task titles must be non-empty.
- Task statuses must be supported.
- Dependencies must reference known tasks.
- `conflicts_with` entries must reference known tasks.
- A task cannot conflict with itself.
- If a component catalog exists, task `component` values must reference known components.
- Path metadata must be repo-relative.
- Path metadata must not be absolute.
- Path metadata must not contain parent path traversal.
- Unknown YAML fields are tolerated so external planning tools can preserve extra metadata.

### Task Statuses

- `todo`: not started. Eligible when dependencies are terminal.
- `in_progress`: partially complete. Selected before `todo` when ready.
- `blocked`: terminal state for work that cannot continue without an external dependency or decision.
- `done`: terminal state for completed work.
- `manual`: terminal state for human-owned or manually completed work.

### Dependencies

- A dependency is satisfied when the dependency task is terminal.
- Terminal statuses are `done`, `blocked`, and `manual`.
- A task with no dependencies is ready when its status is `todo` or `in_progress`.
- A task with dependencies is ready when all dependency tasks are terminal and its own status is `todo` or `in_progress`.

### Boundary Metadata

- Boundary metadata exists to reduce agent context and make safe parallelism provable.
- `project.components` describes durable ownership.
- Task-level metadata can narrow or override the effective task boundary.
- `owns_paths` represents explicit edit authority.
- `reads_contracts` represents contracts/interfaces a task consumes.
- `provides_contracts` represents contracts/interfaces a task owns or writes.
- `conflicts_with` represents explicit task-level incompatibility for parallel execution.
- Tasks lacking ownership metadata can still run serially.
- Tasks lacking ownership metadata are not batched with other selected tasks.

### String Lists

- Plan string-list fields must be YAML lists.
- List fields include `source_docs`, `constraint_files`, `owned_paths`, `reads_contracts`, `provides_contracts`, `deps`, `context_files`, `conflicts_with`, `acceptance`, and `verify_commands`.
- Items containing a colon followed by a space should be quoted.
- The app normalizes a colon-prefixed nested YAML form into the intended single string when loading legacy acceptance list entries.

## Config File Features

### Config File

- The default config path is `vibedrive.yaml`.
- `--config PATH` selects a different config file.
- `vibedrive init` writes a complete scaffolded config.
- Hand-written configs are supported.
- The config must define at least one workflow or top-level step for execution.

### Top-Level Config Fields

- `workspace`: directory where agents and exec steps run. Defaults to `.`.
- `plan_file`: machine-readable task file. Defaults to `vibedrive-plan.yaml`.
- `max_iterations`: maximum run iterations. `0` means unlimited.
- `max_stalled_iterations`: maximum no-progress iterations on the same item. Must be at least `1`.
- `default_workflow`: workflow used by tasks that omit `workflow`.
- `dry_run`: renders prompts and commands without executing.
- `parallel`: optional parallel execution settings.
- `claude`: Claude agent settings.
- `codex`: Codex agent settings.
- `steps`: legacy ordered steps used when workflows are not configured.
- `workflows`: named workflow definitions.

### Parallel Config

- `enabled`: opts into parallel execution.
- `max_parallelism`: maximum safe ready tasks per batch. Must be at least `1`.
- `worktree_root`: workspace-relative or absolute root for isolated worker workspaces.
- `artifact_root`: workspace-relative or absolute root for isolated worker artifacts.
- Scaffolded configs enable parallelism by default with max parallelism `3`.
- Hand-written configs without explicit parallel enablement remain effectively serial.
- When parallelism is disabled or max parallelism is ineffective, the runner behaves serially.

### Claude Config

- `command`: executable to launch. Defaults to `claude`.
- `version`: optional exact `claude --version` output pin.
- `args`: command-line flags passed to Claude.
- `transport`: `tui` or `print`.
- `startup_timeout`: how long to wait for Claude startup. Defaults to `30s`.
- `session_strategy`: `session_id` or `continue`.
- If args omit an effort flag, the default effort is added.
- If args omit a permission flag, a default permission mode is added so agent steps do not stop on approval prompts.
- `session_id` creates a new session ID for a task.
- `continue` resumes according to the Claude CLI behavior and leaves `SessionID` empty.

### Codex Config

- `command`: executable to launch. Defaults to `codex`.
- `version`: optional exact `codex --version` output pin.
- `args`: command-line flags passed to Codex.
- `transport`: `tui` or `exec`.
- `startup_timeout`: how long to wait for Codex startup. Defaults to `30s`.
- If args are omitted, default Codex args are used.
- Codex args are normalized so bypass-permission behavior is present by default.
- Conflicting Codex approval or sandbox flags are removed when default bypass behavior is applied.
- If args omit a reasoning-effort override, a default reasoning-effort setting is added.
- Interactive Codex subcommands are allowed only with TUI transport.
- Non-interactive Codex subcommands are rejected for TUI agent steps.
- `codex.transport: exec` rejects args that already include a Codex subcommand, because the transport itself selects exec behavior.

### Version Pins

- `codex.version` is optional; `claude.version` is recorded but no longer enforced.
- When `codex.version` is configured, startup checks the live CLI version before running.
- A mismatch prevents the run and tells the user to update the pin only after intentionally upgrading the CLI.
- If the pinned codex CLI version cannot be read, startup fails with an error naming the pinned value and command.
- Init scaffolding records live version pins when the agent CLIs are available.
- Relative agent command paths are resolved from the workspace when checking versions.

### Workflow Fields

- A workflow contains an ordered `steps` list.
- Workflows are selected per task by `workflow`, by `default_workflow`, or by the single configured workflow fallback.

### Step Fields

- `name`: required step name shown in logs and views.
- `type`: one of `claude`, `codex`, `agent`, or `exec`; omitted type defaults to `claude`.
- `actor`: for `agent` steps, either `coder` or `reviewer`.
- `prompt`: required for `claude`, `codex`, and `agent` steps.
- `command`: required argv list for `exec` steps.
- `working_dir`: optional exec working directory.
- `env`: optional exec environment additions.
- `required_outputs`: files that must exist and be valid after a step.
- `fresh_session`: isolates an agent-backed step in a new session.
- `timeout`: optional duration applied to the step.
- `continue_on_error`: log failure and continue.
- `disabled`: skip the step.

Step validation:

- Step names are required.
- Agent prompts are required.
- `agent` steps must use actor `coder` or `reviewer`.
- Exec commands must have at least one argv element.
- Unknown step types are rejected.
- Agent steps require configured coder and reviewer roles.

### Template Data

Prompts, exec commands, working directories, environment values, and required output paths can use templates with:

- `Workspace`
- `PlanFile`
- `ConfigPath`
- `ExecutablePath`
- `Iteration`
- `SessionID`
- `WorkflowName`
- `TaskResultPath`
- `TaskNotesPath`
- `ReviewPath`
- `ArtifactRoot`
- `Plan`
- `Task`
- `Now`

Template rendering must fail clearly when a referenced field is missing or invalid.

## Required Output Features

- Steps can declare required output files.
- Required output paths are rendered from templates.
- Relative required output paths resolve under the workspace.
- Duplicate rendered output paths are deduplicated.
- Parent directories are created before the step runs.
- Missing required outputs cause non-agent steps to fail.
- If an agent step misses or corrupts required outputs, Vibedrive asks the same agent to repair them once.
- If a built-in task result or review artifact is still missing or invalid after repair, Vibedrive writes a conservative `in_progress` task result or `changes_requested` review fallback so the run can continue without marking the task done.
- Non-agent steps fail immediately when required outputs are missing or invalid.
- Task result artifacts are validated when they are required outputs.
- Review artifacts are validated when they are required outputs.

Task result JSON:

```json
{"status":"done|in_progress|blocked|manual","notes":"brief phase notes"}
```

Peer review JSON:

```json
{"decision":"approved|changes_requested","summary":"brief summary","findings":["actionable finding"]}
```

Validation:

- Task result JSON must parse.
- Task result status must be supported.
- Peer review JSON must parse.
- Peer review decision must be `approved` or `changes_requested`.
- A required output path that is a directory is invalid.
- Repair prompts name missing paths and invalid-file problems.
- Repair prompts instruct agents not to edit `vibedrive-plan.yaml` or unrelated files.

## Task Notes Features

- Task notes are stored in `.vibedrive/task-notes.yaml`.
- Each task note includes task ID, status, and notes text.
- Task notes are loaded as empty when the file does not exist.
- Task notes are trimmed when loaded.
- Writing task notes creates the parent `.vibedrive` directory.
- Upserting a note replaces an existing note for the same task or appends a new one.
- Invalid task notes YAML written by an agent is detected after agent steps.
- If an agent step changes task notes and leaves invalid YAML, Vibedrive asks the same agent to repair the task notes once.
- Repair prompts tell the agent to preserve existing task notes and statuses as much as possible.
- If repaired task notes still do not parse, the step fails.
- Restart consumes task notes as prior-run learning and removes the notes file after resetting the plan.

## Parallel Execution Features

### Safe Batch Selection

- Parallel execution is opt-in through config.
- The effective parallelism is one when parallelism is disabled.
- When effective parallelism is greater than one, the runner analyzes ready tasks each iteration.
- If only one task is selected, that iteration runs serially.
- If multiple tasks are selected, they run as an isolated batch.
- Safe batching depends on explicit boundaries, not just task readiness.
- Ready tasks can be excluded from a batch while still remaining eligible for a later serial or parallel iteration.

### Isolated Worker Execution

- Each parallel worker receives an isolated workspace.
- Each worker receives a plan snapshot.
- Each worker receives task notes copied from the root workspace.
- Each worker writes task result, review, and notes artifacts under its own artifact root.
- Workers run their configured workflow steps independently.
- A worker can report `done`, `in_progress`, `blocked`, or `manual`.
- A worker that makes no task progress is reported as failed.
- Parallel workers can use fullscreen agent sessions in separate panes or non-interactive transports.
- When fullscreen agents are needed for a parallel batch, the app prints a session name and attach command so the user can inspect workers.
- If the required multi-pane terminal support is unavailable for fullscreen parallel agents, the run fails clearly instead of silently corrupting the parent terminal.

### Parallel Integration

- Completed worker changes are collected and applied back to the root workspace.
- Transient Vibedrive artifacts, dependency directories, and build caches are excluded from worker integration.
- Worker results are integrated in deterministic order.
- A successful sibling task can still be integrated even when another worker fails.
- Worker status and notes are recorded in the root plan and task notes.
- Root verification runs after integrated changes when the worker reports `done`.
- Successful root integration and verification runs the normal finalization behavior and commit behavior.
- Integrated worker workspaces and artifacts are cleaned up after successful integration.

### Oversized Patch Fallback

- If a worker patch is too large to apply normally, Vibedrive can fall back to syncing changed files.
- The fallback first checks that each affected root path is unchanged from the worker base revision.
- The fallback refuses to overwrite root paths that changed independently.
- The fallback refuses unsafe paths, absolute paths, parent traversal, `.git` paths, directories where files are expected, and unsupported file modes.
- Added, modified, deleted, and symlinked files are handled.
- If fallback-integrated changes later fail verification, the root workspace is rolled back to the base state for those files.

### Parallel Failure Recovery

- If a parallel worker fails, only that task is marked `in_progress`.
- The task notes explain that the worker failed.
- Successful sibling integrations are preserved when possible.
- If a worker patch cannot be applied or safely synced, only that task is marked `in_progress`.
- A recovery patch, metadata, and README are preserved under the parallel artifact recovery area.
- If root verification fails after integrating a worker that reported `done`, the task is marked `in_progress`.
- A recovery patch and metadata are preserved for verification failures when possible.
- A follow-up commit can record the failed status and task notes.
- The next coder prompt for that task includes recovery context, the recovery patch path, and metadata path.
- The recovery prompt tells the coder to reconcile useful changes manually against the current workspace and not apply stale hunks blindly.
- Recovery artifacts are cleared when the task later reaches a terminal status.

## Diagnostics Features

- Step failures can write local diagnostics under `.vibedrive/debug`.
- Diagnostics are intended for local debugging and regression tests.
- Diagnostics are bounded so very large output cannot cause unbounded artifact growth.
- Diagnostics prefer output tails for process and parent output.
- Prompt diagnostics preserve the beginning and end of large prompts with an elision marker.
- Diagnostics include a manifest that records run ID, task ID, step name, safe path segments, failure path, failure message, transport kind, and artifact entries.
- Artifact entries record whether each artifact was written, unavailable, not applicable, or failed.
- Diagnostics write failures must not hide the original step failure.
- Diagnostic path segments must be safe and collision-resistant.
- Exec failures record command arguments, working directory, exit code or signal when known, timeout/cancellation state, redacted step environment values, inherited environment keys, stdout tail, stderr tail, combined output tail, and parent output tails.
- Agent process failures also record the prompt sent to the agent.
- Fullscreen agent failures record terminal pane output, title or state history, prompt payload, session metadata, trust prompt count, and parent output tails when available.
- Sensitive environment values are redacted by name patterns such as token, password, secret, key, and credential.
- Diagnostics are local artifacts and may contain sensitive prompts, source code, model output, or test data.

## Terminal Session Dashboard Features

- When a run uses fullscreen agent sessions, Vibedrive can open a live dashboard session.
- The dashboard includes an active-only status view refreshed regularly.
- Agent panes are arranged beside the status pane.
- Additional agent panes are stacked in a predictable layout.
- The app prints the attach command for the user.
- The dashboard session is closed when the run finishes.
- If the required multi-pane terminal support is missing, the app reports a clear error.

## Agent Prompt Delivery Features

- Multi-line TUI prompts are submitted as one agent request.
- If an agent does not accept a submitted prompt, Vibedrive retries submission a small number of times before failing.
- Empty prompts after normalization are rejected.
- Claude and Codex TUI steps submit the rendered prompt through the agent interface.
- Interactive prompts wait for the user to exit the agent interface.
- Non-interactive prompts are sent on standard input.
- Prompt completion is determined by the agent returning to a ready state or the process exiting successfully, depending on transport.
- Startup timeout, submit timeout, unexpected exit, non-zero exit, stdin write failure, cancellation, or unsupported state fails the step.

## Source and Artifact Files

### Root Files

- `DESIGN.md`: product/design document created or updated by `create`.
- `COMPONENTS.md`: component breakdown created or updated by `init`.
- `vibedrive.yaml`: config file created by `init`.
- `vibedrive-plan.yaml`: machine-owned execution plan created by `init`, revised by `restart`, and advanced by `run` or `task finalize`.

### `.vibedrive` Files

- `.vibedrive/create-state.json`: create flow last-stage state.
- `.vibedrive/task-notes.yaml`: task status notes learned during execution.
- `.vibedrive/task-results/<task>.json`: transient task result artifacts for serial execution.
- `.vibedrive/reviews/<task>.json`: transient peer review artifacts for serial execution.
- `.vibedrive/run-state.json`: transient active run/task/step state.
- `.vibedrive/task-runs/*`: parallel worker artifacts.
- `.vibedrive/task-runs/recovery/*`: recovery patches and metadata for failed parallel integration.
- `.vibedrive/worktrees/*`: isolated parallel workspaces when configured.
- `.vibedrive/debug/*`: local diagnostics bundles for step failures.

## Error Handling and Validation Features

- Commands report missing required flags clearly.
- Commands that do not accept positional arguments reject them.
- Unsupported agents are rejected with an error naming valid choices.
- Unsupported transports are rejected with an error naming the field and value.
- Invalid duration fields fail clearly.
- Missing config is allowed only where the command has an explicit config-free path, such as `create`, `start`, `view --plan`, or init source preview.
- Missing plan files fail commands that need a plan.
- Invalid YAML in config or plan files fails loading.
- Plan validation rejects duplicate task IDs, duplicate component IDs, unknown dependencies, unknown conflicts, self-conflicts, unsupported statuses, unknown component references when a component catalog exists, and unsafe path metadata.
- Config validation rejects missing workflow or step configuration, invalid parallelism, invalid step definitions, invalid runtime roles, unsupported Codex subcommands, and unknown transports.
- Required output validation distinguishes missing files, directories, malformed task result JSON, malformed review JSON, unsupported result statuses, and unsupported review decisions.
- Run errors include enough task and step context for the user to know what failed.

## Installation and Requirements

- The app is installed as a CLI named `vibedrive`.
- Claude CLI is required for workflows that use Claude.
- Codex CLI is required for workflows that use Codex.
- Git is required for commit/finalize behavior and for normal parallel integration behavior.
- Fullscreen parallel agent batches require terminal support capable of hosting multiple panes.
- A plan file is required for `run`, `view` without `--plan`, `plan ready-batch` without `--plan`, `restart`, and `task finalize`.
