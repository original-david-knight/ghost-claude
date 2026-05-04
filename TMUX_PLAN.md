# Tmux TUI Parallel Agent Plan

This work will be done directly in the repo, not through Vibedrive/vibecode orchestration.

## Goal

Vibedrive should always run Claude and Codex through their real TUI flows. Parallel task execution should run multiple TUIs concurrently by multiplexing them through tmux, rather than introducing or relying on non-TUI agent modes.

## Current Problems

- `internal/runner/runner.go` currently falls back to serial execution when a configured agent uses fullscreen TUI.
- `internal/config/config.go` still accepts non-TUI transports: `claude.transport: print` and `codex.transport: exec`.
- `pkg/agentcli/codex/codex.go` can fall back from Codex TUI automation to `codex exec`.
- `README.md` documents non-TUI transports and says parallel agent steps require non-fullscreen transports.

## Working Assumptions

- TUI is the only supported agent transport.
- tmux is required for parallel TUI execution.
- Serial execution can keep the existing direct PTY behavior unless we decide tmux should be used everywhere.
- Parallel workers should keep the existing isolated worktree/artifact model.
- Existing configs that request non-TUI transports should fail clearly rather than silently changing behavior.

## Plan

### 1. Make TUI the only supported transport

- Remove `claude.transport: print` from the supported config surface.
- Remove `codex.transport: exec` from the supported config surface.
- Stop inferring Codex exec transport from `codex.args`.
- Reject Codex args whose subcommand is non-interactive, such as `exec` or `review`, when they would be used for agent steps.
- Keep config errors explicit so users know to remove non-TUI transport settings.
- Update config tests that currently expect exec or print behavior.

### 2. Remove Codex exec fallback

- Remove `Session.ExecFallback`.
- Remove `fallbackFromTUIToExec`.
- If Codex TUI does not acknowledge a submitted prompt, return a clear TUI automation error.
- Update or delete tests that assert fallback to `codex exec`.
- Keep the prompt acknowledgement retry behavior, but make failure terminal for that step.

### 3. Add a tmux-backed TUI backend

- Add a small tmux abstraction that can:
  - create a named tmux session for a Vibedrive run
  - create one window or pane per agent session
  - launch the agent command in the correct workspace
  - send pasted prompts safely
  - send Enter and exit sequences
  - capture pane output for diagnostics
  - read title/output state needed by the existing idle/busy monitors
- Use deterministic names that include the process id, task id, and agent role where possible.
- Surface the tmux attach command when a parallel run starts.

### 4. Wire parallel workers through tmux

- Remove the `fullscreenParallelAgent()` serial fallback.
- When `parallel.enabled: true` and effective parallelism is greater than one, require tmux before launching workers.
- Give each parallel task its own TUI session in tmux.
- Preserve the current per-task isolated workspace, plan snapshot, task result, review artifact, and task notes paths.
- Ensure worker output does not corrupt the parent terminal.
- Keep deterministic integration order after workers finish.

### 5. Preserve serial behavior

- Keep serial runs on the current direct PTY path unless we intentionally decide to standardize on tmux.
- Keep shared per-item sessions and `fresh_session: true` semantics.
- Keep startup timeout, prompt submission, idle detection, trust prompt handling, and close behavior consistent with current TUI behavior.

### 6. Update docs and examples

- Remove docs for `claude.transport: print`.
- Remove docs for `codex.transport: exec`.
- Remove the statement that parallel execution requires non-fullscreen transports.
- Document tmux as the required multiplexer for parallel TUI execution.
- Document how to attach to the tmux session and inspect running agent windows.
- Keep examples showing `transport: tui`, or remove the transport field entirely if it becomes redundant.

### 7. Tests

- Config tests:
  - default Claude and Codex transports are TUI
  - non-TUI transport settings are rejected
  - non-interactive Codex subcommands are rejected
- Codex tests:
  - failed TUI submit returns an error
  - no exec fallback occurs
- Runner tests:
  - parallel TUI configuration no longer falls back to serial
  - missing tmux fails parallel execution clearly
  - serial execution still uses the existing TUI session behavior
- tmux backend tests:
  - command construction
  - session/window naming
  - prompt send escaping
  - close command behavior
  - optional integration test gated on `tmux` being installed

## Open Decisions

- Whether serial runs should also use tmux for consistency, or keep direct PTY for minimal change.
- Whether parallel runs should automatically attach the user to tmux or only print the attach command.
- Exact tmux layout: one window per agent session, panes grouped per task, or one window per task with panes for coder/reviewer.
