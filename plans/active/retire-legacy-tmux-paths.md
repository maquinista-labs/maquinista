# Retire legacy tmux dispatch + `internal/queue/`

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

`archive/maquinista-v2-implementation.md` Task 1.9 promised to delete:

1. `internal/queue/queue.go` — the per-user Telegram send queue.
2. `state.ThreadBindings` in-memory map + `session_map.json`
   reader/writer.
3. Direct `tmux.SendKeysWithDelay` calls from `internal/bot/handlers.go`.
4. The in-process mailbox bridge from Task 1.6.

Status after the per-topic-agent pivot + §0 sprint:

- ✅ `session_map.json` readers/writers — retired (see
  `active/json-state-migration.md` Phase A).
- ✅ `state.ThreadBindings` — DB-backed under the `SetPool` path
  (Phase B1). The in-memory map is now a fallback for nil-pool tests.
- ✅ Direct `tmux.SendKeysWithDelay` from `internal/bot/handlers.go`
  for the *inbound-message* path — inbound flows through
  `agent_inbox` → mailbox consumer → pty.
- ❌ `internal/queue/queue.go` — still imported by `cmd/maquinista/cmd_start.go`,
  `internal/bot/status.go`, `internal/bot/bot.go`.
- ❌ `tmux.SendKeysWithDelay` elsewhere — eight files still call it
  directly, including:
    - `internal/bot/commands.go` — `/c_clear`, `/c_esc`, bash-mode
      (`!`) dispatch.
    - `internal/bot/directory_browser.go` — tempfile boot prompts for
      directory-picked windows.
    - `internal/bot/handlers.go` — bash-mode `!cmd` fallthrough.
    - `internal/bot/planner_commands.go` — planner prompt send.
    - `internal/bot/recovery.go` — dead-window recovery boot.
    - `internal/bot/window_picker.go` — picker-driven attach.
    - `cmd/maquinista/mailbox_consumer.go` — the single-process
      consumer (which itself retires once `active/per-agent-sidecar.md`
      Phase 1 lands).
- ❌ In-process mailbox bridge — conceptually replaced by
  `mailbox_consumer` but that is itself the non-per-agent consumer.

## Scope

Three independently shippable steps.

### Step 1 — Delete `internal/queue/queue.go`

Grep its 3 importers:

- `internal/bot/bot.go` — initialization.
- `internal/bot/status.go` — reads from the queue for status display.
- `cmd/maquinista/cmd_start.go` — instantiation.

Replace with direct Telegram send calls keyed on
`channel_deliveries` fan-out, which the `internal/dispatcher`
already handles. Remove status-line integrations or re-point them at
`channel_deliveries.status`.

### Step 2 — Kill the non-inbox tmux call sites that still need direct drive

Most remaining `tmux.SendKeysWithDelay` call sites do **not** drive
an agent turn — they send meta-commands that the agent itself
doesn't need to see as an inbox row (ESC, /clear, bash-mode
commands). Those can legitimately stay, because they're not part of
the message-delivery pipeline.

The ones that should route through the mailbox:

- `planner_commands.go` — planner prompt should be an `agent_inbox`
  row with `from_kind='system'` instead of a direct pty write. Gives
  us idempotency + the same delivery guarantees as user messages.
- `directory_browser.go` — the first prompt after picking a
  directory is today a direct send-keys; route through the mailbox
  so it carries the same soul/memory injection story.
- `recovery.go` — dead-window recovery boot prompt.

Others (bash-mode `!`, `/c_clear`, `/c_esc`) are UI-level
keystrokes, not turns. Document them in code comments as
"intentionally bypass the mailbox" and leave.

### Step 3 — Fold `mailbox_consumer` into the per-agent sidecar

This is the tail end of `active/per-agent-sidecar.md` Phase 1. Once
each agent has its own sidecar, the single-process consumer has
nothing to do. Delete the file.

## Verification

- **Step 1** — `go build ./...` clean; `grep -r 'internal/queue' .`
  returns zero hits.
- **Step 2** — `grep -rn 'tmux.SendKeysWithDelay' internal/bot/
  cmd/maquinista/` returns only bash-mode / `/c_clear` / `/c_esc`
  call sites, each flagged with a comment explaining why it's direct.
- **Step 3** — `grep -n 'runMailboxConsumer' .` returns zero hits.

## Files

- `internal/queue/` — delete.
- `internal/bot/{bot,status}.go` + `cmd/maquinista/cmd_start.go` —
  drop the queue imports.
- `internal/bot/{planner_commands,directory_browser,recovery}.go` —
  route boot prompts through `routeTextViaInbox` instead of
  `SendKeysWithDelay`.
- `cmd/maquinista/mailbox_consumer.go` — retire once per-agent
  sidecar lands.

## Interaction with other plans

- Strictly follows `active/per-agent-sidecar.md`. Step 3 is
  effectively Phase 1 of that plan's final cleanup.
- Unblocks the §10a non-interactive runner cleanup noted in the
  archived implementation plan (Task 3.7). That task was claimed
  done but some non-interactive helpers are still around —
  evaluating that is the responsibility of a follow-up plan.
