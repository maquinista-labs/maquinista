# Migrate on-disk JSON state to Postgres

## Context

The running maquinista process keeps three JSON files under
`$MAQUINISTA_DIR` (default `~/.maquinista/`):

| File | Writer | Purpose |
|---|---|---|
| `session_map.json` | `hook/hook.go:78-84`, `internal/agent/agent.go` fallback | Pane-level Claude session metadata (session_id, cwd, window_name) keyed by `<tmux_session>:<window_id>` |
| `state.json` | `internal/bot/*.go` via `state.State` | Bot-daemon in-memory maps — thread bindings, window display names, runner assignments, project bindings, group chat ids, history-page offsets, worktree metadata |
| `monitor_state.json` | `internal/monitor/monitor.go` | JSONL transcript byte offsets per watched session |

Problems with keeping them on disk:

- **Two sources of truth.** `ThreadBindings` in `state.json` and
  `topic_agent_bindings` in Postgres already diverge; same risk is
  growing for per-window metadata now that the hook writes to `agents`
  directly (commit 3.14).
- **Bot-local only.** Future multi-process topologies (sidecar per
  agent per v2 task 1.7, scheduled jobs, CLI that needs to reason
  about live windows) can't read `state.json` safely.
- **Mutation from multiple processes.** `session_map.json` is written
  by the hook running inside every Claude pane and read by the bot;
  the flock is correct today but the bot's legacy fallback discovery
  path and the sidecar work will add more writers.
- **Harder to inspect.** Every debug session ends with `cat
  ~/.maquinista/state.json | jq`. A `psql` one-liner wins.

The v2 plan (`plans/maquinista-v2-implementation.md §1.9`) explicitly
calls for deleting `state.ThreadBindings` and `session_map.json`
readers/writers and relocating session ids into `agent_topic_sessions`.
That task is not started.

## What each file actually stores

### `session_map.json` — pane metadata

```
map["<tmux_session>:<window_id>"] → SessionMapEntry{
    SessionID  string,   // Claude Code UUID
    CWD        string,
    WindowName string,
}
```

Already half-migrated in commit 3.14: the hook writes `(id,
tmux_session, tmux_window, role, status, runner_type, …)` to `agents`,
but session_id, cwd, and window_name still only live in the JSON.

### `state.json` — bot-local `state.State`

Fields of interest:

- `ThreadBindings` — `(user_id, thread_id) → window_id`. **Already in
  `topic_agent_bindings` as tier-2 owner rows** — this one is a
  redundant cache now.
- `WindowStates` — `window_id → {CWD, RunnerType}`. Overlaps with the
  hook's agents-table writes.
- `WindowDisplayNames` — `window_id → name`. Captured by
  `agents.tmux_window` semantics when `name == id` for auto-spawned
  agents.
- `UserWindowOffsets` — `(user_id, window_id) → byte_offset`. Used by
  `/p_history` pagination. No DB analog.
- `GroupChatIDs` — `(user_id, thread_id) → chat_id`. Used when the bot
  replies to a user outside the topic. No DB analog.
- `ProjectBindings` — `thread_id → project_id`. Set by `/p_bind`. No
  DB analog.
- `WorktreeBindings` — `window_id → WorktreeInfo{TaskID, Branch, Dir}`.
  Set by `/t_pickw`. No DB analog (`merge_queue` is a separate concept).
- `WindowRunners` — `window_id → runner_type`. Overlaps with
  `agents.runner_type`.

### `monitor_state.json` — JSONL tail offsets

```
map["<session_key>"] → TrackedSession{FilePath, LastByteOffset}
```

Per v2 task 1.7 the monitor collapses into a per-agent sidecar; its
state becomes process-local. **Do not migrate — delete when 1.7 lands.**

## Scope

Three phases, each independently shippable.

### Phase A — `session_map.json` → columns on `agents`

Add three columns to `agents`, update the hook and
`ensureDefaultAgent` to upsert them, update consumers, delete
`session_map.json` readers/writers.

Migration `012_agents_session_fields.sql`:

```sql
ALTER TABLE agents ADD COLUMN IF NOT EXISTS session_id  TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS cwd         TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS window_name TEXT;
```

All nullable — tier-4 picker never wrote these, existing rows stay
valid. `session_id` is Claude's UUID at SessionStart; null means the
agent is spawned but hasn't created a session yet.

Code touchpoints:

- `hook/hook.go:registerAgentFromEnv` — commit 3.14 added the row;
  extend the INSERT + ON CONFLICT UPDATE to also write `session_id =
  input.SessionID`, `cwd = input.CWD`, `window_name = windowName`.
- `cmd/maquinista/cmd_start_default_agent.go:ensureDefaultAgent` —
  write `window_name = agentID`, `cwd = cwd`, `session_id = NULL`.
- `internal/bot/directory_browser.go:discoverSessionID` — replace
  `state.LoadSessionMap` reads with `SELECT session_id FROM agents
  WHERE tmux_window = $1`.
- `internal/agent/agent.go:writeSessionMapFallback` — remove.
- `internal/state/session_map.go` — delete the whole file.
- `internal/state/state.go` — remove `SessionMap*` APIs
  (`LoadSessionMap`, `ReadModifyWriteSessionMap`, `SessionMapEntry`).
- `hook/hook.go:78-84` — stop writing `session_map.json`.

After Phase A, `session_map.json` is orphaned. Phase A's final commit
deletes the file on bot startup (one-liner `os.Remove` on the stale
path for existing installs).

### Phase B — `state.json` field-by-field migration

Each `state.State` field moves to DB in its own migration + code
change, oldest-overlap first.

**B1 — `ThreadBindings` removal.** Pure deletion — tier-2 routing
already reads `topic_agent_bindings` in `routing.lookupOwner`. Keep
`BindThread()` as a thin wrapper that does the DB insert, let
`GetWindowForThread` read from DB too, then delete the in-memory map.
No schema change.

**B2 — `WindowRunners` removal.** Replace reads/writes with direct
`agents.runner_type` reads. Code-only change.

**B3 — `WorktreeBindings` → new `window_worktrees` table.**

```sql
CREATE TABLE IF NOT EXISTS window_worktrees (
    window_id     TEXT        PRIMARY KEY,
    task_id       TEXT,
    branch        TEXT,
    worktree_dir  TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Writers: `merge_commands.go`, `task_picker.go`. Readers: `/t_merge`,
`/t_unclaim`, recovery. ~6 call sites.

**B4 — `ProjectBindings` → new `topic_projects` table.**

```sql
CREATE TABLE IF NOT EXISTS topic_projects (
    thread_id   TEXT        PRIMARY KEY,
    project_id  TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Writers: `/p_bind`. Readers: planner flow, auto-task flow.

**B5 — `GroupChatIDs` → new `user_thread_chats` table.**

```sql
CREATE TABLE IF NOT EXISTS user_thread_chats (
    user_id     TEXT        NOT NULL,
    thread_id   TEXT        NOT NULL,
    chat_id     BIGINT      NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, thread_id)
);
```

Used for replies outside a topic. Low-traffic.

**B6 — `UserWindowOffsets` → drop.** Currently tracks byte offsets
for `/p_history` pagination over the Telegram render buffer. The
mailbox already stores every message in `agent_outbox`/`agent_inbox`;
history becomes a live query:

```sql
SELECT … FROM agent_outbox
WHERE agent_id = (
    SELECT agent_id FROM topic_agent_bindings
    WHERE binding_type='owner' AND user_id=$1 AND thread_id=$2
)
ORDER BY created_at DESC OFFSET $3 LIMIT $4
```

No table needed.

**B7 — `WindowStates` / `WindowDisplayNames` removal.** Collapse into
the agents-table reads landed in Phase A. Code-only.

After Phase B, `state.json` is empty. Last commit of B deletes the
file on startup and removes `state.State` entirely.

### Phase C — `monitor_state.json`

Deferred. Lands with v2 task 1.7 (sidecar extraction). No standalone
work here; recorded so the migration plan is complete.

## Recommended sequencing

1. Phase A — small, self-contained, immediate debuggability win.
2. Phase B1 + B2 + B7 — pure deletions, land together once A is out.
3. Phase B3, B4, B5 — one migration per PR; easy to revert.
4. Phase B6 — convert `/p_history` to DB query; last because it also
   changes rendering behavior.
5. Phase C — wait for 1.7.

## Verification per phase

- Each phase: full test suite green (`go test ./...`) plus a manual
  round-trip from Telegram — bot restart, send a message, assert the
  DB row(s) appear and the old JSON field is gone.
- Phase A specifically: after `./maquinista start` + one Telegram
  message, `SELECT id, session_id, cwd, window_name FROM agents;`
  shows the maquinista agent populated, and
  `~/.maquinista/session_map.json` is gone.
- Phase B: for each removed field, grep confirms no remaining
  `state.State.<Field>` references and `~/.maquinista/state.json` size
  strictly shrinks between commits.

## Files to modify (Phase A, full detail)

- `internal/db/migrations/012_agents_session_fields.sql` (new)
- `hook/hook.go` (extend upsert; stop session_map write)
- `cmd/maquinista/cmd_start_default_agent.go` (write window_name, cwd)
- `internal/bot/directory_browser.go` (swap session_map reads for DB)
- `internal/agent/agent.go` (delete writeSessionMapFallback)
- `internal/state/session_map.go` (delete file)
- `internal/state/state.go` (remove SessionMap* APIs)
- `cmd/maquinista/cmd_start.go` (`os.Remove` stale session_map.json
  once on startup)

## Open sequencing question

Phase A alone is ~200 LOC and ships in an evening. Phases B and C
accumulate risk — every migration changes runtime semantics for a
subset of commands. Decide up-front whether a given push should scope
to Phase A, add the pure-deletion batch (B1 + B2 + B7), or draft a
day-by-day sequence for the whole thing.
