# ghost-claude

`ghost-claude` is a terminal-native Go runner that drives Claude Code and Codex through a configurable workflow.

Its primary execution queue is `ghost-plan.yaml`: a machine-readable task graph that the runner reads, updates, and advances automatically.

Legacy TODO mode still exists for compatibility, but the current scaffolded flow does not step through `TODO.md` unless you intentionally omit `plan_file` and configure checklist-based steps.

It launches Claude's real fullscreen TUI inside a PTY when Claude-backed steps run, and it can also invoke Codex non-interactively. The scaffolded workflow uses stable coder/reviewer steps, and you can flip either role at run time.

## Requirements

- Go 1.26+ (the version declared in `go.mod` is currently `1.26.0`)
- The `claude` CLI installed and on your `$PATH` ([Claude Code](https://docs.claude.com/en/docs/claude-code))
- The `codex` CLI installed and on your `$PATH` for the default scaffolded implementation flow
- A `ghost-plan.yaml` file with machine-readable tasks for normal operation
- Optionally, a `TODO.md` file with GitHub-style checkboxes if you intentionally want legacy checklist mode or want to use it as a planning input

## Install

```bash
go install ./cmd/ghost-claude
```

Or run without installing:

```bash
go run ./cmd/ghost-claude <subcommand>
```

## Quick start

From inside the repo you want ghost-claude to work on:

```bash
ghost-claude init              # writes ghost-claude.yaml, uses all top-level regular files in the workspace dir as source, then asks Claude to generate ghost-plan.yaml and review it
ghost-claude init DESIGN.md    # or point init at a specific source file or directory
ghost-claude run    # starts the loop with coder=codex and reviewer=claude
ghost-claude run --coder claude --reviewer codex   # flips roles at run time without changing ghost-plan.yaml
ghost-claude run --coder codex --reviewer codex    # same agent can both code and review
```

Target a different repo without `cd`:

```bash
ghost-claude init --workspace /path/to/repo
ghost-claude run  --workspace /path/to/repo
```

Preview what would happen without touching anything:

```bash
ghost-claude run --dry-run
```

`ghost-claude init` bootstraps plan mode. The generated config points the runner at `ghost-plan.yaml`, not at `TODO.md`.

## How the loop works

For each iteration:

1. Select the next work item.
   In the default and current flow, this is the first ready task in `ghost-plan.yaml`, with `in_progress` tasks preferred over `todo` tasks and dependencies respected.
   Legacy TODO mode instead uses the first unchecked `- [ ]` item in `TODO.md`.
2. Start a fresh Claude session when a Claude step needs one.
3. Run every configured step in order. Claude steps share one session for the work item by default, but `fresh_session: true` isolates a Claude step in its own session. Codex steps run non-interactively.
4. Close any Claude sessions that were opened.
5. Re-read the queue state. If the selected item changed state, advance. If not, count a stall and retry.

The runner stops when there is no work left, when `max_iterations` is reached, or when the same item stalls `max_stalled_iterations` times in a row.

The default workflow scaffolded by `ghost-claude init` is plan-oriented and uses `ghost-plan.yaml` as the execution queue:

1. Execute the selected task with the current coder while preserving the plan's hard constraints.
2. Ask the current reviewer to review the changes and write a structured review artifact.
3. Run a second coder step that reads the review artifact and fixes any actionable findings.
4. Run the task's configured `verify_commands`, apply the JSON task result to `ghost-plan.yaml`, and commit the iteration with an exec step.

During `init`, ghost-claude bootstraps plan mode in two phases:

1. Write `ghost-claude.yaml`.
2. Ask Claude to read the provided source file or directory, or all regular files in the workspace directory when no source is provided, then generate `ghost-plan.yaml`, review it critically, and revise the plan.

## Subcommands

```
ghost-claude run  [-config PATH] [-workspace DIR] [-dry-run] [-coder claude|codex] [-reviewer claude|codex]
ghost-claude init [-config PATH] [-workspace DIR] [-source PATH] [-force] [SOURCE]
ghost-claude task finalize --workspace DIR --plan PATH --task TASK_ID --result PATH [--message MSG]
ghost-claude help
```

If you omit the subcommand, `ghost-claude` behaves like `run`.

When `-workspace` is set, the default config path becomes `<workspace>/ghost-claude.yaml` and every relative path in the config resolves against that workspace.

In the generated config, `plan_file` is set, so `run` executes from `ghost-plan.yaml`. It does not read `TODO.md` as the task queue unless you reconfigure it into legacy TODO mode.

## Config

The runner reads `ghost-claude.yaml` by default. `ghost-claude init` writes a complete scaffold matching [`ghost-claude.example.yaml`](ghost-claude.example.yaml). Shortened plan-mode example:

```yaml
workspace: .
plan_file: ghost-plan.yaml
max_iterations: 0
max_stalled_iterations: 2
default_workflow: implement

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
  args:
    - --dangerously-bypass-approvals-and-sandbox
    - exec
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

      - name: peer-review
        type: agent
        actor: reviewer
        prompt: |
          Review the current uncommitted changes for task {{ .Task.ID }} from {{ .PlanFile }}.

      - name: address-peer-review
        type: agent
        actor: coder
        prompt: |
          Read the peer review artifact at {{ .ReviewPath }} for task {{ .Task.ID }} from {{ .PlanFile }}.

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

Legacy TODO mode still works, but it is no longer the default or recommended flow: omit `plan_file` and `workflows`, keep `steps`, and drive progress by editing the first unchecked checkbox in `TODO.md`.

### `ghost-plan.yaml`

Plan mode uses a machine-readable task file. The repository ships a complete starter in [`ghost-plan.example.yaml`](ghost-plan.example.yaml). Minimal example:

```yaml
project:
  name: planet-v1
  objective: Ship Planet v1 end-to-end in this repository.
  source_docs:
    - DESIGN.md
    - TEST_PLAN.md
  constraint_files:
    - DESIGN.md

tasks:
  - id: scaffold
    title: Scaffold the repo and get `go build ./...` green
    workflow: implement
    status: todo
    details: Set up the initial layout and keep the repo buildable after each change.
    context_files:
      - DESIGN.md
    acceptance:
      - `go build ./...` succeeds
      - fast test script exists and runs
    verify_commands:
      - go build ./...
    commit_message: feat: scaffold the repository

  - id: scaffold-checkpoint
    title: Run the first full-suite checkpoint and fix regressions
    workflow: checkpoint
    status: todo
    deps:
      - scaffold
    verify_commands:
      - go test ./...
```

The intended use is:

- `ghost-plan.yaml` is machine-owned execution state
- the runner normally advances by updating task status and notes in `ghost-plan.yaml`, not by checking boxes in `TODO.md`
- `TODO.md` is still useful when you want legacy checklist mode or want to keep one constraints doc
- `ghost-claude init` can generate the initial plan from a TODO file, a design doc, or a directory of source files
- your external planner can still generate both files if you prefer that flow

### Project fields

| Field              | Meaning                                                                 |
| ------------------ | ----------------------------------------------------------------------- |
| `name`             | Short project name.                                                     |
| `objective`        | One-sentence statement of what the repository is trying to ship.        |
| `source_docs`      | Requirements/design inputs the plan was derived from.                   |
| `constraint_files` | Subset of source docs that define hard requirements, gates, or limits.  |

### Task fields

| Field             | Meaning                                                                                 |
| ----------------- | --------------------------------------------------------------------------------------- |
| `id`              | Required stable task ID. Dependencies refer to this value.                              |
| `title`           | Required short human-readable task title.                                               |
| `details`         | Optional implementation notes or extra context.                                         |
| `status`          | Required execution state. See supported values below.                                   |
| `workflow`        | Optional workflow name from `ghost-claude.yaml`. Falls back to `default_workflow`.      |
| `kind`            | Optional planner metadata. Stored in the plan, but not interpreted by the runner today. |
| `deps`            | Optional list of task IDs that must be `done` before this task is ready.                |
| `context_files`   | Optional repo-relative files the task should pay attention to.                          |
| `acceptance`      | Optional acceptance criteria for the task.                                              |
| `verify_commands` | Optional shell commands run by `task finalize` before a `done` task stays `done`.       |
| `commit_message`  | Optional commit message used by the default finalizer workflow.                          |
| `notes`           | Optional execution notes. Plan-mode progress is tracked from `status` plus `notes`.     |

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
| `workspace`              | `.`           | Directory Claude, Codex, and exec steps run in. Relative `todo_file` and `plan_file` resolve under it. |
| `todo_file`              | `TODO.md`     | Legacy checklist file. Also useful as a constraints source.        |
| `plan_file`              | unset         | Machine-readable task file for plan mode.                          |
| `max_iterations`         | `0`           | Hard cap on iterations. `0` means unlimited.                       |
| `max_stalled_iterations` | `2`           | Abort after this many no-progress iterations on the same item.     |
| `default_workflow`       | unset         | Workflow used when a plan task omits `workflow`.                   |
| `dry_run`                | `false`       | Render prompts and commands without running anything.              |

### `claude` block

| Field              | Default      | Meaning                                                                 |
| ------------------ | ------------ | ----------------------------------------------------------------------- |
| `command`          | `claude`     | Executable to launch.                                                   |
| `args`             | `["--effort", "max"]` | Extra CLI flags passed to Claude. If you set custom args without an explicit `--effort`, ghost-claude appends `--effort max`. |
| `transport`        | `tui`        | `tui` drives the fullscreen UI inside a PTY. `print` uses `--print`.   |
| `startup_timeout`  | `30s`        | How long to wait for Claude to become ready before failing.             |
| `session_strategy` | `session_id` | `session_id` starts a new session per item; `continue` resumes.         |

### `codex` block

| Field     | Default                                           | Meaning                                                  |
| --------- | ------------------------------------------------- | -------------------------------------------------------- |
| `command` | `codex`                                           | Executable to launch.                                    |
| `args`    | `["--dangerously-bypass-approvals-and-sandbox", "exec", "-c", "model_reasoning_effort=\"xhigh\""]` | Extra CLI flags passed to Codex before the rendered prompt. |

ghost-claude prepends `--dangerously-bypass-approvals-and-sandbox` to Codex invocations so the agent never pauses for approval prompts. If you set custom `codex.args` without an explicit `model_reasoning_effort=...` override, ghost-claude appends `-c model_reasoning_effort="xhigh"`.

When the Codex subcommand is `exec`, ghost-claude enables Codex's JSON event stream internally and renders a filtered terminal view: agent messages, command names, and file-change summaries stay visible, while command output, raw file reads, and diff bodies are suppressed. If you explicitly include `--json` in `codex.args`, ghost-claude leaves the stream untouched.

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
| `fresh_session`     | claude-backed steps | Run this Claude-backed step in a one-off session instead of the shared item session. |
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
- `{{ .TodoFile }}`
- `{{ .PlanFile }}`
- `{{ .ConfigPath }}`
- `{{ .ExecutablePath }}`
- `{{ .Iteration }}`
- `{{ .SessionID }}`
- `{{ .TaskResultPath }}`
- `{{ .ReviewPath }}`
- `{{ .NextTodo.Line }}` — 1-indexed line number in `TODO.md`
- `{{ .NextTodo.Raw }}` — the entire line, including the checkbox
- `{{ .NextTodo.Text }}` — just the task description
- `{{ .Plan }}` — parsed `ghost-plan.yaml`
- `{{ .Task }}` — selected plan task in plan mode
- `{{ .Now }}` — current time

## Notes & gotchas

- In TODO mode, the runner only advances when the first incomplete checkbox in the TODO file changes.
- In the normal plan-based flow, the runner advances when the selected task changes status or notes in `ghost-plan.yaml`.
- The generated config from `ghost-claude init` runs in plan mode, so `TODO.md` is not the live execution queue unless you deliberately switch back to legacy TODO mode.
- `ghost-plan.yaml` is intended to be machine-owned state. The default workflow updates it through `ghost-claude task finalize`.
- `ghost-claude task finalize` accepts `done`, `in_progress`, `blocked`, and `manual` task results. The scaffolded prompts only instruct the implementation steps to emit the first three.
- `verify_commands` lets plan tasks declare deterministic checks for the exec finalizer to run before a task can stay `done`.
- If a task result says `done` and a `verify_commands` command fails, the finalizer rewrites the task to `in_progress`, appends a verification-failure note, removes the result file, and returns an error without committing.
- `ghost-claude task finalize` also removes the default peer-review artifact for the task so it does not get staged into the commit.
- The finalizer stages changes with `git add -A` and only creates a commit when something is actually staged.
- Codex steps are non-interactive by default because the scaffold uses `codex exec`. In that mode, ghost-claude suppresses raw file-read and diff payloads but still shows the rest of Codex's progress. If you want a different Codex mode, change the `codex.args` block.
- `--coder` and `--reviewer` are independent. You can set them to different agents or to the same agent.
- Agent role selection is runtime-only. Use `--coder` and `--reviewer` to override the defaults of coder=`codex` and reviewer=`claude`.
- `ghost-claude init` uses the configured Claude transport. With the default `tui` transport, you can watch Claude generate and review the plan live in your terminal.
- In TUI mode, YAML multiline prompts are flattened into one submitted message, because real newlines would be interpreted as separate messages by Claude's composer.
- In a fresh workspace, the runner auto-confirms Claude's trust dialog so the loop can start unattended.
- TUI automation detects "Claude is idle" from terminal-title transitions. If a future Claude release changes that behavior, the detector may need updating.
- `type: exec` lets you move deterministic steps (linters, formatters, arbitrary shell) out of Claude when that becomes cleaner.
- Plan string-list fields such as `acceptance`, `verify_commands`, and `context_files` must be YAML lists. Quote list items that contain `:` followed by a space.
