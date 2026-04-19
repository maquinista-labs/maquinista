# Runners

## What a runner is

A runner is the interactive AI process that lives inside a tmux window.
maquinista treats runners as interchangeable at the protocol level: it
sends keystrokes in, reads transcript output out.

Three runners are supported:

| Name | Binary | Resume flag | Soul injection |
|------|--------|-------------|----------------|
| `claude` | `claude` | `--resume <session_id>` | `--system-prompt "$(maquinista soul render <id>)"` |
| `openclaude` | `claude` | `--resume <session_id>` | same as claude |
| `opencode` | `opencode` | `--session <session_id>` | not supported |

The `runner_type` column on `agents` records which runner owns the window.
`cfg.DefaultRunner` is the system-wide default; individual agents may
override it.

## Launch command

`resolveRunnerCommand` (`cmd/maquinista/spawn_topic_agent.go`) builds the
shell command line for a given runner:

1. Fresh start, no soul → bare runner binary with `AGENT_ID` env var.
2. Fresh start, has soul → append `--system-prompt "$(maquinista soul render <id>)"`.
   The shell substitution runs at launch time so the soul is always
   current; no prompt file on disk.
3. Resume (`session_id` set) → append `--resume <session_id>` (or
   `--session` for opencode). Soul injection is skipped — the resumed
   session already carries the original system prompt in its history.

`AGENT_ID` and `RUNNER_TYPE` are always injected as env vars so the
SessionStart hook can identify the agent.

## Readiness detection

After `tmux.NewWindow`, `waitForRunnerReady` polls the pane for up to
15 s. Detection markers:

- Claude / OpenClaude: `❯` chevron at line start, or `"bypass permissions on"` text.
- OpenCode: `"Build "` status bar.

If the timeout elapses the spawn continues — the send-keys that delivers
the first inbox message may need a manual Enter, but the agent is usable.

## Transcript tailing

The monitor (`internal/monitor/`) polls each runner's transcript source
on a configurable interval. Each source knows how to find and tail the
JSONL output for its runner type:

- `ClaudeSource` — reads `.claude/projects/` JSONL files.
- `OpenCodeSource` — reads OpenCode session files.
- `OpenClaudeSource` — reads OpenClaude session files.

Transcript events with `role=assistant` and `contentType∈{text,thinking}`
are passed to `OutboxWriter`, which appends them to `agent_outbox`.

## Sidecar (future)

The long-term design replaces the monitor + mailbox_consumer pair with a
per-agent `SidecarRunner` (`internal/sidecar/sidecar.go`). The sidecar
owns both the inbox drive loop and the transcript tail for exactly one
agent, keeping `in_reply_to` causally correct. See
`plans/active/per-agent-sidecar.md`.

## TODO

- [ ] Document runner env overrides (EnvOverrides())
- [ ] Document OpenCode session discovery
- [ ] Sidecar wiring once per-agent-sidecar.md is implemented
- [ ] Pi / embedded runner integration (plans/active/pi-integration.md)
