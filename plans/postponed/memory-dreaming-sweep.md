# Memory dreaming sweep

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

Extracted from `agent-memory-db.md` Phase 4 (remaining). The rest of that
plan is fully shipped; this is the one piece that wasn't.

Agents accumulate `signal`-tier archival passages — raw observations, turn
fragments, things the agent or auto-flush thought might be worth keeping but
didn't immediately promote to `long_term`. Without a promotion pass these rows
grow forever, degrade retrieval quality (signal noise drowns out long-term
facts), and hit the size cap indiscriminately.

The "dreaming sweep" is the promotion loop: periodically evaluate each
`signal` row, decide promote / keep / discard, write decisions to an audit
table. Inspired by openclaw's `DREAMS.md` → `MEMORY.md` cron and Letta's
background consolidation agent.

**Off by default.** Gated by `cfg.Memory.Dreaming.Enabled` so operators opt in
per deployment. Per-agent opt-in via `agent_settings.roster->>'dreaming'`.

---

## What the sweep does

1. **Select candidates** — `SELECT … FROM agent_memories WHERE tier='signal' AND score < threshold AND (expires_at IS NULL OR expires_at > NOW()) ORDER BY created_at LIMIT batch`.
2. **Score/classify** — call a small cheap model (e.g. Haiku) with the row's
   title + body and ask: *promote to long_term / keep as signal / discard*.
   Optionally ask it to rewrite the body into a tighter fact.
3. **Apply decision** — `UPDATE agent_memories SET tier='long_term', score=…`
   or `DELETE` or no-op.
4. **Audit** — write one row to `agent_memory_events` per decision
   (action, rationale, model used, latency).

---

## Dispatch options

Three ways to drive the sweep loop. The choice affects throughput,
observability, and how much work we need to add.

### Option A — Background goroutine in the daemon (simplest)

A single goroutine inside `maquinista start` that sleeps N minutes, wakes,
queries due agents, runs the sweep inline, sleeps again.

**Pros:** No new infrastructure. Already have a pattern for daemon goroutines
(`go scheduler.Run(ctx, pool, cfg)`).

**Cons:** Goroutine blocks the daemon process during model calls. Dreaming
latency spikes if the model is slow or the batch is large. No visibility into
in-progress sweeps without log tailing. Hard to restart a sweep that died
mid-batch.

### Option B — `scheduled_jobs` row per agent (reuse existing scheduler)

At agent-add time (same as the autoflush job in W2), insert a `scheduled_jobs`
row with a nightly cron (e.g. `0 3 * * *`) and a prompt that tells the agent
to call `memory_promote` / `memory_discard` on its own signal rows. The agent
does the LLM reasoning itself; no separate model call needed.

**Pros:** Zero new infrastructure. Works with the scheduler already running.
Agent self-curates, which aligns with Letta's design philosophy. Fully
auditable via `job_executions`.

**Cons:** The agent could be offline (status != running) when the cron fires
— the scheduler enqueues the message but no one processes it until the next
spawn. Requires exposing `memory_promote` / `memory_discard` as tools the
agent can call. Ties dreaming throughput to agent turn latency (if the agent
has 1000 signal rows, one dreaming turn may not finish them all).

### Option C — DB-backed job queue (recommended for production scale)

Add a `memory_dream_queue` table. When a signal-tier row is inserted, a
Postgres trigger (or the insert path in `memory.Remember`) enqueues a row
there. A separate worker pool drains the queue with `FOR UPDATE SKIP LOCKED`,
calls the model, applies the decision, deletes the queue row.

```sql
CREATE TABLE memory_dream_queue (
    id          BIGSERIAL    PRIMARY KEY,
    agent_id    TEXT         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    memory_id   BIGINT       NOT NULL REFERENCES agent_memories(id) ON DELETE CASCADE,
    enqueued_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    attempts    INT          NOT NULL DEFAULT 0,
    UNIQUE (memory_id)  -- idempotent re-enqueue
);
```

Worker loop (can run inside the daemon or as a separate process):
```
loop:
  claim batch (FOR UPDATE SKIP LOCKED, LIMIT N)
  for each row:
    call model → decision
    apply decision to agent_memories
    insert into agent_memory_events
    delete from memory_dream_queue
  sleep if batch was empty
```

**Pros:** Decoupled from agent liveness. Each row is processed exactly once
(idempotent via UNIQUE on memory_id). Easy to add concurrency (multiple
workers). Failures auto-retry via attempts counter (dead-letter after N).
Queue depth is directly observable. Can be drained by a separate process if
the daemon is busy.

**Cons:** New table + worker infrastructure. Trigger on `agent_memories` insert
adds write overhead. Over-engineering for a low-volume sweep unless you have
many agents or want reliable retry guarantees.

### Option D — `maquinista memory dream <agent-id>` CLI + cron trigger

Manual/external trigger: `maquinista memory dream <agent-id>` runs the sweep
synchronously for one agent. A system-level cron (crontab / systemd timer)
calls it on a schedule.

**Pros:** Simplest possible implementation. No daemon changes. Easy to test.

**Cons:** Relies on the host cron, not the DB scheduler — violates the "no
config on disk" spirit. Manual; if the cron host goes away the sweep stops
silently.

---

## Recommended path

**Start with Option B** (scheduled_jobs per agent, agent self-curates). It
ships in one PR: add the `memory_promote` / `memory_discard` tools to the
agent tool surface, register a nightly scheduled job at agent-add time, done.
No new tables, no new infrastructure.

**Graduate to Option C** if signal volume becomes a problem or if the agent's
offline window is long enough that batches pile up. The queue table is
additive — Option B jobs can continue to coexist.

---

## Schema additions needed

```sql
-- Audit log for all dreaming decisions (Option B, C, D all use this).
CREATE TABLE agent_memory_events (
    id          BIGSERIAL    PRIMARY KEY,
    agent_id    TEXT         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    memory_id   BIGINT       REFERENCES agent_memories(id) ON DELETE SET NULL,
    action      TEXT         NOT NULL CHECK (action IN ('promote','keep','discard','evict')),
    rationale   TEXT,
    model       TEXT,
    latency_ms  INT,
    at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_memory_events_agent ON agent_memory_events (agent_id, at DESC);
```

Option C additionally needs `memory_dream_queue` (see above).

---

## Files

- `internal/db/migrations/033_agent_memory_events.sql` — audit table (+ queue table for Option C)
- `internal/memory/dreaming.go` — sweep logic: query candidates, call model, apply decision, audit
- `cmd/maquinista/cmd_memory.go` — add `memory dream <agent-id>` subcommand
- `cmd/maquinista/cmd_agent.go` — register nightly dream job at agent-add time (Option B)

---

## Open questions

1. **Model choice.** Haiku is cheap and fast enough for a one-sentence
   promote/discard call. Should model be configurable per agent or global?
2. **Batch size.** How many signal rows per sweep pass? Too many = long turns;
   too few = slow drain. Start at 20, make configurable.
3. **Score threshold.** When does a signal row become a dreaming candidate?
   Current schema has a `score` float (default 0). Need a scoring heuristic
   or just sweep all signal rows and let the model decide.
4. **Option B tool surface.** `memory_promote({id})` and `memory_discard({id})`
   need to be exposed as agent tools. Currently only `memory_remember`,
   `memory_list`, `memory_search`, `memory_forget` exist.
