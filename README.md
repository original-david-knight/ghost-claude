# vibedrive

**Run Claude Code and Codex through a plan until your project is built.**

vibedrive is a CLI that orchestrates AI coding agents against a machine-readable task graph. You can bring your own spec document or run `vibedrive create` to have agents help author `DESIGN.md` interactively. `vibedrive init` turns that spec into `vibedrive-plan.yaml`, then `vibedrive run` loops: pick the next task, have one agent implement it, have another agent peer-review the diff, apply the review feedback, run per-task verification commands, commit — and move on. The loop stops when the plan is done.

Claude Code and Codex both run in their real fullscreen TUIs inside a PTY, so you can watch the work happen. Either agent can play coder or reviewer; pick roles per run.

## Why use it

- **Unattended loops.** The runner picks the next ready task, dispatches it to the agents, stages, verifies, and commits — no babysitting.
- **Two agents, flipped at runtime.** Choose `--coder` and `--reviewer` per run. Defaults: Codex codes, Claude reviews. Flip them, or use the same agent for both.
- **Machine-owned state.** `vibedrive-plan.yaml` is the execution queue. Every task ends by writing back its status there and short phase notes in `.vibedrive/task-notes.yaml`, so the run is resumable and the plan stays focused on task structure.
- **Per-task verification.** Each task declares its own `verify_commands` (build, tests, linters). Plans should include any harnesses or instrumentation agents need to verify their own work, such as scripted screenshot capture for UI changes. A task only stays `done` when its commands pass; otherwise it drops back to `in_progress` with a failure note.
- **Boundary-first parallelism.** Plans can describe components, contract files, owned paths, and explicit task conflicts. Vibedrive uses that metadata to reduce each agent's context and, when parallel execution is explicitly enabled, to select safe batches.
- **Replan with memory.** `vibedrive restart` reads the existing plan plus prior task notes and regenerates a fresh plan informed by what the earlier run actually learned.

## Example

From inside the repo you want built:

```bash
vibedrive start               # alias for create; does not require vibedrive.yaml
vibedrive create              # interactively author DESIGN.md with the default Codex author and Claude critic
vibedrive init DESIGN.md      # scaffold vibedrive.yaml + vibedrive-plan.yaml from your spec
vibedrive run                 # run the loop: codex codes, claude reviews
```

That's the whole flow. The runner walks the plan, dispatches each task, commits each iteration, and stops when there is nothing left to do. Rerunning `vibedrive run` resumes where you left off.

## Requirements

- Go 1.26+ (the version declared in `go.mod` is currently `1.26.0`)
- The `claude` CLI installed and on your `$PATH` ([Claude Code](https://docs.claude.com/en/docs/claude-code))
- The `codex` CLI installed and on your `$PATH` for the default scaffolded implementation flow
- A `vibedrive-plan.yaml` file with machine-readable tasks

## Install

```bash
go install ./cmd/vibedrive
```

Or run without installing:

```bash
go run ./cmd/vibedrive <subcommand>
```

## Quick start

From inside the repo you want vibedrive to work on:

```bash
vibedrive start             # alias for create; starts without vibedrive.yaml
vibedrive create            # menu-driven DESIGN.md authoring; defaults author=codex and critic=claude
vibedrive create --author claude --critic codex
vibedrive init              # writes vibedrive.yaml, resolves top-level regular files as sources by default, then runs author, critic, and author-revision bootstrap phases
vibedrive init --author claude   # bootstrap the plan with Claude instead of the default Codex author
vibedrive init --critic codex    # use Codex for the critic phase instead of the default Claude critic
vibedrive init DESIGN.md    # single positional source alias
vibedrive init --source DESIGN.md --source docs/specs
vibedrive init --source DESIGN.md --source docs/specs --print-sources   # preview resolved sources without writing config or plan
vibedrive restart           # replans from prior task notes, then resets vibedrive-plan.yaml to a fresh-run state
vibedrive run    # starts the loop with coder=codex and reviewer=claude
vibedrive run --coder claude --reviewer codex   # flips roles at run time without changing vibedrive-plan.yaml
vibedrive run --coder codex --reviewer codex    # same agent can both code and review
```

Target a different repo without `cd`:

```bash
vibedrive create --workspace /path/to/repo
vibedrive init --workspace /path/to/repo
vibedrive restart --workspace /path/to/repo
vibedrive run  --workspace /path/to/repo
```

Preview what would happen without touching anything:

```bash
vibedrive run --dry-run
vibedrive plan ready-batch --limit 3
vibedrive init --print-sources
vibedrive init --print-sources --source DESIGN.md --source docs/specs
```

`vibedrive create` intentionally has no `--dry-run` mode. It also does not support `--resume` or a positional idea argument; explain the idea interactively to the Product Definition author agent.

`vibedrive init` bootstraps the plan. The generated config points the runner at `vibedrive-plan.yaml`.

## Create `DESIGN.md`

Usage:

```bash
vibedrive create [-workspace DIR] [--author claude|codex] [--critic claude|codex]
```

`vibedrive create` starts a numbered menu with Product Definition, Feature/Refactor, UX Review, Technical Review, and Stop. Planning appears in the menu only when `DESIGN.md` exists at the workspace root, and it appears whenever that file exists regardless of the last completed create stage.

The authoring stages all write or update the same root `DESIGN.md`:

1. Product Definition inspects the workspace, interviews the user in the agent TUI, pushes on unclear product scope or success criteria, and writes the initial design with agent-verifiable outcomes.
2. Feature/Refactor inspects an existing project, interviews the user about a new feature or refactor, captures current behavior to preserve, affected workflows, compatibility constraints, rollout or migration needs, relevant code paths, and agent-verifiable acceptance criteria.
3. UX Review reads the workspace and current `DESIGN.md`, then improves user journeys, interaction design, accessibility, states, content, terminology, visual verification needs, and related product/design tradeoffs where useful.
4. Technical Review reads the workspace and current `DESIGN.md`, then adds architecture, data model, integration, risk, edge-case, testing, self-verification instrumentation, rollout, and rough implementation guidance without creating a task-by-task plan.

Product Definition and Feature/Refactor keep the author agent TUI open for the interview. When the author has updated `DESIGN.md`, exit the agent TUI to return to Vibedrive; for Codex this is usually Ctrl-D, and for Claude type `/exit`.

After each successful author stage, vibedrive returns to the menu. You can rerun any stage, jump forward or backward, stop and manually edit `DESIGN.md`, or choose Planning once `DESIGN.md` exists.

After each stage, vibedrive asks whether you want a second opinion. If you answer yes, it runs the critic in a fresh instance, shows the critic output in the terminal, then passes that feedback to a fresh author instance. The critic does not edit `DESIGN.md`; the author owns all document changes. Defaults are author=`codex` and critic=`claude`, and both roles can be set to either `claude` or `codex`.

Choosing Planning reuses the existing init bootstrap flow exactly as if you had run:

```bash
vibedrive init --source DESIGN.md --author <same-author> --critic <same-critic>
```

`DESIGN.md` is the only init source used by the create handoff. `vibedrive create` does not support `--dry-run`, `--resume`, or a positional idea argument by design.

## How the loop works

For each iteration:

1. Load `vibedrive-plan.yaml`. With default serial execution, select the next ready task, preferring `in_progress` over `todo` and respecting dependencies.
2. When `parallel.enabled: true` and effective parallelism is greater than one, analyze the ready tasks for a safe batch. Tasks only run together when their declared boundaries make that safe; otherwise the runner falls back to one task for that iteration.
3. Start fresh agent sessions when a step needs one.
4. Run every configured step in order for each selected task. Claude and Codex TUI steps share one session for the work item by default, but `fresh_session: true` isolates a step in its own session.
5. Close any agent sessions that were opened.
6. Re-read the queue state. If the selected item changed state, advance. If not, count a stall and retry.

The runner stops when there is no work left, when `max_iterations` is reached, or when the same item stalls `max_stalled_iterations` times in a row.

The default workflow scaffolded by `vibedrive init` uses `vibedrive-plan.yaml` as the execution queue:

1. Execute the selected task with the current coder while preserving the plan's hard constraints.
2. Ask the current reviewer to review the changes and write a structured review artifact.
3. Run a second coder step that reads the review artifact and fixes any actionable findings.
4. Run the task's configured `verify_commands`, apply the JSON task status to `vibedrive-plan.yaml`, write notes to `.vibedrive/task-notes.yaml`, and commit the iteration with an exec step.

During `init`, vibedrive bootstraps the plan in four steps:

1. Write `vibedrive.yaml`.
2. Launch the selected author in a fresh instance to read every resolved init source and create `vibedrive-plan.yaml`.
3. Close the author, launch the selected critic in a fresh instance, and have it review the plan without editing it. Critic feedback is passed through a transient `.vibedrive` artifact, not saved next to `vibedrive-plan.yaml`.
4. Close the critic, launch the author again in a fresh instance, and have it revise `vibedrive-plan.yaml` from the actionable critic feedback.

`vibedrive init` defaults to `--author codex` and `--critic claude`. You can set either role to Claude or Codex, including the same agent type for both; each phase still starts a fresh instance with new context. These bootstrap roles do not change the runtime coder/reviewer defaults used by `run`. You can supply sources with repeatable `--source` flags and still use a single positional source as an alias for one extra entry. When no source is provided, init falls back to all top-level regular files in the workspace directory. `vibedrive init --print-sources` resolves that same deduped, sorted source set in deterministic order and exits before writing config or prompting agents. `-force` keeps its existing behavior of overwriting generated init files. The bootstrap prompts keep testing and cleanup expectations inline with implementation by default, and only ask for standalone tech-debt tasks when planning-time risk triggers apply, such as a new abstraction, risky temporary coupling or workaround, destructive or stateful behavior, or a broad expected implementation surface. Those triggers describe expected breadth and discovered risk, not actual changed-file counts that only exist after execution.

## Subcommands

```
vibedrive run  [-config PATH] [-workspace DIR] [-dry-run] [-coder claude|codex] [-reviewer claude|codex]
vibedrive start [-workspace DIR] [--author claude|codex] [--critic claude|codex]
vibedrive create [-workspace DIR] [--author claude|codex] [--critic claude|codex]
vibedrive init [-config vibedrive.yaml] [-workspace DIR] [--source PATH ...] [--author claude|codex] [--critic claude|codex] [--print-sources] [-force] [SOURCE]
vibedrive restart [-config PATH] [-workspace DIR]
vibedrive plan ready-batch [-config vibedrive.yaml] [-workspace DIR] [-plan vibedrive-plan.yaml] [-limit N]
vibedrive task finalize --workspace DIR --plan PATH --task TASK_ID --result PATH [--message MSG]
vibedrive help
```

If you omit the subcommand, `vibedrive` behaves like `run`.

When `-workspace` is set, the default config path becomes `<workspace>/vibedrive.yaml` and every relative path in the config resolves against that workspace.

## Config

The runner reads `vibedrive.yaml` by default. `vibedrive init` writes a complete scaffold matching [`vibedrive.example.yaml`](vibedrive.example.yaml). Shortened example:

```yaml
workspace: .
plan_file: vibedrive-plan.yaml
max_iterations: 0
max_stalled_iterations: 2
default_workflow: implement

parallel:
  enabled: false
  max_parallelism: 3
  worktree_root: .vibedrive/worktrees
  artifact_root: .vibedrive/task-runs

claude:
  command: claude
  transport: tui
  startup_timeout: 30s
  session_strategy: session_id
  args:
    - --effort
    - max
    - --permission-mode
    - bypassPermissions

codex:
  command: codex
  transport: tui
  startup_timeout: 30s
  args:
    - --dangerously-bypass-approvals-and-sandbox
    - -c
    - model_reasoning_effort="xhigh"

workflows:
  implement:
    steps:
      - name: execute-task
        type: agent
        actor: coder
        prompt: |
          Execute task {{ .Task.ID }} from {{ .PlanFile }}.
          Title: {{ .Task.Title }}

          Hard constraints to preserve:
          {{- range .Plan.Project.ConstraintFiles }}
          - {{ . }}
          {{- end }}
        required_outputs:
          - "{{ .TaskResultPath }}"

      - name: peer-review
        type: agent
        actor: reviewer
        prompt: |
          Review the current uncommitted changes for task {{ .Task.ID }} from {{ .PlanFile }}.
        required_outputs:
          - "{{ .ReviewPath }}"

      - name: address-peer-review
        type: agent
        actor: coder
        prompt: |
          Read the peer review artifact at {{ .ReviewPath }} for task {{ .Task.ID }} from {{ .PlanFile }}.
        required_outputs:
          - "{{ .TaskResultPath }}"

      - name: finalize-task
        type: exec
        command:
          - "{{ .ExecutablePath }}"
          - task
          - finalize
          - --workspace
          - "{{ .Workspace }}"
          - --plan
          - "{{ .PlanFile }}"
          - --task
          - "{{ .Task.ID }}"
          - --result
          - "{{ .TaskResultPath }}"
          - --message
          - "{{- if .Task.CommitMessage -}}{{ .Task.CommitMessage }}{{- else -}}{{ .Task.Title }}{{- end -}}"
```

### `vibedrive-plan.yaml`

The runner uses a machine-readable task file. The repository ships a complete starter in [`vibedrive-plan.example.yaml`](vibedrive-plan.example.yaml). Boundary-aware example:

```yaml
project:
  name: service-web-v1
  objective: Ship the service and web client behind a stable public contract.
  source_docs:
    - DESIGN.md
    - TEST_PLAN.md
  constraint_files:
    - DESIGN.md
  components:
    - id: contracts
      name: Shared contracts
      owned_paths:
        - internal/contracts/**
      provides_contracts:
        - internal/contracts/public-api.md
    - id: service
      name: Service implementation
      owned_paths:
        - internal/service/**
      reads_contracts:
        - internal/contracts/public-api.md
    - id: web
      name: Web client
      owned_paths:
        - web/**
      reads_contracts:
        - internal/contracts/public-api.md

tasks:
  - id: define-public-contract
    title: Define the public contract shared by service and web work
    workflow: implement
    status: todo
    component: contracts
    owns_paths:
      - internal/contracts/**
    provides_contracts:
      - internal/contracts/public-api.md
    acceptance:
      - contract file defines the service and web interface
      - task notes capture what was learned in this phase and any replanning input for a fresh rerun
    verify_commands:
      - test -f internal/contracts/public-api.md
    commit_message: docs: define public integration contract

  - id: implement-service-contract
    title: Implement the service side of the public contract
    workflow: implement
    status: todo
    deps:
      - define-public-contract
    component: service
    owns_paths:
      - internal/service/**
    reads_contracts:
      - internal/contracts/public-api.md
    acceptance:
      - service behavior matches the contract
    verify_commands:
      - go test ./internal/service/...

  - id: implement-web-contract
    title: Implement the web client side of the public contract
    workflow: implement
    status: todo
    deps:
      - define-public-contract
    component: web
    owns_paths:
      - web/**
    reads_contracts:
      - internal/contracts/public-api.md
    acceptance:
      - web behavior matches the contract
    verify_commands:
      - npm test --prefix web

  - id: integration-checkpoint
    title: Run the service and web integration checkpoint
    workflow: checkpoint
    status: todo
    deps:
      - implement-service-contract
      - implement-web-contract
    reads_contracts:
      - internal/contracts/public-api.md
    conflicts_with:
      - implement-service-contract
      - implement-web-contract
    acceptance:
      - full integration verification is complete after both component implementations are integrated
      - task notes capture what was learned in this phase and any replanning input for a fresh rerun
    verify_commands:
      - go test ./...
      - npm test --prefix web
```

The intended use is:

- `vibedrive-plan.yaml` is machine-owned execution state
- the runner advances by updating task status in `vibedrive-plan.yaml` and task notes in `.vibedrive/task-notes.yaml`
- each task should end by leaving short notes about what it learned in that phase so the plan can be revised and rerun from a fresh environment
- `vibedrive restart` re-reads the current plan, source docs, and prior task notes, then rewrites `vibedrive-plan.yaml` for a fresh rerun with every task back at `todo` and clears stale task notes
- `vibedrive init` can generate the initial plan from one or more `--source` inputs, the single positional source alias, or the workspace's top-level regular files when you omit sources
- `vibedrive init` runs fresh author, critic, and author-revision agent instances; the critic reviews without editing the plan and the author owns both plan writes
- generated `init` and `restart` plans should identify components, owned paths, interfaces/contracts, and integration checkpoints before generating executable tasks
- boundary metadata such as `component`, `owns_paths`, `reads_contracts`, and `provides_contracts` is meant to reduce the context each agent needs, not merely to make parallel work faster
- cross-cutting implementation tasks should be split by component or depend on an earlier contract/foundation task that establishes the shared interface or ownership model
- the scaffolded `init` prompt keeps testing and cleanup work inside implementation tasks unless explicit planning-time risk triggers justify a standalone tech-debt follow-up
- generated plans should give agents a self-verification path for each task, including preparatory checks, fixtures, harnesses, screenshot capture, or other instrumentation when existing commands are not enough
- those risk triggers are about expected breadth and discovered risk from the source inputs or prior notes, not runtime-observed changed-file counts
- your external planning tool can still generate both files if you prefer that flow

### Boundary-first parallelism

Parallel execution is an outcome of explicit boundaries, not a blanket speed setting. A good plan first identifies components, the contract files they share, the paths each task may edit, and the tasks that must never run together. That metadata lets each agent receive less context and gives the runner enough information to decide whether a batch is safe.

Use `project.components` for durable ownership:

```yaml
components:
  - id: service
    owned_paths:
      - internal/service/**
    reads_contracts:
      - internal/contracts/public-api.md
  - id: web
    owned_paths:
      - web/**
    reads_contracts:
      - internal/contracts/public-api.md
```

Then give each implementation task explicit edit authority:

```yaml
tasks:
  - id: implement-service-contract
    deps: [define-public-contract]
    component: service
    owns_paths:
      - internal/service/**
    reads_contracts:
      - internal/contracts/public-api.md

  - id: implement-web-contract
    deps: [define-public-contract]
    component: web
    owns_paths:
      - web/**
    reads_contracts:
      - internal/contracts/public-api.md
```

After `define-public-contract` is `done`, those two tasks are safe to batch because they write disjoint owned paths and only read the shared contract. Put the integration checkpoint after the independent work has been merged back:

```yaml
  - id: integration-checkpoint
    workflow: checkpoint
    deps:
      - implement-service-contract
      - implement-web-contract
    reads_contracts:
      - internal/contracts/public-api.md
    verify_commands:
      - go test ./...
      - npm test --prefix web
```

Use `vibedrive plan ready-batch` to inspect the decision before launching agents:

```bash
vibedrive plan ready-batch --limit 3
```

Example output after the contract task is complete:

```text
Ready batch (2/3):
  - implement-service-contract: Implement the service side of the public contract
  - implement-web-contract: Implement the web client side of the public contract
Not selected:
  - integration-checkpoint: unmet_dependencies deps=implement-service-contract,implement-web-contract
```

Serial fallback is deliberate. A ready task with no `owns_paths` and no component-owned paths can still run, but it will not be batched with another selected task because the runner cannot prove its write boundary. Ready tasks also remain serial when they list each other in `conflicts_with`, write overlapping `owns_paths`, or both write the same `provides_contracts` file. The inspection command reports those reasons as `missing_ownership_metadata`, `explicit_conflict`, `ownership_conflict`, or `contract_writer_conflict`.

### Project fields

| Field              | Meaning                                                                 |
| ------------------ | ----------------------------------------------------------------------- |
| `name`             | Short project name.                                                     |
| `objective`        | One-sentence statement of what the repository is trying to ship.        |
| `source_docs`      | Requirements/design inputs the plan was derived from.                   |
| `constraint_files` | Subset of source docs that define hard requirements, gates, or limits.  |
| `components`       | Optional component catalog with repo-relative ownership and contract metadata. |

### Component fields

| Field                 | Meaning                                                                 |
| --------------------- | ----------------------------------------------------------------------- |
| `id`                  | Required stable component ID. Tasks may refer to this value.            |
| `name`                | Optional human-readable component name.                                 |
| `owned_paths`         | Optional repo-relative path globs owned by this component.              |
| `reads_contracts`     | Optional repo-relative contract files this component consumes.          |
| `provides_contracts`  | Optional repo-relative contract files this component provides or owns.  |

### Task fields

| Field             | Meaning                                                                                 |
| ----------------- | --------------------------------------------------------------------------------------- |
| `id`              | Required stable task ID. Dependencies refer to this value.                              |
| `title`           | Required short human-readable task title.                                               |
| `details`         | Optional implementation notes or extra context.                                         |
| `status`          | Required execution state. See supported values below.                                   |
| `workflow`        | Optional workflow name from `vibedrive.yaml`. Falls back to `default_workflow`.      |
| `kind`            | Optional planning metadata. Stored in the plan, but not interpreted by the runner today. |
| `deps`            | Optional list of task IDs that must be `done` before this task is ready.                |
| `context_files`   | Optional repo-relative files the task should pay attention to.                          |
| `component`       | Optional component ID for this task. If a component catalog exists, the ID must be declared there. |
| `owns_paths`      | Optional repo-relative path globs the task is allowed to edit.                          |
| `reads_contracts` | Optional repo-relative contract files the task consumes.                                |
| `provides_contracts` | Optional repo-relative contract files the task creates, updates, or owns.            |
| `conflicts_with`  | Optional task IDs that explicitly conflict with this task for parallel execution. The serial runner does not use this field for ready-task selection. |
| `acceptance`      | Optional acceptance criteria for the task.                                              |
| `verify_commands` | Optional shell commands run by `task finalize` before a `done` task stays `done`; if those commands need new tooling, the plan should include that instrumentation work first. |
| `commit_message`  | Optional commit message used by the default finalizer workflow.                          |

### Task statuses

| Status         | Meaning                                                                                     |
| -------------- | ------------------------------------------------------------------------------------------- |
| `todo`         | Not started yet. Eligible once all dependencies are `done`.                                 |
| `in_progress`  | Partially complete. Ready tasks in this state are selected before `todo` tasks.             |
| `blocked`      | Terminal state for work that cannot continue without an external dependency or decision.     |
| `done`         | Terminal state for completed work.                                                           |
| `manual`       | Terminal state for manual or human-owned work. Supported by the finalizer and custom flows. |

### Top-level fields

| Field                    | Default       | Meaning                                                            |
| ------------------------ | ------------- | ------------------------------------------------------------------ |
| `workspace`              | `.`                   | Directory Claude, Codex, and exec steps run in. Relative `plan_file` resolves under it. |
| `plan_file`              | `vibedrive-plan.yaml` | Machine-readable task file the runner advances through.            |
| `max_iterations`         | `0`           | Hard cap on iterations. `0` means unlimited.                       |
| `max_stalled_iterations` | `2`           | Abort after this many no-progress iterations on the same item.     |
| `default_workflow`       | unset         | Workflow used when a plan task omits `workflow`.                   |
| `dry_run`                | `false`       | Render prompts and commands without running anything.              |
| `parallel`               | serial        | Optional block for isolated parallel task execution. Defaults keep execution serial until explicitly enabled. |

### `parallel` block

| Field             | Default                    | Meaning                                                                 |
| ----------------- | -------------------------- | ----------------------------------------------------------------------- |
| `enabled`         | `false`                    | Opts into parallel execution. When false, `max_parallelism` is ignored and the runner stays serial. |
| `max_parallelism` | `3`                        | Maximum number of safe ready tasks to run in one batch. Must be at least `1`. |
| `worktree_root`   | `.vibedrive/worktrees`     | Workspace-relative root for isolated git worktrees used by parallel workers. |
| `artifact_root`   | `.vibedrive/task-runs`     | Workspace-relative root for isolated task results, review artifacts, and task notes from parallel workers. |

The scaffold writes `parallel.enabled: false` and `max_parallelism: 3`, so existing plans still run exactly one task at a time until parallel execution is explicitly enabled. Setting `parallel.enabled: true` only allows batching after `vibedrive plan ready-batch` can prove the selected tasks have compatible boundaries. Parallel agent steps also require non-fullscreen transports; with the default `tui` transports, vibedrive warns and continues serially instead of trying to drive multiple fullscreen sessions at once.

### `claude` block

| Field              | Default      | Meaning                                                                 |
| ------------------ | ------------ | ----------------------------------------------------------------------- |
| `command`          | `claude`     | Executable to launch.                                                   |
| `args`             | `["--effort", "max", "--permission-mode", "bypassPermissions"]` | Extra CLI flags passed to Claude. If you set custom args without an explicit `--effort`, vibedrive appends `--effort max`. If you do not set a Claude permission flag, vibedrive appends `--permission-mode bypassPermissions` so agent steps do not stop on approval prompts. |
| `transport`        | `tui`        | `tui` drives the fullscreen UI inside a PTY. `print` uses `--print`.   |
| `startup_timeout`  | `30s`        | How long to wait for Claude to become ready before failing.             |
| `session_strategy` | `session_id` | `session_id` starts a new session per item; `continue` resumes.         |

### `codex` block

| Field             | Default                                                                 | Meaning                                                                 |
| ----------------- | ----------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| `command`         | `codex`                                                                 | Executable to launch.                                                   |
| `transport`       | `tui`                                                                   | `tui` drives Codex's native interactive UI inside a PTY. `exec` keeps the non-interactive runner flow. |
| `startup_timeout` | `30s`                                                                   | How long to wait for Codex to become ready in `tui` mode before failing. |
| `args`            | `["--dangerously-bypass-approvals-and-sandbox", "-c", "model_reasoning_effort=\"xhigh\""]` | Extra CLI flags passed to Codex before the rendered prompt.             |

vibedrive prepends `--dangerously-bypass-approvals-and-sandbox` to Codex invocations so the agent never pauses for approval prompts. If you set custom `codex.args` without an explicit `model_reasoning_effort=...` override, vibedrive appends `-c model_reasoning_effort="xhigh"`.

In `tui` mode, Codex runs the same fullscreen terminal UI you get from invoking `codex` yourself, and vibedrive reuses that PTY session across steps for the current item. In `exec` mode, vibedrive enables Codex's JSON event stream internally and renders a filtered terminal view: the runner prints the rendered step instructions first, then agent messages, command names, and file-change summaries stay visible, while command output, raw file reads, and diff bodies are suppressed. If you explicitly include `--json` in `codex.args`, vibedrive leaves the stream untouched.

### Step fields

| Field               | Applies to  | Meaning                                                           |
| ------------------- | ----------- | ----------------------------------------------------------------- |
| `name`              | all         | Required. Shown in logs.                                          |
| `type`              | all         | `claude` (default), `codex`, `agent`, or `exec`.                  |
| `actor`             | agent       | `coder` or `reviewer`. Resolved at runtime from `--coder` / `--reviewer`, defaulting to `codex` and `claude`. |
| `prompt`            | claude, codex, agent | Go template rendered and sent to the resolved agent.        |
| `command`           | exec        | Argv list to run. Each element is a Go template.                  |
| `working_dir`       | exec        | Defaults to `workspace`. Relative paths resolve from `workspace`. |
| `env`               | exec        | Extra env vars. Values are Go templates.                          |
| `fresh_session`     | agent-backed steps | Run this Claude- or Codex-backed step in a one-off TUI session instead of the shared item session. |
| `timeout`           | all         | Go duration (for example `10m`). No timeout by default.           |
| `continue_on_error` | all         | Log the failure and keep going instead of aborting.               |
| `disabled`          | all         | Skip the step.                                                    |

### Workflow fields

| Field   | Meaning                           |
| ------- | --------------------------------- |
| `steps` | Ordered list of steps to execute. |

### Template data

Prompts, `command`, `working_dir`, and `env` values are rendered with Go's `text/template`. Available fields:

- `{{ .Workspace }}`
- `{{ .PlanFile }}`
- `{{ .ConfigPath }}`
- `{{ .ExecutablePath }}`
- `{{ .Iteration }}`
- `{{ .SessionID }}`
- `{{ .TaskResultPath }}`
- `{{ .TaskNotesPath }}`
- `{{ .ReviewPath }}`
- `{{ .Plan }}` — parsed `vibedrive-plan.yaml`
- `{{ .Task }}` — selected plan task
- `{{ .Now }}` — current time

## Notes & gotchas

- The runner advances when the selected task changes status in `vibedrive-plan.yaml` or notes in `.vibedrive/task-notes.yaml`.
- `vibedrive-plan.yaml` is intended to be machine-owned state. The default workflow updates it through `vibedrive task finalize`.
- `vibedrive task finalize` writes task status back into `vibedrive-plan.yaml`, writes task notes to `.vibedrive/task-notes.yaml`, runs `verify_commands`, removes task artifacts, and commits staged changes when needed. It does not auto-insert follow-up tasks or enforce changed-file-count triggers.
- In the default scaffold, task-result notes are intended to capture what the coder learned in that phase so you can revise the plan and rerun it from a fresh environment.
- `vibedrive task finalize` accepts `done`, `in_progress`, `blocked`, and `manual` task results. The scaffolded prompts only instruct the implementation steps to emit the first three.
- `verify_commands` lets plan tasks declare deterministic checks for the exec finalizer to run before a task can stay `done`.
- `vibedrive plan ready-batch` loads the plan without launching agents and explains why ready tasks are or are not parallel-ready.
- If a task result says `done` and a `verify_commands` command fails, the finalizer rewrites the task to `in_progress`, appends a verification-failure note in `.vibedrive/task-notes.yaml`, removes the result file, and returns an error without committing.
- `vibedrive task finalize` also removes the default peer-review artifact for the task so it does not get staged into the commit.
- `required_outputs` lets a step declare files it must leave behind. The runner creates parent directories before the step runs and fails the step immediately if the files are still missing afterward.
- The finalizer stages changes with `git add -A` and only creates a commit when something is actually staged.
- Codex steps use native TUI mode by default, so the app now shows the same Codex interface you get from running `codex` directly.
- If you prefer the older non-interactive behavior, set `codex.transport: exec`. In that mode, vibedrive suppresses raw file-read and diff payloads but still shows the rest of Codex's progress.
- `--coder` and `--reviewer` are independent. You can set them to different agents or to the same agent.
- Agent role selection is runtime-only. Use `--coder` and `--reviewer` to override the defaults of coder=`codex` and reviewer=`claude`.
- `vibedrive init` uses the selected bootstrap author and critic and their configured transports. Defaults are author=`codex` and critic=`claude`. `--author claude` uses Claude for authoring, `--critic codex` uses Codex for critique, and each bootstrap phase uses a fresh instance even when both roles resolve to the same agent type.
- In TUI mode, YAML multiline prompts are flattened into one submitted message, because real newlines would be interpreted as separate messages by Claude's composer.
- In a fresh workspace, the runner auto-confirms Claude's trust dialog so the loop can start unattended.
- TUI automation detects "Claude is idle" from terminal-title transitions. If a future Claude release changes that behavior, the detector may need updating.
- `type: exec` lets you move deterministic steps (linters, formatters, arbitrary shell) out of Claude when that becomes cleaner.
- Plan string-list fields such as `acceptance`, `verify_commands`, and `context_files` must be YAML lists. Quote list items that contain `:` followed by a space.
