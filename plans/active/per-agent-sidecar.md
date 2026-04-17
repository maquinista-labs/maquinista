# Per-agent sidecar

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

`reference/maquinista-v2.md` §7 defines the α lifetime model: "a dedicated
**sidecar** per agent is the sole consumer of that agent's mailbox. The
sidecar is the only piece of code that touches the pty; the bot,
orchestrator, outbox relay, and Telegram dispatcher never interact with
tmux."

This was captured as Task 1.7 in the v2 implementation plan. It did not
ship. `archive/maquinista-v2-implementation.md` was moved to `archive/`
while the task list was declared ~95% done, but Task 1.7 was left
undone. This doc rehomes the remaining work under `active/` so the
status is honest.

## Current behavior

`cmd/maquinista/mailbox_consumer.go:36:runMailboxConsumer` runs **one**
goroutine draining every agent's inbox serially via
`SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1`. When one agent is
mid-tool-call, every other agent's message waits until that row is
acked. `internal/sidecar/sidecar.go` is a near-empty skeleton that is
not spawned per agent.

Symptoms:

- Two topics sending messages simultaneously will see one topic reply
  instantly while the other blocks for the full first-topic turn.
- A stalled runner (claude hung on a long tool) blocks the whole bot.
- The monitor-source transcript tailing lives in a single goroutine
  shared across all agents (ok in practice, but the sidecar design
  colocates it with the pty driver).

## Scope

Three phases, each shippable independently.

### Phase 1 — Promote `internal/sidecar/` to per-agent runner

- Define `SidecarRunner` owning one agent: pty driver + transcript tail.
- At daemon boot (and at every tier-3 spawn / startup reconcile), spawn
  one sidecar goroutine per live agent.
- Sidecar subscribes to `LISTEN agent_inbox_new` with a payload filter
  — a notification carries `agent_id`, sidecars only react to their
  own.
- Sidecar lease and claim semantics: `FOR UPDATE SKIP LOCKED` scoped to
  `agent_id = $own`. Two sidecars never fight for the same row
  (different `agent_id`).
- Existing `runMailboxConsumer` is deleted; the mailbox consumer path
  retires.

### Phase 2 — Fold monitor sources into the sidecar

- Today `internal/monitor/source_claude.go` polls all Claude sessions
  in one process via `loadRunnerSessionMap`. Move the tailing code
  into the sidecar (per-agent), removing the cross-agent polling
  loop.
- Status-line + interactive-UI detection stays in the shared `monitor`
  package but is invoked by the sidecar on its own pane.

### Phase 3 — Lease reaper + crash recovery

- Background reaper scans `agent_inbox` for rows with
  `status='processing'` and `lease_expires < NOW()`; flips them back
  to `pending`.
- On sidecar start, reclaim any row where `claimed_by` matches this
  sidecar's id (supervisor restart path). Decide per-row whether the
  pty observed the response (ack) or not (retry), using the JSONL
  tail offset as the arbiter.

## Interaction with other active plans

- `resume-memory-refresh.md` becomes trivial once this lands: the
  sidecar knows when a turn completes, so it can inject a
  "since-you-were-last-here" summary as a synthesized user turn on
  the first resume.
- `retire-legacy-tmux-paths.md` is partly blocked on Phase 1 — many
  `tmux.SendKeysWithDelay` call sites in `internal/bot/*.go` exist
  because the mailbox consumer is in-process with the bot; a true
  sidecar moves that code out and lets the bot stop touching tmux
  entirely.

## Verification

- **Phase 1** — spawn two agents, each running a long-lived `Bash` tool
  call. From another topic, send a message to a third agent; the reply
  time is ≈ the single-agent latency, not the sum of all agents' turn
  times.
- **Phase 2** — kill `internal/monitor` package entirely; per-agent
  sidecars continue to capture transcripts.
- **Phase 3** — `kill -9` the maquinista daemon mid-turn; restart;
  `agent_inbox` rows that were `processing` flip back to `pending`
  within 5 minutes and the sidecar re-drives them.

## Files to modify

- `internal/sidecar/sidecar.go` — implement `SidecarRunner`.
- `cmd/maquinista/cmd_start.go` — spawn a sidecar per live agent at
  boot; `reconcileAgentPanes` already iterates the same rows, so the
  sidecar spawn can piggyback on that loop.
- `cmd/maquinista/mailbox_consumer.go` — retire.
- `internal/monitor/source_claude.go` / `source_openclaude.go` /
  `source_opencode.go` — move tailing into the sidecar package.
- New: `internal/sidecar/reaper.go` for Phase 3.

## Open questions

1. **Supervisor model** — supervisor goroutine per daemon (Go-channel
   restart on panic) or standalone process per agent? Start with
   goroutines; upgrade only if OS-level isolation becomes necessary.
2. **`LISTEN` connection pooling** — pgx's LISTEN ties up a connection.
   N sidecars = N dedicated connections. Manageable at small N; for
   bigger deployments consider a single listener that fans events out
   to per-agent channels.
3. **Turn boundary detection** — when the sidecar decides a turn is
   complete (so it acks the inbox row and writes the outbox), it
   currently reads the JSONL tail. Works for Claude/OpenClaude; for
   OpenCode the sidecar must read OpenCode's SQLite DB. Both already
   implemented in `internal/monitor/source_*`, just needs to move.
