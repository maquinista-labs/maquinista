# Per-topic agent pivot

## §0 Principle: Postgres is the system of record

**Persistent state is a Postgres row.** Never a markdown file, never JSON on
disk, never a dotfile. The database is the system of record. Every value that
has to survive a bot restart — bindings, sessions, memory, soul, skills,
checkpoints, configuration, scheduled jobs, per-topic overrides — is a table
column, not a filesystem artifact. Markdown is for humans reading
documentation; tables are for the system to read. If a feature would introduce
"we scan some files under `~/.maquinista/*.md` at spawn time," redesign it as a
table.

Rationale:

- One source of truth. No file/DB divergence, no stale caches.
- Transactional updates across related fields.
- Queryable from operators, dashboard, schedulers, tests.
- Multi-writer-safe without flocks or replace-and-pray.
- Schema evolves through migrations, not ad-hoc file-format parsers.
- Restart reliability: if the daemon dies mid-write, Postgres is the arbiter.

The only permitted exceptions are ephemeral, process-local artifacts (monitor
byte offsets, transcript tailing cursors) — and per `json-state-migration.md`
even those are moving to Postgres.

This principle will be copied into `maquinista-v2.md` as a new section §0,
placed before the problem statement. Every other plan doc gets a one-line
header reminder: "This plan adheres to §0: Postgres is the system of record."

## Why pivot

Current behavior (after commits 3.9 / 3.16 / 3.18):

- §8.1 tier-3 routing auto-binds every fresh topic to the global default agent
  via owner binding.
- One pty drives N topics — the sidecar serializes cross-thread messages into
  one Claude process. Explicitly acknowledged at `maquinista-v2.md:783`.
- Commit 3.18 prevents cross-topic *output* leak by tracking the last active
  thread per window, but the shared pty still means one Claude in-memory
  context for N topics → context contamination, coupled failure modes, and any
  future attempt at per-topic isolation would need to reload context on every
  turn (token-expensive).

Target:

- Each Telegram topic gets its own `agents` row, tmux window, Claude process,
  and session.
- Pty **is** the conversation. No serialization, no context swap, no reload.
- Same shape volta had before mailbox/routing landed; same shape
  hermes-agent uses per `session_id`.
- The mailbox / outbox / relay / A2A stack stays intact — it was never the
  cause of the pain.

## Scope

In scope:

- Tier-3 of the routing ladder: from "look up default agent" to "spawn new
  agent for this topic".
- `agent_id` format: auto-generated `t-<chat_id>-<thread_id>` at spawn.
  Stable, globally unique, grep-friendly. Never shown directly to end users.
- `agents.handle` column: nullable user-assignable alias, unique when set.
  Format `^[a-z0-9_-]{2,32}$`, reserved prefix `t-` forbidden. Resolves in
  `@mention` lookups alongside `id`.
- New command `/agent_rename <handle>`: set the handle of the current topic's
  owner agent.
- Retarget `/default @handle` to "attach this topic to existing @handle" and
  rename to `/agent_default`. Unknown handle returns an error with guidance
  (`/agent_list`, or start a new topic) — does NOT auto-spawn.
- Remove `/global-default` command.
- Remove startup spawn entirely: `maquinista start` spawns no Claude
  processes; the first message in any topic triggers tier-3 spawn.
- Command-prefix standardization (hard cutover, no aliases): rename all
  agent-family commands to `/agent_<verb>`.
    - `/agents` → `/agent_list`
    - `/default` → `/agent_default`
    - `/kill` → `/agent_kill` (if present)
    - `/observe` → `/agent_observe` (when landed)
    - future `/agent_sleep` (session-resume plan)
- Drop `agent_settings.is_default` column + unique index.
- Plan doc rewrites.

Explicitly out of scope, deferred to follow-up plans:

- **Memory model.** Hermes-shape semantics (snapshot at spawn, tool-based
  writeback), but Postgres-backed per §0. Columns on `agents` or a new
  `agent_memory` table; the table schema is a separate plan.
- **Session resume across daemon restarts** via `agent_topic_sessions.session_id`
  + `claude --resume <sid>`. Schema exists; wiring is its own plan.
- Observer bindings, A2A mentions, checkpoint/rollback, soul, dashboard,
  scheduled jobs — all orthogonal to topic↔agent arity. Unchanged.

## Part 1 — plan doc updates

1. `maquinista-v2.md`
   - Add §0: the principle statement above.
   - §8.1 tier-3: rewrite from "global default lookup" to "spawn a fresh
     per-topic agent". Tier-4 picker remains the explicit "attach to existing
     agent" path.
   - §11.1 "cross-thread ordering for the same agent": delete — no longer
     possible when one pty serves one thread.
   - §7 sidecar lifecycle: clarify "one pty = one conversation = one topic".
     No other change; the sidecar loop already fits.
   - Appendix B glossary: update `default agent`, `owner binding`, `observer
     binding`.
   - Appendix D task-agent observer attach: confirm wording; no behavior
     change.

2. `maquinista-v2-implementation.md`
   - Task 1.8: remove `/global-default` bullet. Narrow `/default` to topic
     attachment semantics. Add a new subtask: "tier-3 spawns new agent via
     SpawnTopicAgent".
   - Task 1.7 sidecar: no code change, but note that one sidecar per topic is
     the steady state.
   - Any lingering reference to `is_default`: remove.

3. `architecture-comparison.md`
   - Update tinyclaw comparison: maquinista is now 1 topic : 1 agent : 1
     session, closer to volta and hermes-agent than to Letta.

4. `json-state-migration.md`
   - Phase B1 note: `agent_topic_sessions` stays as the forward path for
     session resume. Under 1:1, its PK collapses to effectively
     `agent_id`-unique; the tightening migration is deferred to the
     session-resume plan.
   - Drop `reset_flag` from the table (no meaningful use under 1:1).

5. `multi-agent-registry.md`
   - Reconcile loop: one reconciled agent per live topic binding, not one
     shared default. Small rewrite.

6. `agent-to-agent-communication.md`
   - §0 header reminder. Audit for shared-default assumptions; flag if any
     (expect none).

7. `checkpoint-rollback.md`, `agent-soul-db-state.md`, `agent-memory-db.md`,
   `dashboard.md`, `opencode-integration.md`, `execution_plan.md`
   - §0 header reminder. No content changes expected; anything found in audit
     is a follow-up.

## Part 2 — code changes

Implementation order:

1. `internal/routing/routing.go`
   - Introduce `type SpawnFunc func(ctx, userID, threadID string, chatID *int64) (agentID string, err error)`.
   - `Resolve` accepts `SpawnFunc` (field on a new `Resolver` struct, or a
     context value — pick the less invasive form).
   - Tier-3 body: call `SpawnFunc`, then `writeOwnerBinding(resolvedID)`. Drop
     `lookupGlobalDefault`, drop `SetGlobalDefault`.

2. `cmd/maquinista/` — extract `ensureDefaultAgent` into a reusable
   `SpawnTopicAgent(cfg, pool, userID, threadID, chatID, cwd, runner) (agentID, windowID, error)`
   in its own file (e.g. `spawn_topic_agent.go`).
   - No startup invocation. The startup flow no longer calls any spawn — the
     daemon comes up with zero Claude processes.
   - Agent id format: `t-<chat_id>-<thread_id>`. Deterministic, globally
     unique (Telegram thread_ids are unique per chat), no UUID required.
     Reconstructable from a (chat, thread) pair without a DB round-trip.
   - Document the format in the glossary (`maquinista-v2.md` appendix B).

3. `internal/bot/handlers.go`
   - Wire `SpawnTopicAgent` into `routing.Resolve`.
   - Keep `syncAgentStateFor` — the 3.18 `ActiveThreads` safety net costs
     nothing and protects against any future regression.
   - Extend mention/handle resolution: `@foo` resolves via
     `SELECT id FROM agents WHERE id=$1 OR LOWER(handle)=LOWER($1) LIMIT 1`.
     Use this resolver in tier-1 `Resolve`, `/agent_default`, and `agentExists`.
   - Remove the old `/default` and `/global-default` handlers. Register
     `/agent_default` with the new attach-only semantics; unknown handle
     returns a guidance error (never auto-spawns).
   - Register `/agent_rename <handle>`: validates the handle regex and
     reserved-prefix rule, looks up the current topic's owner agent, updates
     `agents.handle`. Reply with confirmation or a uniqueness / format error.

4. `internal/routing/routing_test.go`
   - Tier-3 tests: inject mock `SpawnFunc`; assert it's called exactly once
     per fresh (user, thread); assert the returned id receives the owner
     binding.
   - Mention resolution test: `@id` and `@handle` both resolve to the same
     canonical `agents.id`.
   - Tier-2 owner-binding tests: unchanged.

5. Migrations

   `migrations/013_drop_default_agent_flag.sql`:

   ```sql
   DROP INDEX IF EXISTS uq_agent_settings_is_default;
   ALTER TABLE agent_settings DROP COLUMN IF EXISTS is_default;
   ```

   `migrations/014_agents_handle.sql`:

   ```sql
   ALTER TABLE agents ADD COLUMN IF NOT EXISTS handle TEXT;
   CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_handle_lower
     ON agents (LOWER(handle)) WHERE handle IS NOT NULL;
   ```

6. `internal/bot/commands.go`
   - Hard cutover. Deregister `/agents`, `/default`, `/global-default`, and
     any other agent-family commands under their old names. Register
     `/agent_list`, `/agent_default`, `/agent_rename`, and preserve existing
     handlers under the new names.
   - Update help text and `BotCommand` metadata so autocomplete shows the
     `/agent_*` family grouped together.

7. `internal/bot/commands_test.go`
   - Drop global-default tests, drop legacy `/agents` / `/default` tests.
   - Add coverage for `/agent_list`, `/agent_rename` (valid handle, regex
     rejection, reserved prefix, uniqueness collision), `/agent_default`
     (attach existing, error on unknown, no-op on already-bound).

8. Code comments — `internal/mailbox` or wherever `agent_topic_sessions` is
   referenced: annotate the `reset_flag` column and the full `(user_id,
   thread_id)` PK as "owned by the deferred session-resume plan; not read by
   the pivoted 1:1 routing model." Don't drop; don't wire.

Deferred (session-resume plan):

- `migrations/015_tighten_agent_topic_sessions.sql`: `UNIQUE (agent_id)`
  once 1:1 is confirmed.
- `agent_topic_sessions.reset_flag` fate (drop, repurpose, or replace with
  `session_id IS NULL` semantics).
- Sidecar wiring for `claude --resume <sid>`.
- `/agent_sleep @handle` command.

## Rollout order

Three commits, each self-contained and independently revertible:

1. **3.19 — docs.** Add §0 to `maquinista-v2.md`; update the plan docs listed
   in Part 1. Zero runtime impact.
2. **3.20 — routing pivot + commands.** Ship the behavior change:
   `SpawnTopicAgent`, tier-3 swap, migrations 013 + 014, `handle` column,
   `/agent_list` / `/agent_default` / `/agent_rename` (hard-cutover rename
   from `/agents` / `/default`), removal of `/global-default` and startup
   spawn, tests. User-visible cutover.
3. **3.21 — cleanup.** Remove `lookupGlobalDefault`, `SetGlobalDefault`,
   `ensureDefaultAgent` callsites at startup, any legacy command handlers,
   dead test fixtures. Add the `reset_flag` / `agent_topic_sessions` code
   comments flagging the deferred session-resume work.

## Acceptance criteria

Behavioral (manual verification against live bot after 3.20):

- Two fresh Telegram topics, one message in each, produce two distinct
  `agents` rows (ids `t-<chat>-<thread1>`, `t-<chat>-<thread2>`) and two
  distinct tmux windows.
- Each topic's reply reaches only that topic.
- `state.json` shows 1:1 `(user, thread) → window` with no duplicates.
- `/agent_list` prints the running agents.
- `/agent_rename researcher` sets a handle; it then autocompletes the `@`
  mention and resolves to the same agent.
- `/agent_default @researcher` from a fresh topic attaches to that agent
  (shared-agent escape hatch works when opt-in).
- `/agent_default @nobody-here` returns a guidance error; does not spawn.
- Legacy names (`/agents`, `/default`, `/global-default`) return "unknown
  command."
- `maquinista start` comes up with zero Claude processes; no agent spawns
  until a user sends a message.

Structural:

- `go test ./...` green.
- `routing_test.go` covers tier-3 spawn and `id`-or-`handle` mention
  resolution.
- `commands_test.go` covers `/agent_rename` validation (regex, reserved
  prefix `t-`, uniqueness collision) and `/agent_default` unknown-handle
  error path.
- Migrations 013 + 014 apply cleanly on a DB with and without existing
  `is_default=TRUE` rows and with and without pre-existing agents.

## Risk and mitigation

- **Cold-start latency per new topic.** First message blocks ~1–3 s on tmux
  spawn + Claude init. Mitigation: reply with a "spawning…" placeholder
  immediately, then let the agent take over.
- **Memory footprint.** N topics ≈ N Claude processes (~150–300 MB each).
  Acceptable on a workstation; a follow-up plan can add `/agent_sleep
  @handle` to suspend idle topics (detach pty, keep row; resume via
  `claude --resume` when next message arrives — ties into the
  session-resume plan).
- **Existing long-running default agent after cutover.** Its prior owner
  bindings remain valid; those topics keep routing to it (tier-2 hits first).
  New topics get fresh agents. No data migration required; rows heal
  naturally.
- **Agent sprawl in `agents` table.** Expected, not a bug. The dashboard and
  `maquinista agents` command will show one row per active topic — that's the
  point.

## Resolved decisions

1. **Agent id format.** Auto-generated `t-<chat_id>-<thread_id>`, stable PK,
   never user-facing. Separate nullable `agents.handle` column gives users a
   friendly alias via `/agent_rename`.
2. **Startup spawn.** Removed entirely. `maquinista start` comes up with zero
   Claude processes. First message in any topic triggers tier-3 spawn.
3. **`/agent_default @unknown`.** Error with guidance (`/agent_list` to see
   existing, or open a fresh topic for a new agent). No auto-spawn from
   `/agent_default`; creation happens only via tier-3.
4. **`agent_topic_sessions.reset_flag`.** Deferred to the session-resume
   plan. Add code comments in this pivot marking the column and the full
   `(agent_id, user_id, thread_id)` PK as "owned by the session-resume
   plan, not read under 1:1 routing."
5. **Command-prefix standardization.** Hard cutover to `/agent_<verb>` for
   all agent-family commands (`/agent_list`, `/agent_default`,
   `/agent_rename`, `/agent_kill`, `/agent_observe`). No aliases for the old
   names. Telegram's BotFather registration only allows `[a-z0-9_]`, hence
   underscore separator.
