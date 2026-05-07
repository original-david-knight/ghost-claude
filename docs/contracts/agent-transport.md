# Agent Transport Contract

This contract defines the supported `vibedrive.yaml` agent transport values and
the observable behavior future agent client implementations must provide.
This document is a schema and scaffold contract; accepting a transport value in
config does not imply that the runner already implements the transport.

## Config Values

`claude.transport` supports:

| Value | Mode | Intended use |
| --- | --- | --- |
| `tui` | Fullscreen Claude TUI in tmux | Human-watchable sessions, trust prompts, manual intervention, session reuse. |
| `print` | Non-interactive `claude --print` child process | Orchestrated prompt-in/result-out steps that should finish without a watched terminal. |

`codex.transport` supports:

| Value | Mode | Intended use |
| --- | --- | --- |
| `tui` | Fullscreen Codex TUI in tmux | Human-watchable sessions, trust prompts, manual intervention, session reuse. |
| `exec` | Non-interactive `codex exec` child process | Orchestrated prompt-in/result-out steps that should finish without a watched terminal. |

Unknown values must fail config validation with an error naming the unsupported
field and value. For `codex.transport: exec`, `codex.args` are additional CLI
flags/options; the transport selects the Codex `exec` subcommand, so
`codex.args` must not contain Codex subcommands. Interactive Codex subcommands
such as `resume` remain TUI-only.

## Prompt Delivery

### TUI Transports

For `transport: tui`, Vibedrive starts or reuses a tmux-backed fullscreen agent
session. The rendered prompt is delivered to the terminal session through the
TUI input path, equivalent to pasting the prompt into the pane and submitting it.
The session may persist across multiple steps unless the step requests
`fresh_session: true` or the runner creates a new workflow session.

Claude TUI steps may receive `{{ .SessionID }}` in their rendered prompt when a
Claude session strategy provides one. Codex TUI steps do not receive a session
identifier today.

### `claude.transport: print`

The client must launch one Claude process per prompt using the configured
`claude.command`, configured `claude.args`, and the transport-selected `--print`
mode. The full rendered prompt must be passed to the child process on stdin as
bytes, and stdin must be closed after the prompt is written. The prompt must not
be shell-interpolated or split through a shell command string.

No reusable Claude session is created for `print`. `{{ .SessionID }}` is empty
and prompts that require a session ID are incompatible with this transport.
`claude.session_strategy` applies only to `tui`.

### `codex.transport: exec`

The client must launch one Codex process per prompt using the configured
`codex.command`, configured `codex.args`, and the transport-selected `exec`
mode. The full rendered prompt must be passed to the child process on stdin as
bytes, and stdin must be closed after the prompt is written. The prompt must not
be shell-interpolated or split through a shell command string.

No reusable Codex session is created for `exec`. `fresh_session: true` has no
additional effect beyond the one-process-per-prompt behavior.

## Output And Completion

### TUI Transports

For TUI transports, the agent's visible state is read from the tmux pane and
terminal title state machine. The agent step completes when the TUI client
detects that the submitted prompt has finished and the session is ready for the
next prompt. The tmux pane output, title transitions, rendered prompt payload,
and parent process stdout/stderr are diagnostic artifacts, not the primary
success signal.

Closing a TUI session should close the tmux-backed process. Startup timeout,
submit timeout, unexpected process exit, or an unrecognized terminal state must
return a step failure.

### Non-Interactive Transports

For `print` and `exec`, stdout and stderr from the child process must be streamed
to the runner's corresponding output writers while also being available to
diagnostics on failure. Stdout is the agent's response stream. Stderr is CLI
diagnostic output. Required output files and task result/review artifacts remain
the authoritative workflow products.

The step completes when the child process exits with status 0. A non-zero exit,
startup error, stdin write error, context timeout, or signal termination is a
step failure. On timeout, the implementation must terminate the child process
and return a failure that identifies the transport and step.

## Step Compatibility

`type: exec` steps do not use agent transports.

`type: claude` steps use `claude.transport`. `type: codex` steps use
`codex.transport`. `type: agent` steps resolve `actor: coder` or
`actor: reviewer` to the configured runtime agent and then use that agent's
transport.

TUI transports are compatible with all agent step types and remain required for:

- steps that need a human-watchable terminal session;
- login, trust, permission, or other interactive prompts;
- prompts that depend on a persistent terminal state;
- prompts that require `{{ .SessionID }}`.

Non-interactive transports are compatible with orchestrated agent steps that can
be expressed as a single rendered prompt and verified through process exit,
stdout/stderr, task notes, review JSON, or required output files. The default
implement workflow is expected to move these steps to non-interactive transports
after the transports are implemented:

- `execute-task`: `codex.transport: exec`;
- `peer-review`: `claude.transport: print`;
- `address-peer-review`: `codex.transport: exec`;
- checkpoint agent steps with the same coder/reviewer mapping.

`finalize-task` remains a `type: exec` step and is unaffected by agent
transport selection.
