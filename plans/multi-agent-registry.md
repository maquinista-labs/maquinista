# Multi-agent registry & reconcile loop

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

After commits 3.10–3.15, `./maquinista start` boots **one** agent
decided by `--agent` / `MAQUINISTA_DEFAULT_AGENT` (fallback
`"maquinista"`). `ensureDefaultAgent` writes that single row to
`agents` and spawns its tmux window.

Real usage wants more:

- Multiple persistent agents (e.g. `@maquinista`, `@alice`, `@bob`),
  each with its own identity, cwd, system prompt, optional memory
  store.
- The `agents` table becomes the **registry** (source of truth).
  `./maquinista start` reconciles reality against the registry —
  respawn anything missing, leave anything healthy alone.
- Per-agent "soul" config (persona, system prompt, runner) flows into
  the spawned pane automatically via `agent_settings`.

The existing schema already has most of what's needed:

- `agents (id, tmux_session, tmux_window, role, status, runner_type,
  task_id, stop_requested, started_at, last_seen)` — migrations 001, 006,
  008, 009.
- `agent_settings (agent_id, persona, system_prompt, heartbeat, roster,
  is_default)` — migration 009.

Task-scoped agents (spawned by `internal/orchestrator/ensure_agent.go`
with `task_id != NULL`) are orthogonal — they come and go per task and
must **not** be reconciled here. The new reconcile loop targets only
persistent user agents: `role='user' AND task_id IS NULL`.

## Scope

Three phases. Each independently shippable.

### Phase 1 — Reconcile loop replaces `ensureDefaultAgent`

Rename and generalise the current single-agent spawn into
`reconcilePersistentAgents(ctx, cfg, pool)`.

Algorithm:

1. `SELECT id, tmux_session, COALESCE(tmux_window,'') AS tw, status,
   runner_type, cwd FROM agents WHERE role='user' AND task_id IS NULL
   AND status != 'archived' AND NOT stop_requested`.
2. If zero rows: **bootstrap** — insert the row `(id,
   cfg.TmuxSessionName, '', 'user', 'stopped', cfg.DefaultRunner,
   cfg.DefaultAgentCWD)` using the 3.10 precedence (flag > env >
   fallback `"maquinista"`). Re-query.
3. For each row:
   a. If `tw != ''` and `tmuxWindowExists(session, tw)` → leave alone,
      log `reconcile: <id> healthy at <tw>`.
   b. Else spawn a tmux window for this agent (details §Phase 2), update
      `agents.tmux_window` + `status='running'` + `stop_requested=FALSE`.

Bootstrap uses the same `--agent` / `MAQUINISTA_DEFAULT_AGENT` /
fallback chain from 3.10 so a fresh install still "just works".

Deletion of `ensureDefaultAgent` — its logic becomes step 3b's spawn
helper. The dead-window reconcile from 3.15 (`flip status to stopped
when window gone`) stays, but now it triggers respawn on the next
`reconcilePersistentAgents` call rather than waiting for
`./maquinista stop` / restart.

Optional stretch: run reconcile on a periodic ticker (every 30 s) so a
manually `tmux kill-window` recovers without restarting the bot.
Defer decision; flag-gated later.

### Phase 2 — Inject `agent_settings` into the spawned pane

Every spawn currently exports:

```
AGENT_ID     = <id>
DATABASE_URL = <url>
RUNNER_TYPE  = <runner>
```

Extend to:

```
AGENT_ID     = <id>
DATABASE_URL = <url>
RUNNER_TYPE  = agents.runner_type
PERSONA      = agent_settings.persona         (if non-null)
MAQUINISTA_AGENT_PROMPT = /tmp/maquinista-prompts/<id>.md
                         (written with agent_settings.system_prompt)
```

Runner command selection:

- If `agent_settings.system_prompt` is non-empty, use the runner's
  `PlannerCommand(systemPromptPath, cfg)` variant — claude already
  supports `--system-prompt "$(cat FILE)"` (`internal/runner/claude.go:30-32`).
- Else use `LaunchCommand(cfg)` as today.

Add a `PromptPath()` helper on the Config struct so the tempfile lives
under `$MAQUINISTA_DIR/prompts/` and is rewritten at every spawn.

Deliverable: `maquinista agent edit <id> --system-prompt prompt.md`
(Phase 3) followed by `./maquinista start` and the tmux pane runs
`claude --dangerously-skip-permissions --system-prompt "$(cat
/tmp/.../<id>.md)"` with the prompt applied.

### Phase 3 — `maquinista agent …` CLI

New `cmd/maquinista/cmd_agent.go` with subcommands:

| Subcommand | Purpose |
|---|---|
| `agent list [--all]` | Table of `id, runner, status, tmux_window, cwd, is_default, task_id, last_seen`. `--all` includes `archived` + task-scoped. |
| `agent add <id> [--runner claude] [--role user] [--cwd DIR] [--system-prompt FILE] [--persona NAME]` | Insert row into `agents` + upsert `agent_settings`. |
| `agent edit <id> [--runner …] [--cwd …] [--system-prompt …] [--persona …]` | Partial update. |
| `agent archive <id>` | `UPDATE agents SET status='archived'`. Reconcile skips. Keeps history / bindings intact. |
| `agent kill <id>` | `UPDATE agents SET stop_requested=TRUE`. Signals the agent's sidecar (future) to exit; reconcile stops spawning. |
| `agent spawn <id>` | Force-respawn: kill tmux window if present, clear `tmux_window`, run reconcile for just this row. |

Existing `maquinista agent_list` Telegram command stays; it reads the
same rows. Existing `maquinista spawn` (task-scoped agents via
`cmd_spawn.go`) is not touched — different concept.

### Phase 4 — Memory (deferred)

Out of scope for this plan, but recorded so future work has a hook:

- New table `agent_memory(agent_id, key, value, updated_at, PRIMARY
  KEY(agent_id, key))`.
- Runner reads it via a new `maquinista memory get <id> <key>` CLI —
  agents call it as a tool, not a built-in.
- Alternative: a per-agent `CLAUDE.md` written under the agent's cwd
  at spawn time (zero schema, zero CLI), refreshed from
  `agent_settings.system_prompt + derived context`.

Revisit after Phase 1–3 land.

## Schema changes

Minimal. Add two nullable columns to `agents` that Phase 2 needs:

Migration `013_agents_cwd_system_prompt.sql`:

```sql
ALTER TABLE agents ADD COLUMN IF NOT EXISTS cwd TEXT;
-- session_id + window_name land in the separate plan
-- plans/json-state-migration.md Phase A; this migration only adds cwd
-- since reconcile needs it in the agents row directly (avoid reading
-- across tables during spawn hot path).
```

If `plans/json-state-migration.md` Phase A lands first, `cwd` is
already present — skip this migration, reuse the column.

No new columns on `agent_settings`; it already has what Phase 2 needs.

## Files to modify

### Phase 1

- `cmd/maquinista/cmd_start.go` — call `reconcilePersistentAgents` in
  place of the current `ensureDefaultAgent` invocation.
- `cmd/maquinista/cmd_start_default_agent.go` — rename file to
  `cmd_start_reconcile.go`; keep bootstrap helper that reads
  `--agent` / env / fallback; add the reconcile loop.

### Phase 2

- `cmd/maquinista/cmd_start_reconcile.go` — per-agent spawn helper
  reads `agent_settings`, writes tempfile prompt, picks runner
  variant.
- `internal/runner/runner.go` — no interface change (use existing
  `PlannerCommand` as the "with-system-prompt" variant, or add a
  third method `WithSystemPromptCommand`; prefer reusing
  `PlannerCommand`).

### Phase 3

- `cmd/maquinista/cmd_agent.go` (new) — cobra group.
- `internal/db/queries.go` — add `UpsertAgent`, `ArchiveAgent`,
  `SetAgentSettings` helpers (some may exist; extend as needed).

### Phase 4 (deferred)

- `internal/db/migrations/NNN_agent_memory.sql` — new table.
- `cmd/maquinista/cmd_memory.go` — CRUD surface.

## Verification per phase

- **Phase 1** — `DELETE FROM agents;` + `./maquinista start` → one row
  appears (bootstrap) + one tmux window spawned. Then `tmux kill-window
  -t maquinista:<name>` + wait / restart → the window respawns. Then
  `psql: INSERT INTO agents (id, role, ...) VALUES ('alice', 'user',
  ...);` + restart → two windows. No task-scoped agents (`task_id IS
  NOT NULL`) get respawned.
- **Phase 2** — `UPDATE agent_settings SET system_prompt='Be Yoda.'
  WHERE agent_id='maquinista';` + restart → tmux window's command line
  includes `--system-prompt`; sending a Telegram message gets a
  Yoda-speak reply.
- **Phase 3** — `maquinista agent add alice --system-prompt alice.md
  --cwd ~/code/other` → row + settings inserted. `maquinista agent
  list` shows both. `./maquinista start` spawns both. `maquinista
  agent archive alice` → next start skips alice.

## Open questions for whoever executes

1. **Periodic reconcile ticker** (Phase 1 stretch) — default on or off?
   If on: what interval avoids spamming tmux on healthy boxes? 30 s
   feels right but needs an escape hatch.
2. **One tmux session for all agents or one per agent?** Current code
   uses `cfg.TmuxSessionName` ("maquinista") for everyone; N agents in
   one session scales fine up to ~20 windows but attaching becomes
   noisy. Could add `agents.tmux_session` override per row.
3. **Bootstrap precedence** — if the DB was intentionally empty (user
   archived the last agent), do we re-insert the default? Current
   Phase 1 §Step 2 says yes. Consider a `--no-bootstrap` flag or a
   sentinel row.
4. **Interaction with `global_default`** — should `agent add` set
   `is_default=TRUE` for the first inserted agent automatically? Makes
   the fresh-install flow "messages route immediately without
   picker"; but surprises multi-agent users who add a second one.
5. **`agent_settings.roster`** — undocumented today. Does anyone know
   what it holds? Check before Phase 2 writes more settings.

---

Not committed. Review, adjust scope, then execute phase-by-phase.
