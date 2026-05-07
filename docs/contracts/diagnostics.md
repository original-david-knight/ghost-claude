# Failure Diagnostics Capture Contract

This contract defines the diagnostics bundle Vibedrive writes whenever a step
fails after it has enough run, task, and step context to identify the failure.
The bundle is intended for local debugging and for deterministic regression
tests; callers must not depend on human-readable error strings for the full
diagnostic payload.

## Debug Directory

Each failed step writes under the run debug root:

```text
.vibedrive/debug/<run-id>/<task-id>/<step-name>/
  manifest.json
  parent/
    stdout-tail.txt
    stderr-tail.txt
  prompt/
    raw.bin
    normalized.txt
    metadata.json
  tmux/
    pane.txt
    title-history.jsonl
    metadata.json
  exec/
    command.json
    stdout-tail.txt
    stderr-tail.txt
    combined-tail.txt
```

`<run-id>`, `<task-id>`, and `<step-name>` are filesystem-safe path segments
derived from the raw run state ID, task ID, and step name. The raw values must
be recorded in `manifest.json`. Path segment encoding must reject `..`, path
separators, empty segments, and absolute paths. If sanitizing changes a value,
append a short stable hash so distinct raw IDs do not collide.

Only artifacts that apply to the failed transport are required to be written.
Artifacts that do not apply, cannot be collected, or fail to write must still be
listed in `manifest.json` with `status` set to `not_applicable`, `unavailable`,
or `failed`. A diagnostics write failure must not replace the original step
failure; the original error remains the primary error and the capture failure is
reported as a warning or secondary detail.

All artifact paths in the manifest are relative to the step diagnostics
directory. Writers must create files atomically by writing a temporary file in
the same directory and renaming it into place.

## Size Budgets

Diagnostics collection must be bounded and tail-preferred for output streams so
capture cannot become another large-output failure.

| Artifact kind | Required bound | Truncation policy |
| --- | ---: | --- |
| Tmux pane capture | `3000` lines from `tmux capture-pane -p -S -3000`, then max `1 MiB` bytes | Preserve the tail if byte trimming is needed. |
| Tmux title history | `512` events, max `512` bytes per title | Preserve the most recent events; truncate long titles with a marker. |
| Prompt raw bytes | `256 KiB` | Preserve first `128 KiB` and last `128 KiB` with an elision marker. |
| Prompt normalized text | `256 KiB` | Same as prompt raw bytes after UTF-8 replacement and line-ending normalization. |
| Parent stdout tail | `64 KiB` | Preserve the tail. |
| Parent stderr tail | `64 KiB` | Preserve the tail. |
| Exec stdout tail | `256 KiB` | Preserve the tail. |
| Exec stderr tail | `256 KiB` | Preserve the tail. |
| Exec combined output tail | `256 KiB` | Preserve the tail in observed write order. |
| JSON metadata files | `128 KiB` each | Omit optional large fields and mark them in the manifest. |

When any artifact is truncated, the manifest entry must include
`truncated: true`, `original_bytes` or `original_events` when known, the applied
limit, and a SHA-256 digest of the full input when the full input was available
without unbounded buffering. Stream outputs must use bounded ring buffers rather
than collecting full output to compute a digest.

## Manifest Schema

`manifest.json` is UTF-8 JSON with the following top-level shape:

```json
{
  "schema_version": "diagnostics.v1",
  "created_at": "2026-05-07T16:00:00Z",
  "run_id": "20260507-abc123",
  "task_id": "define-diagnostics-contract",
  "step_name": "implement",
  "path_segments": {
    "run": "20260507-abc123",
    "task": "define-diagnostics-contract",
    "step": "implement"
  },
  "failure": {
    "path": "exec_step_non_zero_exit",
    "message": "run command: exit status 1",
    "captured_at": "2026-05-07T16:00:01Z"
  },
  "transport": {
    "kind": "exec",
    "agent": "",
    "interactive": false
  },
  "artifacts": [
    {
      "kind": "exec_combined_tail",
      "path": "exec/combined-tail.txt",
      "media_type": "text/plain; charset=utf-8",
      "status": "written",
      "required": true,
      "bytes": 8192,
      "truncated": false,
      "limit_bytes": 262144
    }
  ]
}
```

Required top-level fields:

| Field | Meaning |
| --- | --- |
| `schema_version` | Must be `diagnostics.v1` for this contract. |
| `created_at` | RFC 3339 timestamp for manifest creation. |
| `run_id`, `task_id`, `step_name` | Raw identity values. |
| `path_segments` | The actual directory segment values used on disk. |
| `failure.path` | Stable machine-readable failure path name. |
| `failure.message` | Short original error string, bounded to `4096` bytes. |
| `failure.captured_at` | RFC 3339 timestamp for the capture trigger. |
| `transport.kind` | `tmux`, `exec`, `codex_exec`, `claude_print`, or another future transport name. |
| `transport.agent` | `codex`, `claude`, or empty for non-agent exec steps. |
| `transport.interactive` | Whether the transport uses an interactive terminal. |
| `artifacts` | One entry per expected artifact, including missing or inapplicable artifacts. |

Artifact entries must include:

| Field | Meaning |
| --- | --- |
| `kind` | Stable artifact kind, such as `tmux_pane`, `prompt_raw`, or `exec_stderr_tail`. |
| `path` | Relative path within the step diagnostics directory. |
| `media_type` | Content type. Use `application/json`, `application/octet-stream`, or text with charset. |
| `status` | `written`, `not_applicable`, `unavailable`, or `failed`. |
| `required` | Whether this artifact is required for the active transport and failure path. |
| `bytes` | Bytes written, when status is `written`. |
| `truncated` | Whether content was truncated. |
| `limit_bytes`, `limit_lines`, or `limit_events` | The budget that applied. |

Optional artifact fields include `sha256`, `original_bytes`, `original_lines`,
`original_events`, `error`, and `reason`.

## Required Tmux Failure Artifacts

Tmux-backed failures include Codex and Claude TUI sessions.

Required artifacts:

| Path | Kind | Contents |
| --- | --- | --- |
| `tmux/pane.txt` | `tmux_pane` | Output from `tmux capture-pane -p -S -3000`, bounded as specified above. |
| `tmux/title-history.jsonl` | `tmux_title_history` | Timestamped terminal-title and screen-state transition events. |
| `tmux/metadata.json` | `tmux_metadata` | Pane target, agent, command, args, workdir, startup timeout, current transition counters, trust prompt count, and final classified state when known. |
| `prompt/raw.bin` | `prompt_raw` | Exact byte payload pasted into the TUI. For bracketed paste transports this includes bracketed paste control wrappers. |
| `prompt/normalized.txt` | `prompt_normalized` | Rendered prompt after Vibedrive prompt normalization, with bracketed paste wrappers removed. |
| `prompt/metadata.json` | `prompt_metadata` | Prompt byte counts, normalization mode, whether bracketed paste was used, and truncation details. |
| `parent/stdout-tail.txt` | `parent_stdout_tail` | Tail of Vibedrive parent process stdout as observed by the diagnostics package. |
| `parent/stderr-tail.txt` | `parent_stderr_tail` | Tail of Vibedrive parent process stderr as observed by the diagnostics package. |
| `manifest.json` | `manifest` | Manifest described above. |

`tmux/title-history.jsonl` is newline-delimited JSON. Each event must be bounded
and should have this shape:

```json
{"ts":"2026-05-07T16:00:00.100Z","source":"title","title":"busy vibedrive","state":"busy","idle_transitions":1,"busy_transitions":2,"trust_prompts":0}
```

`source` may be `title`, `screen`, `trust_prompt`, or `transport`. `state` may
be `idle`, `busy`, `unknown`, `trust_prompt`, or a future transport-specific
state. For terminal titles that cannot be read, record an `unavailable` artifact
entry in the manifest and include the title read error in `tmux/metadata.json`
when possible.

## Required Exec Failure Artifacts

Exec failures cover Vibedrive `type: exec` steps and future non-TUI agent
transports such as `codex exec` and `claude --print`.

Required artifacts for a Vibedrive `type: exec` non-zero exit:

| Path | Kind | Contents |
| --- | --- | --- |
| `exec/command.json` | `exec_command` | Rendered argv, working directory, exit status if known, timeout/cancellation flag, and bounded environment metadata. |
| `exec/stdout-tail.txt` | `exec_stdout_tail` | Tail of child stdout. |
| `exec/stderr-tail.txt` | `exec_stderr_tail` | Tail of child stderr. |
| `exec/combined-tail.txt` | `exec_combined_tail` | Tail of child stdout and stderr in observed write order. |
| `parent/stdout-tail.txt` | `parent_stdout_tail` | Tail of Vibedrive parent process stdout. |
| `parent/stderr-tail.txt` | `parent_stderr_tail` | Tail of Vibedrive parent process stderr. |
| `manifest.json` | `manifest` | Manifest described above. |

For a plain `type: exec` step, prompt artifacts are `not_applicable` unless the
step later gains a prompt-like input. For future non-TUI agent transports,
`prompt/raw.bin`, `prompt/normalized.txt`, and `prompt/metadata.json` are
required and describe the prompt sent to the command, even though no tmux
artifacts are present.

`exec/command.json` must be bounded and should have this shape:

```json
{
  "argv": ["sh", "-c", "go test ./..."],
  "working_dir": "/workspace/project",
  "exit_code": 1,
  "signal": "",
  "timed_out": false,
  "env": {
    "step": {"GOFLAGS": "-count=1"},
    "inherited_keys": ["HOME", "PATH", "SHELL"]
  },
  "redaction": {
    "sensitive_name_patterns": ["TOKEN", "PASSWORD", "SECRET", "KEY", "CREDENTIAL"],
    "values_redacted": true
  }
}
```

The diagnostics package must not dump the full inherited environment with
values by default. It records step-rendered environment values after applying
sensitive-name redaction and records inherited environment keys without values.

## Caller Obligations

The following existing failure paths must call the diagnostics package before
returning the final step error:

| Failure path | Stable `failure.path` | Required transport context |
| --- | --- | --- |
| Tmux submit-prompt timeout in Codex or Claude TUI clients, including no busy transition after the submit attempts | `tmux_submit_prompt_timeout` | Tmux pane, title history, rendered prompt payload, transition counters, trust prompt count. |
| `Runner.ensureRequiredOutputsAfterStep` final failure, including non-agent required-output failure and agent repair failure that still leaves missing or invalid outputs | `ensure_required_outputs_after_step` | Step identity, target agent or exec marker, required output paths, inspection issues, and the latest transport diagnostics available for that step. |
| Exec step non-zero exit or signal from `Runner.runStep` for `type: exec` | `exec_step_non_zero_exit` | Rendered argv, working directory, bounded child output tails, environment metadata, timeout/cancellation status. |
| `Runner.validateTaskNotesAfterAgentStep` final failure, including failed repair prompt or task notes still invalid after repair | `validate_task_notes_after_agent_step` | Task notes path, YAML validation error, repair prompt payload when rendered, and latest transport diagnostics available for that step. |

Tmux startup timeout and unexpected TUI exit while a step is running must also
capture a tmux diagnostics bundle, using `tmux_startup_timeout` and
`tmux_unexpected_exit` respectively. These names are reserved so later tmux
wiring can report them consistently.

Callers must pass enough identity to the diagnostics package to derive the debug
directory. If a lower-level transport detects the failure before it knows the
task or step identity, the runner must either pass that identity into the
transport wrapper or perform the capture at the runner boundary with the
transport state attached.

## Privacy And Portability

Diagnostics are local artifacts under `.vibedrive/debug` and must be treated as
potentially sensitive. Prompt captures may contain source code, issue text,
credentials pasted by a user, and model responses. Output tails may contain test
data or stack traces. Future upload or sharing features must require explicit
operator action.

The manifest schema is transport-agnostic. Tmux transports populate `tmux/*`.
Process transports populate `exec/*`. Agent process transports additionally
populate `prompt/*`. New transports may add directories, but they must keep the
same identity, manifest, artifact status, and size-budget semantics.
