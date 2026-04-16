# Agent memory (DB-backed)

## Context

openclaw gives agents a three-tier memory layer, all file-based on
disk under `~/.openclaw/workspace/`:

- **`MEMORY.md`** — durable curated facts, injected into system context
  at DM session start (loaded once, not searched per turn).
- **`memory/YYYY-MM-DD.md`** — daily operational log; today + yesterday
  auto-loaded on session start.
- **`DREAMS.md`** — write-only background "dreaming" log that a cron
  promotes into `MEMORY.md` via scoring gates.

Retrieval is hybrid: `memory_search` tool uses BM25 + vector similarity
over a local SQLite FTS5 + embedding index
(`~/.openclaw/memory/<agentId>.sqlite`). Writes come from three
sources: the agent explicitly (via `memory_search`-adjacent tools), a
silent flush turn before compaction, and the optional dreaming cron.
Categories are structural (long-term vs daily vs signal) rather than
semantic (user vs feedback vs project like Claude Code's memory).

Maquinista is Postgres-only and prefers no new files on disk. This
plan ports openclaw's memory model to Postgres, keeps the three
structural categories, adds semantic categories as a lightweight tag,
and exposes memory as both a runner-facing tool and a spawn-time
injection.

Related: `multi-agent-registry.md` §Phase 4 sketches `agent_memory(agent_id,
key, value)` as a stub — this plan supersedes it.

## Scope

Five phases. Phase 0 introduces a two-tier storage split (core
blocks + archival passages) inspired by Letta — too load-bearing not
to adopt. Phase 1 is the minimum viable archival surface; 2–4 add
search, auto-flush, and the dreaming sweep; 5 enables cross-agent
memory sharing via archives.

### Phase 0 — Three-layer memory model (Letta-inspired)

Letta (`/home/otavio/code/letta/letta/schemas/memory.py:68-70` and
`schemas/block.py:13-69`) splits memory into three layers with
distinct read/write patterns. Maquinista's original plan conflated
them; adopting the split is a clean structural win:

| Layer | Letta term | What it is | Read | Write |
|---|---|---|---|---|
| Core (small, in-context) | **Blocks** | Persona + current-user facts + task note | Every turn, injected into system prompt | Agent tool calls (`core_memory_append`, `core_memory_replace`) |
| Archival (unbounded, semantic) | **Passages** | Durable facts, feedback, references | On-demand (search tool) | Agent tool call (`archival_memory_insert`) or auto-flush |
| Recall (conversation log) | **Messages** | Inbox/outbox history | On-demand (search by role/time) | Automatic (inbox/outbox rows) |

**Recall** is already done — `agent_inbox` + `agent_outbox` rows
with the `conversation_id` thread and FTS over content is a perfect
recall layer. No new work.

**Core blocks** and **archival passages** are two distinct tables
(this plan used to conflate them into one `agent_memories` table —
that's now split):

```sql
-- migration 015a_agent_blocks.sql   (core, in-context)
CREATE TABLE agent_blocks (
  id          BIGSERIAL PRIMARY KEY,
  agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  label       TEXT NOT NULL,                    -- 'persona', 'human', 'task-note', 'system/constraints'
  value       TEXT NOT NULL DEFAULT '',
  char_limit  INTEGER NOT NULL DEFAULT 2200,    -- hermes default; Letta default 8000
  read_only   BOOLEAN NOT NULL DEFAULT FALSE,   -- blocks operator-set content from agent edits
  description TEXT,                              -- one-liner shown to agent so it knows what to store here
  version     INTEGER NOT NULL DEFAULT 1,        -- optimistic-lock counter (Letta pattern)
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (agent_id, label)
);
```

Default blocks auto-created at agent creation (parallel to the
default soul template):

- `persona` — agent's self-notes (environment quirks, tool
  conventions). Seeded from soul's `core_truths` on insert; the agent
  edits it via `core_memory_append` as it learns.
- `human` — facts about the operator (pt-BR on Telegram, prefers terse
  replies). Seeded empty; grown via auto-flush on explicit "remember
  that I…" patterns.
- `task-note` — scratchpad tied to the current inbox turn. Auto-
  cleared when `conversation_id` changes.

Both the `label` scheme and the `read_only` flag match Letta
(`letta/schemas/block.py:13-69`). The character limit is enforced on
`update_block_value` — overflow raises an error the agent sees and
must resolve by pruning. This is how Letta forces the agent to curate
its own context instead of letting it grow unbounded.

**Tools exposed to the agent** (surface matches Letta's
`core_memory_append` / `core_memory_replace` —
`letta/services/tool_executor/core_tool_executor.py`):

```
core_memory_append({label, content})                → new_value
core_memory_replace({label, old_content, new_content}) → new_value
                                                         (exact-match required; errors if not found)
core_memory_read({label})                           → value
```

Archival memory keeps the richer schema the original plan defined —
rebranded as passages:

### Phase 1 — `agent_memories` table (archival passages) + CRUD tools

```sql
-- migration 015_agent_memories.sql
CREATE TABLE agent_memories (
  id           BIGSERIAL PRIMARY KEY,
  agent_id     TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  dimension    TEXT NOT NULL CHECK (dimension IN ('agent','user')),
  tier         TEXT NOT NULL CHECK (tier IN ('long_term','daily','signal')),
  category     TEXT NOT NULL CHECK (category IN ('feedback','project','reference','fact','preference','other')),
  title        TEXT NOT NULL,            -- short label, ≤120 chars
  body         TEXT NOT NULL,            -- Markdown content
  source       TEXT NOT NULL,            -- 'agent'|'operator'|'auto_flush'|'dream'|'import'
  source_ref   TEXT,                     -- session id, inbox id, etc. — free-form
  tags         TEXT[] NOT NULL DEFAULT '{}',
  pinned       BOOLEAN NOT NULL DEFAULT FALSE,   -- always injected at spawn
  score        REAL NOT NULL DEFAULT 0,   -- dreaming promotion score
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ,               -- NULL = never; daily rows get now()+30d
  -- Full-text search over title + body
  tsv          tsvector GENERATED ALWAYS AS
               (to_tsvector('simple', coalesce(title,'') || ' ' || coalesce(body,''))) STORED
);
CREATE INDEX agent_memories_agent_dim_tier_idx ON agent_memories (agent_id, dimension, tier, created_at DESC);
CREATE INDEX agent_memories_tsv_idx ON agent_memories USING GIN (tsv);
CREATE INDEX agent_memories_tags_idx ON agent_memories USING GIN (tags);
CREATE INDEX agent_memories_pinned_idx ON agent_memories (agent_id, dimension) WHERE pinned;

-- Eviction helper: nightly job prunes expired rows.
-- CREATE INDEX … expires_at IS NOT NULL covered by cron, not an index.
```

**Dimension semantics** (added after surveying hermes — it keeps two
distinct stores `MEMORY.md` and `USER.md`, cf. `memory_tool.py:52-55`,
character limits 2200 / 1375 chars). That split is load-bearing: the
agent's notes-to-self are shaped differently from facts about the
user, and agents confuse the two unless the schema separates them:

- `agent` — the agent's own operational notes (environment quirks,
  tool conventions, things it learned the hard way). Grown by the
  agent itself.
- `user` — facts about the operator (preferences, communication style,
  workflow). Grown from explicit "remember that I…" statements and
  from auto-flush turns reading inbox/outbox.

Both dimensions share the tier + category taxonomy.

Tier semantics (direct port from openclaw):

- `long_term` — curated, durable; always a candidate for spawn-time
  injection.
- `daily` — operational log; auto-expires at 30 days unless promoted.
- `signal` — raw dreaming input; never injected, only ever promoted or
  pruned.

Category is orthogonal, borrowed from Claude Code's global memory
system but minus `user` (promoted to `dimension`): `feedback`,
`project`, `reference`, `fact`, `preference`, `other`.

CRUD surface — one Go package, one CLI, one runner-tool shape:

```go
// internal/memory/memory.go
type Memory struct { ID, AgentID, Tier, Category, Title, Body, Source string; Tags []string; Pinned bool; ... }
func Remember(ctx, pool, Memory) (int64, error)
func Get(ctx, pool, agentID string, id int64) (Memory, error)
func List(ctx, pool, agentID, tier, category string, limit int) ([]Memory, error)
func Update(ctx, pool, id int64, patch Patch) error
func Forget(ctx, pool, id int64) error
```

CLI:

```
maquinista memory remember <agent-id> --tier long_term --category feedback --title "…" --body "…"
maquinista memory list <agent-id> [--tier …] [--category …]
maquinista memory search <agent-id> "query"     (FTS; Phase 2)
maquinista memory show <agent-id> <id>
maquinista memory forget <agent-id> <id>
maquinista memory pin <agent-id> <id>
```

Runner tool (exposed by the sidecar/MCP surface planned in
`maquinista-v2.md`, or as a shell wrapper that shells out to the CLI):

```
memory_remember({tier, category, title, body, tags?, pinned?})
memory_list({tier?, category?, limit?})
memory_search({query, limit?})
memory_forget({id})
```

### Phase 2 — Search

Postgres FTS (`tsv` column, GIN index) covers BM25-style queries with
zero new infrastructure. Query:

```sql
SELECT id, dimension, tier, category, title,
       ts_headline('simple', body, q, 'MaxWords=30') AS snippet,
       ts_rank(tsv, q) AS rank
FROM agent_memories, plainto_tsquery('simple', $2) q
WHERE agent_id = $1
  AND (expires_at IS NULL OR expires_at > now())
  AND tsv @@ q
ORDER BY pinned DESC, rank DESC, created_at DESC
LIMIT $3;
```

Vector search is **deferred**. Rationale:

- Maquinista has no embedding provider wired up today.
- pgvector is a separate extension that must be installed per-host —
  adds operator burden.
- FTS handles 80 % of retrieval use cases (exact phrase, keyword).

When we add it, stage it behind an optional migration
`016_agent_memories_vector.sql`:

```sql
CREATE EXTENSION IF NOT EXISTS vector;
ALTER TABLE agent_memories
  ADD COLUMN embedding vector(1024);       -- nullable; backfilled by a job
CREATE INDEX agent_memories_embedding_idx
  ON agent_memories USING ivfflat (embedding vector_cosine_ops)
  WITH (lists = 100);

-- Provider-keyed embedding cache (openclaw pattern —
-- memory-schema.ts:12-57 keys on provider+model+hash).
CREATE TABLE embedding_cache (
  provider     TEXT NOT NULL,         -- 'openai' | 'voyage' | …
  model        TEXT NOT NULL,         -- 'text-embedding-3-large' | …
  content_hash BYTEA NOT NULL,        -- sha256 of body
  embedding    vector(1024) NOT NULL,
  dims         INTEGER NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, model, content_hash)
);
```

The cache table is the load-bearing piece — embedding API calls are
the expensive part, and the same body text reappears constantly
(duplicates across agents, re-imports, pin/unpin churn). Look up by
`(provider, model, sha256(body))` before hitting the provider; insert
on miss. Keep the Phase-2 FTS path as the fallback when no embedding
provider is configured. Providers gate on
`cfg.Memory.EmbeddingProvider` (`openai`|`voyage`|`none`).

### Phase 3 — Spawn-time injection (frozen snapshot)

At every reconcile spawn (`cmd_start_reconcile.go`, per
`multi-agent-registry.md` Phase 2), fetch a bounded slice of memory
and append it to the rendered soul prompt. **The slice is frozen
for the lifetime of the tmux process** — new memory rows written
mid-session update the DB but do **not** rewrite the tempfile. This
preserves the runner's prefix cache across turns (pattern: hermes
`memory_manager.py:13-14`, openclaw bootstrap injection — both
explicitly document "snapshot at session start, not per-turn").

```go
// Pulled in priority order, capped at cfg.Memory.InjectMaxChars (default 8 KB).
rows := memory.FetchForInjection(ctx, pool, agentID, FetchPolicy{
  IncludePinned:       true,
  IncludeLongTerm:     cfg.Memory.InjectLongTerm,     // default true
  IncludeDailyRecent:  cfg.Memory.InjectDailyDays,    // default 2 (today + yesterday)
  AgentDimensionMax:   cfg.Memory.AgentMaxChars,      // default 2200 (hermes default)
  UserDimensionMax:    cfg.Memory.UserMaxChars,       // default 1375 (hermes default)
  MaxChars:            cfg.Memory.InjectMaxChars,     // overall cap
})
```

Injection format, appended after the soul, split by dimension so the
model treats user facts and agent notes separately:

```
## Memory — about you (user)

### Pinned
- [preference] prefer pt-BR on Telegram, English elsewhere  (2026-04-15)
- [feedback]   don't summarize the diff at end of turn      (2026-03-01)

## Memory — my notes (agent)

### Pinned
- [reference] pipeline bugs tracked in Linear "INGEST"      (2026-02-28)

### Recent (today + yesterday)
- [project] merge freeze starts 2026-03-05                  (2026-04-15)
- [fact]    claude 4.6 has 1M context flag                  (2026-04-15)
```

Rendering rules:

- Pinned rows always included (up to 32 rows per dimension) regardless
  of character cap; they're the agent's non-negotiable memory.
- Long-term rows ranked by `score DESC, created_at DESC`, truncated to
  fit the dimension's char budget.
- Daily rows pulled for the last N days (config), newest first.
- Signal tier **never** injected.

When overflow still happens after per-dimension capping, apply the
same head/tail truncation Phase 3 of `agent-soul-db-state.md`
defines (openclaw `bootstrap.ts:86-144`) — head-heavy preserves the
highest-priority pinned rows at the top.

**Cross-agent sharing (explicit, not accidental).** A sub-agent
spawned via `agent-to-agent-communication.md` Phase 4 is explicitly
blocked from writing to its parent's `agent_memories` rows (see that
plan's blocked-tools list, pattern stolen from hermes
`delegate_tool.py:32-38`). A parent *can* pin a row in the child's
memory at spawn time via `memory_share(target_agent, memory_id)`,
which inserts a copy into the child's table with `source='import'`
and `source_ref='from:<parent_id>:<row_id>'`.

### Phase 5 — Shared memory via archives (Letta `archives_agents`)

Letta's cleanest multi-agent primitive: an `archives` row owns a set
of passages, and an `archives_agents` junction table grants agents
access to it (`/home/otavio/code/letta/letta/orm/archive.py:24-63`
and `passage.py:76-104`). Multiple agents share an archive without
data duplication or eventual-consistency bookkeeping.

Adopt the shape:

```sql
-- migration 017_agent_archives.sql
CREATE TABLE agent_archives (
  id              BIGSERIAL PRIMARY KEY,
  name            TEXT NOT NULL,               -- e.g. "team-conventions", "project-memory"
  description     TEXT,
  owner_agent_id  TEXT NOT NULL REFERENCES agents(id),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE archive_members (
  archive_id      BIGINT NOT NULL REFERENCES agent_archives(id) ON DELETE CASCADE,
  agent_id        TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  role            TEXT NOT NULL CHECK (role IN ('owner','writer','reader')),
  granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (archive_id, agent_id)
);

ALTER TABLE agent_memories
  ADD COLUMN archive_id BIGINT REFERENCES agent_archives(id);
-- When archive_id IS NULL → private to agent_id (existing behavior).
-- When archive_id IS NOT NULL → agent_id indicates *writer*; readership
--   determined by archive_members.
CREATE INDEX agent_memories_archive_idx ON agent_memories (archive_id)
  WHERE archive_id IS NOT NULL;
```

Retrieval: `memory_search` expands to include rows the caller
either owns (`agent_id = $caller`) or has archive-level read access to
(`archive_id IN (SELECT archive_id FROM archive_members WHERE
agent_id = $caller)`). Writes default to the caller's private scope
unless the tool call explicitly targets an archive:

```
memory_remember({…, archive: "team-conventions"})   → writes to shared archive
memory_share(archive: "team-conventions", memory_id: N)  → copies existing private row to shared
```

This supersedes the A2A plan's sketch of cross-agent `memory_share`
and gives it a proper home. Integrates cleanly with the
`agent_souls.allow_delegation` flag: sub-agents inherit read-only
access to the parent's designated archives via a `lease` parameter
at spawn time, not full write access.

### Phase 4 — Auto-flush + dreaming sweep

**Auto-flush** — a silent turn triggered before compaction (same idea as
openclaw's pre-compaction flush). Implementation:

- `internal/runner/claude.go` already invokes claude with a known
  prompt; add a hook that, when the session transcript passes a
  configurable token threshold (`cfg.Memory.FlushAfterTokens`,
  default 30 K), injects a system message: *"Before your next turn,
  call `memory_remember` for any durable facts you learned this
  session. Keep entries ≤1 paragraph. Categorize as feedback/project/
  reference/fact."* Block until the tool calls settle, then continue.

Low-cost alternative if hooking into claude's stream is fragile: run
auto-flush as a scheduled job (`scheduled_jobs` row per agent, every
2 h) that spawns a one-shot `claude -p` session reading the last N
inbox/outbox rows and asked to emit memory tool calls.

**Dreaming sweep** — a cron job (reuse `scheduled_jobs` from migration
010) that:

1. `SELECT … FROM agent_memories WHERE tier='signal' AND score > threshold`.
2. For each, calls a small model to decide: promote to `long_term`, keep
   as `signal`, or discard. Promotion rewrites tier + resets score.
3. Records decisions in a `agent_memory_events(agent_id, memory_id,
   action, rationale, at)` audit table.

Dreaming is off by default (`cfg.Memory.Dreaming.Enabled=false`).
Operators opt in per agent via `agent_settings.roster->>'dreaming'`.

## Eviction

Three mechanisms:

1. **`expires_at`** — daily-tier rows set to `now()+30d` on insert; a
   nightly cron deletes `WHERE expires_at < now()`.
2. **Manual `forget`** — operator or agent deletes a row.
3. **Size cap per agent** — `agent_settings.roster->>'memoryMaxRows'`
   (default 5000). When exceeded, oldest non-pinned `signal` rows
   evicted first, then oldest non-pinned `daily`. `long_term` is never
   auto-evicted.

No hard-delete of pinned rows under any condition. Operator-forced
deletes via CLI are audited in `agent_memory_events`.

## What we deliberately skip vs openclaw / hermes

- **No Markdown files on disk.** Everything in Postgres.
- **No separate SQLite per-agent index.** Postgres FTS (Phase 2).
- **No file-watcher reindex.** Writes are DB inserts; `tsv` is a
  generated column so indexing is free.
- **No pluggable external backend (Honcho / QMD / Mem0) in Phase 1.**
  Single DB-backed path. But leave an extensibility hook:
  `internal/memory/provider.go` defines a `Provider` interface
  (hermes `memory_provider.py:42` pattern — `Initialize`,
  `SystemPromptBlock`, `Prefetch`, `SyncTurn`, `ToolSchemas`,
  `HandleToolCall`, `Shutdown`). Phase 1 ships only the built-in
  Postgres provider; adding Honcho later becomes a plugin.
- **Max one external provider.** Hermes enforces this
  (`memory_manager.py:97-119`) and warns loudly if two are registered.
  Apply the same rule.
- **No raw JSONL session transcript.** Maquinista already persists
  inbox/outbox rows; transcripts are reconstructible from those and
  don't need a second store.

## Interaction with other plans

- **`agent-soul-db-state.md`** — soul renders first, memory second,
  per-turn context last. `Render(soul) + Render(memorySlice)` is the
  composed system prompt.
- **`multi-agent-registry.md` Phase 4** — its stubbed
  `agent_memory(agent_id, key, value)` table is replaced by this
  plan's richer schema. Kill Phase 4 from that plan; reference this
  one instead.
- **`agent-to-agent-communication.md`** — a2a conversations can cite
  memory rows by id when agents reference shared context. Specifically
  useful when delegating: the spawning agent can pin a row in the
  spawnee's memory before handoff.

## Files

New:

- `internal/db/migrations/015a_agent_blocks.sql` (Phase 0)
- `internal/db/migrations/015_agent_memories.sql` (Phase 1)
- `internal/db/migrations/016_agent_memories_vector.sql` (Phase 2 optional)
- `internal/db/migrations/017_agent_archives.sql` (Phase 5)
- `internal/memory/blocks.go` — `Block`, `Append`, `Replace`, `Read`,
  char-limit enforcement (Phase 0).
- `internal/memory/memory.go` — archival passage CRUD.
- `internal/memory/inject.go` — `FetchForInjection` + `Render` (blocks
  rendered as `<memory_blocks>` XML, archival summary appended).
- `internal/memory/archive.go` — shared archives (Phase 5).
- `internal/memory/flush.go` — auto-flush driver (Phase 4).
- `internal/memory/dreaming.go` — promotion sweep (Phase 4).
- `internal/memory/provider.go` — pluggable external-provider interface.
- `cmd/maquinista/cmd_memory.go` — cobra group.

Modified:

- `cmd/maquinista/cmd_start_reconcile.go` — renders blocks + archival
  summary into the system prompt tempfile.
- `internal/config/config.go` — `Memory` config section.
- `internal/agent/soul.go` — agent creation seeds default blocks
  (`persona`, `human`, `task-note`) alongside soul.

## Verification per phase

- **Phase 0** — `./maquinista agent add alice` → three rows in
  `agent_blocks` with labels persona/human/task-note. Send agent a
  message: "remember I prefer pt-BR". Check that a `core_memory_append`
  tool call lands, `agent_blocks.value` for `label='human'` gains the
  fact, and respawning the agent surfaces it in the rendered prompt.
- **Phase 1** — `maquinista memory remember maquinista --tier long_term
  --category feedback --title "be terse" --body "no trailing summaries"`
  then `memory list maquinista` → row visible.
- **Phase 2** — `maquinista memory search maquinista "terse"` returns
  the row with a snippet. Re-insert the same body → embedding_cache
  hit (log line or metric).
- **Phase 3** — pin the row; `./maquinista start` → tmux pane's system
  prompt tempfile contains a `## Memory / ### Pinned` section with the
  row, plus `<memory_blocks>` XML with persona/human blocks. Sending
  "what's your tone preference?" on Telegram → reply cites the memory.
- **Phase 4** — burn through 30 K tokens in a session → next turn ends
  with a silent tool call inserting a new memory row (`source='auto_flush'`).
  Flip the dreaming cron on for the test agent, wait one tick → signal
  rows with score above threshold either promoted or kept, and
  `agent_memory_events` has one row per decision.
- **Phase 5** — `maquinista archive create team-notes maquinista` →
  archive row. `maquinista archive grant team-notes alice --role reader`
  → junction row. Alice's `memory_search` now returns rows maquinista
  inserted with `archive='team-notes'`.

## Open questions

1. **Token-based flush trigger** — hard to observe reliably without
   parsing claude's stream. If we can't do it cleanly, fall back to
   the scheduled-job approach from Phase 4.
2. **Category taxonomy.** Claude Code's set works but maquinista has no
   CLAUDE.md to reference — does the agent reliably pick categories
   without guidance? May need a system-prompt snippet describing each.
3. **Cross-agent memory sharing.** Resolved in Phase 5 via archives +
   role-based membership (Letta pattern). The A2A plan's `memory_share`
   tool becomes a thin wrapper around `archive_insert`.
4. **Embedding provider choice** (Phase 2 follow-up) — pick one or
   support multi? Single provider (OpenAI or Voyage) keeps ops simple.
5. **Sidecar vs CLI wrapper for the runner tool.** If the v2 per-agent
   sidecar ships, memory tools live there natively. Until then, a
   small shell helper that shells out to `maquinista memory …` and
   prints JSON is the cheapest path.
6. **Block edit concurrency.** Letta uses an optimistic `version`
   counter (`orm/block.py:20-62`). If two tool calls fire
   `core_memory_append` in the same turn, increment-check on version
   catches the race. Adopt the same column and pattern — it's free
   once the column exists.
