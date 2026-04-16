# Maquinista v2 — DB-Backed Mailbox & Outbox Architecture

Design note comparing the current maquinista topology against tinyclaw's filesystem mailbox model, then diverging on the substrate: **Postgres as the message bus** via the transactional outbox + inbox pattern, `LISTEN/NOTIFY`, and `SELECT … FOR UPDATE SKIP LOCKED`.

Status: **draft** — written for iteration.

---

## 0. Principle: Postgres is the system of record

**Persistent state is a Postgres row.** Never a markdown file, never JSON on disk, never a dotfile. The database is the system of record. Every value that has to survive a bot restart — bindings, sessions, memory, soul, skills, checkpoints, configuration, scheduled jobs, per-topic overrides — is a table column, not a filesystem artifact. Markdown is for humans reading documentation; tables are for the system to read. If a feature would introduce "we scan some files under `~/.maquinista/*.md` at spawn time," redesign it as a table.

Rationale:

- One source of truth. No file/DB divergence, no stale caches.
- Transactional updates across related fields.
- Queryable from operators, dashboard, schedulers, tests.
- Multi-writer-safe without flocks or replace-and-pray.
- Schema evolves through migrations, not ad-hoc file-format parsers.
- Restart reliability: if the daemon dies mid-write, Postgres is the arbiter.

The only permitted exceptions are ephemeral, process-local artifacts (monitor byte offsets, transcript tailing cursors) — and per `json-state-migration.md` even those are moving to Postgres. Every plan under `plans/` adheres to this principle.

---

## 1. Context & goals

Maquinista today hard-couples three things: one Telegram topic ↔ one tmux window ↔ one Claude Code process. The bot dispatches synchronously via `tmux send-keys`; the monitor captures output by pane-scraping + Claude JSONL tailing. This leaks runtime concerns (panes, processes, file offsets) into product concerns (topics, agents, conversations).

Tinyclaw (`/home/otavio/code/tinyclaw`) demonstrates a cleaner topology but uses a filesystem mailbox (`~/.tinyclaw/queue/{incoming,processing,outgoing}/*.json`) as the substrate. That choice is ergonomic for a single-node Node.js app; it does not survive contact with: concurrent writers, exactly-once delivery requirements, transactional consistency across business state + messaging, HA consumers, observability of in-flight messages, or retention/audit.

We already run Postgres for tasks, worktrees, planner sessions, observations, and agents. **Postgres is the right substrate** for our mailbox too. This doc enumerates how.

**Hard constraints** (from user decisions):

- Preserve Postgres + task DAG + orchestrator + worktrees + merge commands + planner/executor roles.
- **Single agent-lifetime model: α** — long-lived agent (tmux pane + Claude) + per-agent sidecar that pumps the DB mailbox into/out of the pty. Per-message CLI invocation (β) and dual-mode (γ) are explicitly out of scope. Features in the current codebase that assume a non-interactive / one-shot execution path will be removed (see §10a).
- **No filesystem-based communication**. Every piece of state tinyclaw keeps in files goes in Postgres instead (messages, conversations, session IDs, attachments).
- Doc lives at `plans/reference/maquinista-v2.md`.

---

## 2. Current maquinista coupling

| Concern | File:Line | What it enforces |
|---|---|---|
| Topic → window binding | `internal/state/state.go:100-145` | `ThreadBindings[userID][threadID] → windowID` (memory + `state.json`) |
| Inbound dispatch | `internal/bot/handlers.go:37,52` | `tmux.SendKeysWithDelay(session, windowID, text, 500)` — no buffer |
| Window name = agent ID | `internal/agent/agent.go:49,65` | Agent ID doubles as tmux window name |
| Agent liveness | `internal/orchestrator/orchestrator.go:147` | `!tmux.WindowExists(...)` → mark dead |
| Output capture | `internal/monitor/source_claude.go:43-76` | Reads `~/.claude/projects/*/<session>.jsonl`, keyed by `session:windowID` |
| Session map | `hook/hook.go` + `internal/state/session_map.go` | Claude SessionStart hook writes to `session_map.json` |
| Per-user Telegram send queue | `internal/queue/queue.go` | One goroutine per userID; merges by windowID |

Relevant existing DB objects:

- Migration 001: `tasks`, `task_deps`, `task_context`, `agents`.
- Migration 004: `NOTIFY task_events`, `NOTIFY planner_events` — pattern already in use.
- Migration 007: `topic_agent_bindings (topic_id, agent_id, binding_type)` — observer table. **Will be extended**, not duplicated.
- Migration 008: `agent_role` on `agents`.

Next migration slot: `009`.

---

## 3. Tinyclaw topology (file-based) — why we diverge

Tinyclaw's substrate:

```
~/.tinyclaw/queue/
├── incoming/       JSON files: telegram_<id>.json            ← user message
├── processing/     JSON files in flight (crash replay buffer)
└── outgoing/       JSON files: telegram_<id>_<ts>.json       ← agent response

~/.tinyclaw/<agent>/
├── topic_sessions.json    { topicId: sessionId } per-topic Claude sessions
├── reset_flag             transient marker
└── AGENTS.md / SOUL.md / heartbeat.md   agent persona files

~/.tinyclaw/chats/<team>/<ts>.md    conversation aggregation history
~/.tinyclaw/files/                  binary attachments
```

The queue processor (`src/queue-processor.ts`) polls `incoming/` every 1s; the telegram client (`src/channels/telegram-client.ts`) polls `outgoing/` every 1s. Atomicity = `rename`. Crash recovery = replay `processing/`.

Why filesystem is insufficient for maquinista:

| Need | Filesystem | Postgres |
|---|---|---|
| Atomic "write business state + enqueue message" | impossible across tables + files | one TX |
| Concurrent workers w/o double-processing | `O_EXCL` + lock files, brittle | `FOR UPDATE SKIP LOCKED` |
| Low-latency wake (no polling) | inotify / fsnotify (platform-specific) | `LISTEN/NOTIFY` |
| Exact-once delivery | requires disciplined dedupe + ordering | unique constraints + idempotency keys |
| Fan-out to N subscribers | copy files N times | 1 outbox row → N delivery rows |
| In-flight observability | `ls` | any SQL query |
| Retention / audit / replay | manual | `WHERE created_at > …` |
| Backup / DR | separate concern | already covered by DB backups |

Tinyclaw's topology — routing, addressing, per-topic session IDs, agent-to-agent handoff — is the part we adopt. The storage substrate is what we replace.

---

## 4. Architecture diagrams

### 4.1 Tinyclaw (filesystem substrate)

```
  ┌─────────────────┐                 ┌──────────────────────┐
  │  Telegram User  │                 │    Telegram User     │
  │  topic "coder"  │                 │  topic "coder-ext"   │
  └────────┬────────┘                 └──────────┬───────────┘
           │                                     │
           ▼                                     ▼
  ┌──────────────────────────────────────────────────────────┐
  │          telegram-client.ts   (long-lived process)       │
  │   - resolve topic → agent_id (by name/defaults/map)      │
  │   - write file  ~/.tinyclaw/queue/incoming/tg_<id>.json  │
  └──────────────────────────┬───────────────────────────────┘
                             │
                             ▼   (polled every 1s; rename to processing/)
                   ┌─────────────────────┐
                   │   incoming/*.json   │
                   └──────────┬──────────┘
                              │
                              ▼   mv → processing/
  ┌────────────────────────────────────────────────────────────┐
  │            queue-processor.ts  (long-lived)                │
  │   - parse @agent_id, resolve team                          │
  │   - read <agent>/topic_sessions.json → sessionId           │
  │   - spawn:  claude --session-id <sid> -p <msg>             │
  │     (NEW PROCESS PER MESSAGE; working dir = <agent>)       │
  │   - capture stdout → response string                       │
  │   - scan for [@teammate: ...] → enqueue internal msg       │
  │   - write file ~/.tinyclaw/queue/outgoing/tg_<id>_<ts>.json│
  └──────────────────────────┬─────────────────────────────────┘
                             │
                             ▼   (polled every 1s)
                   ┌─────────────────────┐
                   │   outgoing/*.json   │
                   └──────────┬──────────┘
                              │
                              ▼
  ┌──────────────────────────────────────────────────────────┐
  │          telegram-client.ts  (same long-lived proc)      │
  │   - match messageId → pending map → chatId/topicId       │
  │   - bot.sendMessage(...)                                 │
  │   - delete outgoing file                                 │
  └──────────────────────────┬───────────────────────────────┘
                             │
                             ▼
                     ┌────────────────┐
                     │ Telegram topic │
                     └────────────────┘

  State outside queue:
    <agent>/topic_sessions.json    { "101": "sid-abc", "202": "sid-def" }
    chats/<team>/<ts>.md            aggregated handoff transcripts
    files/tg_<id>_<name>            binary attachments
    stop/<agent>.stop               stop signal flag files
```

Every arrow between processes is a file. Every piece of state is a JSON file.

### 4.2 Maquinista v2 (Postgres substrate)

```
  ┌─────────────────┐                 ┌──────────────────────┐
  │  Telegram User  │                 │    Telegram User     │
  │  topic @exec-1  │                 │   topic (observer)   │
  └────────┬────────┘                 └──────────▲───────────┘
           │                                     │
           ▼                                     │
  ┌────────────────────────────┐                 │
  │    maquinista bot          │                 │
  │  (existing long-lived)     │                 │
  │  resolve (user_id,         │                 │
  │  thread_id) → agent_id via │                 │
  │  topic_agent_bindings      │                 │
  │                            │                 │
  │  INSERT INTO agent_inbox…  │                 │
  │  (single TX)               │                 │
  └────────────┬───────────────┘                 │
               │                                 │
               ▼                                 │
  ┌──────────────────────────────────────────┐   │
  │  Postgres                                │   │
  │  ┌────────────────────────────────────┐  │   │
  │  │ agent_inbox                        │  │   │
  │  │  id, agent_id, conv_id, from,      │  │   │
  │  │  origin_topic, content(jsonb),     │  │   │
  │  │  status, claimed_by, attempts      │  │   │
  │  └────────────────┬───────────────────┘  │   │
  │                   │ NOTIFY agent_inbox    │   │
  │                   │ (payload=agent_id)    │   │
  └───────────────────┼──────────────────────┘   │
                      │                          │
                      ▼                          │
  ┌──────────────────────────────────────────┐   │
  │ Agent sidecar (one per live agent)       │   │
  │   LISTEN agent_inbox                     │   │
  │   SELECT … FROM agent_inbox              │   │
  │     WHERE agent_id=$ AND status=pending  │   │
  │     FOR UPDATE SKIP LOCKED               │   │
  │   pipe content into the agent's pty;     │   │
  │   tail Claude JSONL for response;        │   │
  │   within TX:                             │   │
  │     INSERT INTO agent_outbox(…)          │   │
  │     UPDATE agent_inbox SET status=done   │   │
  └────────────────┬─────────────────────────┘   │
                   │                              │
                   ▼                              │
  ┌──────────────────────────────────────────┐   │
  │  agent_outbox                            │   │
  │   id, agent_id, in_reply_to, conv_id,    │   │
  │   content(jsonb), mentions(jsonb),       │   │
  │   status                                 │   │
  │                  │ NOTIFY agent_outbox   │   │
  └──────────────────┼───────────────────────┘   │
                     │                            │
                     ▼                            │
  ┌──────────────────────────────────────────┐   │
  │ Outbox relay                             │   │
  │   LISTEN agent_outbox                    │   │
  │   fan-out in TX:                         │   │
  │     for each mention → INSERT agent_inbox│   │
  │     for each subscriber (owner/observer) │   │
  │       → INSERT channel_deliveries        │   │
  │   UPDATE agent_outbox SET status=routed  │   │
  └────────────────┬─────────────────────────┘   │
                   │                              │
                   ▼                              │
  ┌──────────────────────────────────────────┐   │
  │  channel_deliveries                      │   │
  │   outbox_id, channel, chat_id,thread_id, │   │
  │   mode (owner|observer), status, attempts│   │
  │                  │ NOTIFY channel_deliv  │   │
  └──────────────────┼───────────────────────┘   │
                     │                            │
                     ▼                            │
  ┌──────────────────────────────────────────┐   │
  │ Telegram dispatcher                      │───┘
  │   LISTEN channel_delivery_new            │
  │   SELECT … FOR UPDATE SKIP LOCKED        │
  │   bot.Send(chatId, thread_id, content)   │
  │   UPDATE status='sent', external_msg_id  │
  └──────────────────────────────────────────┘
```

Every arrow is a SQL statement or `LISTEN/NOTIFY` event. Every piece of state is a row. Fan-out is N rows in `channel_deliveries` from one `agent_outbox` row — in the same transaction as the routing decision.

### 4.3 Side-by-side substrate comparison

```
                      TINYCLAW                           MAQUINISTA v2
                  ──────────────────                ─────────────────────
   enqueue   :  fs.writeFile(tmp);              BEGIN; INSERT; COMMIT;
              fs.rename(tmp, final)             pg_notify('agent_inbox', agent_id)

   claim     :  fs.readdir; fs.rename            SELECT … FOR UPDATE SKIP LOCKED
              → processing/                     UPDATE status='processing', claimed_by

   ack       :  fs.unlink                        UPDATE status='processed', processed_at

   fail      :  fs.rename back to incoming       UPDATE status='pending', attempts++

   fanout    :  copy file N times                INSERT … SELECT (one row per subscriber)

   wake      :  1s poll                          LISTEN / NOTIFY  (sub-ms latency)

   attach    :  files/<id>-<name>                bytea in message_attachments
                                                 (or LO for >10MB)

   session   :  agent/topic_sessions.json        agent_topic_sessions table

   handoff   :  regex [@agent: msg] + new file   regex + INSERT INTO agent_inbox
              inside queue-processor             inside outbox relay, same TX as fanout

   restart   :  replay processing/ → incoming/   outstanding rows where status='processing'
                                                 AND claimed_at < now()-lease_expiry
                                                 → UPDATE to 'pending'
```

---

## 5. The outbox + inbox pattern, applied

### 5.1 Transactional outbox

Classic pattern: whenever an aggregate's state changes in a way that must produce an external event, the event row is inserted **in the same DB transaction** as the state change. A separate relay process ships those rows out-of-band.

In maquinista v2:

- Bot receives Telegram message → in **one TX**: insert `agent_inbox` row (+ store attachments). If anything fails, nothing is enqueued. Telegram's `update_id` serves as idempotency key.
- Agent worker produces a response → in **one TX**: insert `agent_outbox` row, update `agent_inbox` row to `processed`, persist any new `task_context` rows. If the worker crashes after TX commit, the response is durable; if it crashes before commit, the inbox row stays `pending` (or `processing` with expired lease) and is retried.
- Outbox relay fans out → in **one TX**: insert N `channel_deliveries` + M `agent_inbox` (for mentions) + update `agent_outbox` row to `routed`. Fan-out is atomic.

### 5.2 Inbox pattern (consumer idempotency)

Every consumer deduplicates by a natural key:

- `agent_inbox` has `UNIQUE (origin_channel, external_msg_id)` preventing double-enqueue of the same Telegram message.
- `channel_deliveries` has `UNIQUE (outbox_id, channel, chat_id, thread_id)` preventing double-send.
- Relay writes are `INSERT … ON CONFLICT DO NOTHING` on these keys.

### 5.3 Lease-based claim

Workers claim work with `FOR UPDATE SKIP LOCKED`, writing `claimed_by` + `claimed_at`. A reaper job (or a startup cleanup pass) moves rows where `status='processing' AND claimed_at < now() - lease` back to `pending`. This gives at-least-once delivery; combined with the inbox pattern above, effectively exactly-once from the consumer's perspective.

### 5.4 `LISTEN` / `NOTIFY`

Already used in maquinista for `task_events` and `planner_events` (migration 004). Extend the pattern:

- `NOTIFY agent_inbox_new, '<agent_id>'` — wakes the inbox worker responsible for that agent.
- `NOTIFY agent_outbox_new, '<outbox_id>'` — wakes the outbox relay.
- `NOTIFY channel_delivery_new, '<delivery_id>'` — wakes the Telegram dispatcher.

Workers fall back to a slow poll (e.g., 10s) for safety; `LISTEN/NOTIFY` is a performance optimization, not a correctness mechanism.

### 5.5 Attachments in DB

Binary content (screenshots, downloaded Telegram files, log snippets) lives in `message_attachments` as `BYTEA` for payloads up to ~10MB. Beyond that, Postgres Large Objects (`pg_largeobject` / `lo_import`). Either way, **no filesystem**. The only OS files we still read are source code (agent's working dir for task execution) — which is git-managed and not part of the messaging layer.

---

## 6. Proposed schema (migration 009)

Extends existing objects where possible. All new tables live under `internal/db/migrations/009_mailbox.sql`.

```sql
-- 009_mailbox.sql

-- --------------------------------------------------------------------
-- Extend topic_agent_bindings (migration 007) with addressing fields
-- --------------------------------------------------------------------
-- Existing shape:
--   topic_agent_bindings(topic_id BIGINT, agent_id TEXT, binding_type TEXT, created_at)
-- We need user_id + thread_id + chat_id to route Telegram correctly across users.
-- topic_id in 007 was a Telegram thread_id alone; insufficient for multi-user routing.

ALTER TABLE topic_agent_bindings
  ADD COLUMN user_id   TEXT,
  ADD COLUMN thread_id TEXT,
  ADD COLUMN chat_id   BIGINT;

-- Migrate existing rows: treat legacy topic_id as thread_id, user_id/chat_id from config.
UPDATE topic_agent_bindings SET thread_id = topic_id::TEXT WHERE thread_id IS NULL;

-- Normalize constraints.
ALTER TABLE topic_agent_bindings
  ALTER COLUMN thread_id SET NOT NULL,
  ADD CONSTRAINT topic_binding_mode_check
    CHECK (binding_type IN ('owner', 'observer'));

CREATE UNIQUE INDEX IF NOT EXISTS uq_topic_binding_owner_thread
  ON topic_agent_bindings (user_id, thread_id)
  WHERE binding_type = 'owner';
-- A (user, thread) has at most ONE owner agent; observer rows are unconstrained per agent.

CREATE INDEX IF NOT EXISTS idx_topic_binding_route
  ON topic_agent_bindings (user_id, thread_id, binding_type);

-- --------------------------------------------------------------------
-- Per-(agent, topic) runner session IDs (replaces tinyclaw's
-- topic_sessions.json and maquinista's session_map.json)
-- --------------------------------------------------------------------
CREATE TABLE agent_topic_sessions (
  agent_id    TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  user_id     TEXT        NOT NULL,
  thread_id   TEXT        NOT NULL,
  runner      TEXT        NOT NULL,      -- 'claude' | 'opencode' | 'openclaude'
  session_id  TEXT        NOT NULL,      -- runner's native session UUID
  reset_flag  BOOLEAN     NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (agent_id, user_id, thread_id)
);

-- --------------------------------------------------------------------
-- Conversations: track multi-agent handoff aggregation
-- --------------------------------------------------------------------
CREATE TABLE conversations (
  id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  origin_inbox_id   UUID        NOT NULL,           -- the triggering inbox row
  origin_user_id    TEXT        NOT NULL,
  origin_thread_id  TEXT        NOT NULL,
  origin_chat_id    BIGINT      NOT NULL,
  pending_count     INT         NOT NULL DEFAULT 1, -- decremented when participating agent responds
  aggregated        BOOLEAN     NOT NULL DEFAULT FALSE,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  closed_at         TIMESTAMPTZ
);
CREATE INDEX idx_conversations_open ON conversations(aggregated) WHERE NOT aggregated;

-- --------------------------------------------------------------------
-- Agent inbox: messages to be processed by an agent
-- --------------------------------------------------------------------
CREATE TABLE agent_inbox (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id        TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  conversation_id UUID        REFERENCES conversations(id),
  from_kind       TEXT        NOT NULL CHECK (from_kind IN ('user','agent','system')),
  from_id         TEXT,                                   -- user_id or agent_id
  origin_channel  TEXT,                                   -- 'telegram' | NULL for internal
  origin_user_id  TEXT,
  origin_thread_id TEXT,
  origin_chat_id  BIGINT,
  external_msg_id TEXT,                                   -- e.g., telegram update_id
  content         JSONB       NOT NULL,                   -- {type:'text'|'command', text, ...}
  status          TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','processing','processed','failed','dead')),
  claimed_by      TEXT,                                   -- worker id (host:pid or uuid)
  claimed_at      TIMESTAMPTZ,
  lease_expires   TIMESTAMPTZ,                            -- now() + lease on claim
  attempts        INT         NOT NULL DEFAULT 0,
  max_attempts    INT         NOT NULL DEFAULT 5,
  last_error      TEXT,
  enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  processed_at    TIMESTAMPTZ,
  UNIQUE (origin_channel, external_msg_id)               -- idempotency
);
CREATE INDEX idx_inbox_ready
  ON agent_inbox (agent_id, enqueued_at)
  WHERE status = 'pending';
CREATE INDEX idx_inbox_expired_lease
  ON agent_inbox (lease_expires)
  WHERE status = 'processing';

-- --------------------------------------------------------------------
-- Agent outbox: messages emitted by an agent, awaiting routing/fan-out
-- --------------------------------------------------------------------
CREATE TABLE agent_outbox (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id        TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  conversation_id UUID        REFERENCES conversations(id),
  in_reply_to     UUID        REFERENCES agent_inbox(id) ON DELETE SET NULL,
  content         JSONB       NOT NULL,                   -- {parts:[{type,text|image_ref}]}
  mentions        JSONB       NOT NULL DEFAULT '[]',      -- [{agent_id, text}]
  status          TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','routing','routed','failed')),
  attempts        INT         NOT NULL DEFAULT 0,
  last_error      TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  routed_at       TIMESTAMPTZ
);
CREATE INDEX idx_outbox_pending
  ON agent_outbox (created_at)
  WHERE status = 'pending';

-- --------------------------------------------------------------------
-- Channel deliveries: one row per (outbox, subscriber) to send externally
-- --------------------------------------------------------------------
CREATE TABLE channel_deliveries (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  outbox_id       UUID        NOT NULL REFERENCES agent_outbox(id) ON DELETE CASCADE,
  channel         TEXT        NOT NULL,                   -- 'telegram'
  user_id         TEXT        NOT NULL,
  thread_id       TEXT        NOT NULL,
  chat_id         BIGINT      NOT NULL,
  binding_type    TEXT        NOT NULL CHECK (binding_type IN ('owner','observer','origin')),
  status          TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','sending','sent','failed','skipped')),
  external_msg_id BIGINT,                                   -- Telegram message id returned by API
  attempts        INT         NOT NULL DEFAULT 0,
  last_error      TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sent_at         TIMESTAMPTZ,
  UNIQUE (outbox_id, channel, user_id, thread_id)
);
CREATE INDEX idx_deliveries_pending
  ON channel_deliveries (created_at)
  WHERE status = 'pending';

-- --------------------------------------------------------------------
-- Attachments in DB (replaces ~/.tinyclaw/files/)
-- --------------------------------------------------------------------
CREATE TABLE message_attachments (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  inbox_id      UUID        REFERENCES agent_inbox(id)  ON DELETE CASCADE,
  outbox_id     UUID        REFERENCES agent_outbox(id) ON DELETE CASCADE,
  name          TEXT        NOT NULL,
  mime_type     TEXT        NOT NULL,
  size_bytes    INT         NOT NULL,
  content       BYTEA,                                   -- NULL iff large_object_oid set
  large_object_oid OID,                                  -- for payloads > 10MB
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CHECK ((inbox_id IS NOT NULL)::INT + (outbox_id IS NOT NULL)::INT = 1),
  CHECK ((content IS NOT NULL)::INT + (large_object_oid IS NOT NULL)::INT = 1)
);
CREATE INDEX idx_attachments_inbox  ON message_attachments(inbox_id)  WHERE inbox_id  IS NOT NULL;
CREATE INDEX idx_attachments_outbox ON message_attachments(outbox_id) WHERE outbox_id IS NOT NULL;

-- --------------------------------------------------------------------
-- Agent settings (replaces per-agent files like heartbeat.md, AGENTS.md)
-- --------------------------------------------------------------------
CREATE TABLE agent_settings (
  agent_id      TEXT        PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
  persona       TEXT,                                    -- SOUL.md equivalent
  system_prompt TEXT,                                    -- CLAUDE.md equivalent
  heartbeat     TEXT,                                    -- heartbeat.md equivalent
  roster        JSONB       NOT NULL DEFAULT '[]',       -- AGENTS.md equivalent (teammate list)
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Note: an earlier draft carried an `is_default BOOLEAN` column used by the
-- old tier-3 "global default agent" routing. Dropped in migration 013 per
-- per-topic-agent-pivot.md — tier 3 now spawns a fresh agent per topic.

-- --------------------------------------------------------------------
-- NOTIFY triggers
-- --------------------------------------------------------------------
CREATE OR REPLACE FUNCTION notify_agent_inbox()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' OR (TG_OP = 'UPDATE' AND NEW.status = 'pending' AND OLD.status != 'pending') THEN
    PERFORM pg_notify('agent_inbox_new', NEW.agent_id);
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER on_agent_inbox_notify
AFTER INSERT OR UPDATE OF status ON agent_inbox
FOR EACH ROW EXECUTE FUNCTION notify_agent_inbox();

CREATE OR REPLACE FUNCTION notify_agent_outbox()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM pg_notify('agent_outbox_new', NEW.id::TEXT);
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER on_agent_outbox_notify
AFTER INSERT ON agent_outbox
FOR EACH ROW EXECUTE FUNCTION notify_agent_outbox();

CREATE OR REPLACE FUNCTION notify_channel_delivery()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM pg_notify('channel_delivery_new', NEW.id::TEXT);
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER on_channel_delivery_notify
AFTER INSERT ON channel_deliveries
FOR EACH ROW EXECUTE FUNCTION notify_channel_delivery();

-- --------------------------------------------------------------------
-- Stop signals (replaces ~/.tinyclaw/stop/<agent>.stop files)
-- --------------------------------------------------------------------
ALTER TABLE agents ADD COLUMN stop_requested BOOLEAN NOT NULL DEFAULT FALSE;
CREATE OR REPLACE FUNCTION notify_agent_stop()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.stop_requested AND NOT OLD.stop_requested THEN
    PERFORM pg_notify('agent_stop', NEW.id);
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER on_agent_stop_notify
AFTER UPDATE OF stop_requested ON agents
FOR EACH ROW EXECUTE FUNCTION notify_agent_stop();
```

### Row-count estimates (per day, single-user deployment)

- `agent_inbox` / `agent_outbox`: O(10³)
- `channel_deliveries`: O(10³ · subscribers_per_agent)
- `message_attachments`: O(10²), mostly small screenshots
- `agent_topic_sessions`: O(10) — bounded by agents × topics
- `conversations`: O(10²)

A weekly archive job (see `docs/retention.md` — not yet written) moves rows older than N days to a history schema.

---

## 7. Approach: long-lived agent + DB mailbox (α)

The agent process stays alive — tmux pane running Claude (or another supported interactive runner) — and a dedicated **sidecar** per agent is the sole consumer of that agent's mailbox. The sidecar is the only piece of code that touches the pty; the bot, orchestrator, outbox relay, and Telegram dispatcher never interact with tmux.

Sidecar lifecycle:

- Starts when the orchestrator spawns an agent; exits when the agent is stopped.
- Subscribes via `LISTEN agent_inbox_new` (falling back to a 10s poll).
- On wake, claims pending rows for its agent with `FOR UPDATE SKIP LOCKED` and a lease (`claimed_by`, `claimed_at`, `lease_expires`).
- Pipes the message content into the pty (keystrokes for text; injects slash-commands verbatim; handles paste-chunking for large inputs).
- Tails the Claude JSONL transcript (today's pattern from `internal/monitor/source_claude.go`) to capture the response; the moment a terminal turn-end marker is observed, commits one TX that inserts `agent_outbox`, marks the inbox row `processed`, and records `task_context` observations.
- Attachments (screenshots, agent-authored files) go into `message_attachments` in the same TX via `INSERT … RETURNING id`.

Why this is the right (and only) model for maquinista:

- Preserves the interactive Claude UX that planner/executor flows already depend on: plan mode, mid-session approvals, streaming, `Esc to interrupt`.
- Keeps native session continuity — no need to thread `--session-id` through every invocation; the pty process is the session.
- Sidecar crash is isolated: the agent pane survives, and restarting the sidecar replays rows in `status='processing'` with expired leases.
- One sidecar per agent matches the "one pane per agent" constraint of tmux and removes any need for worker-fan-out contention on a single agent.
- Under the per-topic-agent pivot (see `per-topic-agent-pivot.md`) the agent↔topic arity collapses to 1:1: one pty serves one conversation. The sidecar loop is unchanged, but "cross-thread interleaving on a shared agent" stops being a concern at all.

Consequences accepted:

- Still depends on the pty + transcript scraping. That complexity moves from the monitor into the sidecar but does not go away.
- One OS process per live agent (sidecar). Acceptable for single-user deployments; scales linearly with agents, not with messages.
- No horizontal scale of workers for a single agent; deliberate — a single pty is the physical bottleneck.

---

## 8. Flows (DB substrate)

### 8.1 Inbound message

Routing ladder (resolved in order; first hit wins). See `plans/archive/per-topic-agent-pivot.md` for the pivot that rewrites tier 3.

1. **Explicit mention** — message text starts with `@<id_or_handle>`. Matches against both `agents.id` and `agents.handle` (nullable user-assigned alias). Strip the mention, use the named agent. Does **not** write a binding.
2. **Topic owner binding** — `topic_agent_bindings` row with `(user_id, thread_id)` and `binding_type='owner'`. This is the steady state after any previous routing established the topic.
3. **Spawn per-topic agent** — no existing binding. The ladder calls `SpawnTopicAgent(user, thread, chat, cwd, runner)`: inserts an `agents` row with id `t-<chat_id>-<thread_id>`, spawns a tmux window and Claude process, and writes the owner binding. Matches the tinyclaw "conversation is the unit" shape and the hermes-agent per-session-runtime model; it replaces the earlier shared-default-agent design. Each topic gets its own pty; contexts never mix.
4. **Agent picker (explicit attach)** — user invokes `/agent_default @handle` to attach this topic to an already-running agent. Writes the owner binding to the named agent. Unknown handle returns a guidance error (never auto-spawns; spawning happens only via tier 3 on a message).

Tiers deliberately omitted (vs. tinyclaw):

- Static config mapping (`settings.json`) — the DB binding table already owns this.
- Topic-name ↔ agent-id fuzzy match — fragile under rename; prefer `/agent_default`.
- Silent "first available agent" fallback — prefer error + explicit attach.
- Global default agent — removed. The earlier `agent_settings.is_default` column is dropped in migration 013.

```
Bot receives Telegram message m
    │
    ▼  BEGIN TX
  -- 1. Explicit @id-or-handle mention in m.text?
  if m.text matches /^@([A-Za-z0-9][A-Za-z0-9_-]*)/:
      token = captured
      SELECT id FROM agents WHERE id=$token OR LOWER(handle)=LOWER($token) LIMIT 1;
      agent_id = resolved; strip mention from content
  else:
      -- 2. Topic owner binding?
      SELECT agent_id FROM topic_agent_bindings
        WHERE user_id=$u AND thread_id=$t AND binding_type='owner';
      if not found:
          -- 3. Spawn new per-topic agent.
          agent_id = SpawnTopicAgent($u, $t, $c, cwd, runner)
                   = 't-' || $c || '-' || $t   (deterministic; upsert-safe)
          INSERT INTO topic_agent_bindings
            (user_id=$u, thread_id=$t, chat_id=$c,
             agent_id=$new, binding_type='owner');
  INSERT INTO agent_inbox (agent_id, from_kind='user', origin_*,
                           external_msg_id=m.update_id, content=…)
    ON CONFLICT (origin_channel, external_msg_id) DO NOTHING;
  INSERT INTO message_attachments (inbox_id=…, content=bytea) FOR EACH file;
  COMMIT;  -- NOTIFY agent_inbox_new fires on commit
    │
    ▼
  Inbox worker wakes on LISTEN
    │
    ▼  BEGIN TX
  SELECT id FROM agent_inbox
    WHERE agent_id=$a AND status='pending'
    ORDER BY enqueued_at
    FOR UPDATE SKIP LOCKED LIMIT 1;
  UPDATE agent_inbox SET status='processing', claimed_by=$w, claimed_at=NOW(),
         lease_expires=NOW()+'5min' WHERE id=$id;
  COMMIT;
    │
    ▼  (sidecar pipes content into the agent's pty; tails JSONL transcript)
  Capture response text + any emitted attachments
    │
    ▼  BEGIN TX
  INSERT INTO agent_outbox (agent_id, in_reply_to=$inbox_id,
                            content=…, mentions=extracted);
  UPDATE agent_inbox SET status='processed', processed_at=NOW() WHERE id=$inbox_id;
  INSERT INTO task_context (task_id=$current_task, kind='observation', content=…);
  COMMIT;  -- NOTIFY agent_outbox_new
```

### 8.2 Outbox relay (fan-out)

```
Relay wakes on LISTEN agent_outbox_new
    │
    ▼  BEGIN TX
  SELECT * FROM agent_outbox
    WHERE status='pending'
    FOR UPDATE SKIP LOCKED LIMIT 1;
  UPDATE agent_outbox SET status='routing';
  -- Fan out to:
  --   (a) the originating topic of the triggering inbox row (covers tier-1
  --       @mentions where no binding exists), and
  --   (b) all subscribed topics (owner + observers).
  -- UNIQUE (outbox_id, channel, user_id, thread_id) dedupes when the origin
  -- is also a subscriber.
  INSERT INTO channel_deliveries (outbox_id, channel, user_id, thread_id,
                                  chat_id, binding_type)
  SELECT $outbox_id, 'telegram', i.origin_user_id, i.origin_thread_id,
         i.origin_chat_id, 'origin'
  FROM agent_inbox i
  WHERE i.id = $in_reply_to AND i.origin_channel = 'telegram'
  UNION
  SELECT $outbox_id, 'telegram', b.user_id, b.thread_id, b.chat_id, b.binding_type
  FROM topic_agent_bindings b
  WHERE b.agent_id = $agent_id
    AND b.binding_type IN ('owner','observer')
  ON CONFLICT (outbox_id, channel, user_id, thread_id) DO NOTHING;
  -- Agent-to-agent handoff from mentions:
  INSERT INTO agent_inbox (agent_id, conversation_id, from_kind='agent',
                           from_id=$agent_id, content=…)
  SELECT mention->>'agent_id', $conv_id, 'agent', $agent_id, mention->'text'
  FROM jsonb_array_elements($mentions) mention;
  -- Conversation accounting:
  UPDATE conversations SET pending_count = pending_count + mentions_count
    WHERE id = $conv_id;
  UPDATE agent_outbox SET status='routed', routed_at=NOW() WHERE id=$outbox_id;
  COMMIT;  -- NOTIFY channel_delivery_new AND agent_inbox_new
```

### 8.3 Channel dispatcher

```
Dispatcher wakes on LISTEN channel_delivery_new
    │
    ▼  BEGIN TX
  SELECT * FROM channel_deliveries
    WHERE status='pending'
    FOR UPDATE SKIP LOCKED LIMIT 1;
  UPDATE channel_deliveries SET status='sending', attempts=attempts+1;
  COMMIT;
    │
    ▼  telegram.SendMessage(chat_id, thread_id, render(outbox.content))
    │
    ▼  BEGIN TX
  UPDATE channel_deliveries SET status='sent', sent_at=NOW(),
         external_msg_id=$telegram_message_id
  WHERE id=$delivery_id;
  COMMIT;
```

On API failure: `status='pending'` again if `attempts < max`, else `status='failed'`. Rate-limit (HTTP 429) handled by deferred retry: `UPDATE … SET status='pending', enqueued_at = NOW() + '30s'` (add field) or a sleep in the dispatcher.

---

## 9. Integration with existing layers

| Layer | Impact |
|---|---|
| `internal/bot/handlers.go` | Replace `tmux.SendKeysWithDelay` with `INSERT INTO agent_inbox` |
| `internal/bot/window_picker.go` | → `agent_picker`; writes `topic_agent_bindings` |
| `internal/state/state.go` | `ThreadBindings` → read-through cache over `topic_agent_bindings` |
| `internal/agent/agent.go` | Spawn creates `agent_settings` row + starts the sidecar for the new pane |
| `internal/orchestrator/orchestrator.go` | Sidecar liveness (not raw `tmux.WindowExists`) drives the agent-alive signal |
| `internal/runner/` | Keep `InteractiveCommand` only; delete `NonInteractiveArgs` / `RunNonInteractive` (see §10a) |
| `internal/monitor/` | Folded into the sidecar: the JSONL tail becomes the input to `agent_outbox` inserts, not a push path to Telegram |
| `internal/queue/queue.go` | Replaced by channel dispatcher (reads `channel_deliveries`) |
| `internal/state/session_map.go` | Migrated to `agent_topic_sessions` |
| `hook/hook.go` | Writes `agent_topic_sessions` on SessionStart instead of the JSON file |
| DB migrations | `009_mailbox.sql` (described above) |
| New packages | `internal/mailbox/` (DB ops), `internal/sidecar/` (per-agent pty bridge), `internal/dispatcher/` (Telegram outbound from `channel_deliveries`) |

---

## 10. Migration path

1. **Migration 009**: apply schema. Backfill `topic_agent_bindings` (user_id, thread_id, chat_id) from `state.ThreadBindings` JSON. No behavior change.
2. **Outbox first**: monitor starts writing to `agent_outbox` alongside its existing direct-to-Telegram-queue path. Build outbox relay + channel dispatcher. Feature-flag per topic which path actually sends. Verify parity.
3. **Inbox second**: bot starts writing to `agent_inbox` behind a per-topic feature flag. Initial consumer is a thin in-process bridge that does `tmux send-keys` as today.
4. **Extract sidecar**: promote the bridge into `internal/sidecar/` as a per-agent goroutine (or subprocess) that owns the pty bridge and the JSONL tail. The monitor package collapses into the sidecar.
5. **Retire old paths**: delete `state.ThreadBindings` in-memory map, direct `tmux.SendKeysWithDelay` from bot handlers, the pane-scraping monitor path, `internal/queue/queue.go`, and `state.session_map` — alongside the non-interactive runner surface enumerated in §10a.
6. **Agent-to-agent + observers**: wire mention parsing in the outbox relay for `[@agent_id: msg]` handoff; expose `binding_type='observer'` through a `/observe <agent_id>` command so existing observation machinery folds in.

Each step is shippable independently, gated behind a flag, and testable end-to-end before widening.

---

## 10a. Features to remove (incompatible with α)

α assumes every live agent runs inside an interactive pty with Claude (or another interactive runner) and that the sidecar is the only thing driving it. The codebase and the earlier drafts of this doc carry a non-interactive / one-shot execution surface that becomes dead code under α and must be removed in the migration. Removal is **forward-looking**: it happens as part of the rollout in §10, not as a prerequisite.

| Feature | Location (current) | Why incompatible with α |
|---|---|---|
| `Runner.NonInteractiveArgs` / `Runner.RunNonInteractive` interface methods | `internal/runner/runner.go` (~L42–L47) | α never invokes a runner as a one-shot; only `InteractiveCommand` is exercised |
| `ClaudeRunner.NonInteractiveArgs` / `RunNonInteractive` | `internal/runner/claude.go` (~L36, L57) | same — interactive pty replaces `claude … -p` invocations |
| `OpenCodeRunner.NonInteractiveArgs` / `RunNonInteractive` | `internal/runner/opencode.go` (~L36, L48) | interactive-only |
| `OpenClaudeRunner.NonInteractiveArgs` / `RunNonInteractive` | `internal/runner/openclaude.go` (~L36, L57) | interactive-only |
| `CustomRunner.NonInterTpl` + `NonInteractiveArgs` / `RunNonInteractive` | `internal/runner/custom.go` (~L17, L25, L49, L54) | drop the template field and the two methods; keep `InteractiveTpl` |
| Non-interactive test suites | `internal/runner/claude_test.go`, `opencode_test.go`, `custom_test.go` (`TestXXXRunner_NonInteractiveArgs*`) | remove alongside the methods they cover |
| `--session-id`-driven one-shot invocation path | would have lived in the runner wrappers above | α uses the pty's native session; no need to thread session IDs through CLI args |
| Proposed batch / queue-processor daemon | §7.2 / §7.3 of earlier drafts, `cmd/maquinista-queue/` planned binary | never built; removed from the forward plan |
| Proposed multi-worker horizontal scaling per agent | earlier §11 risks | α is single-sidecar-per-agent by design |
| `agent_settings.lifetime_mode` column + mode-aware claim predicates | earlier §6 migration 009 draft | dropped in the current §6 schema — called out here so readers of prior drafts don't re-introduce it |
| Dual output codepaths (stream-json vs. one-shot stdout aggregation) | earlier monitor/runner design | α only needs the JSONL tail |
| Synthesized `kind='approval_request'` protocol | earlier §11 risk #2 | α preserves Claude's native approval UX inside the pty; no synthesis needed |

Follow-up: once the inbox/outbox path is live end-to-end for one agent (step 4 of §10), open a dedicated PR per row above. Keep the removals small and mechanical so each is easy to review and revert.

---

## 11. Risks & open questions

1. **Ordering guarantees**: per-agent in-order delivery holds by construction — one sidecar per agent drains one pty. Under the per-topic-agent pivot (see `per-topic-agent-pivot.md`), each topic owns its own agent, so "cross-thread ordering for the same agent" no longer arises; the earlier `thread_lock` advisory-lock follow-up was specific to the shared-default-agent model and is retired along with it.
2. **Lease expiry**: choosing the lease duration. Under α, stale `processing` rows only appear if the sidecar crashes mid-turn. Too short → duplicate processing on slow turns; too long → stuck messages after a real crash. Start at 5min, make configurable.
3. **Sidecar crash recovery**: on sidecar restart, reclaim any row where `status='processing' AND claimed_by=<this-agent-sidecar-id>`; decide per-row whether to retry (content not yet sent into the pty) or ack (response observed in the transcript but the commit TX never happened). The JSONL tail offset stored in sidecar state is the arbiter.
4. **Conversation aggregation semantics**: when does a conversation "close"? Tinyclaw decrements on each response; we port that logic into the outbox relay (`UPDATE conversations SET pending_count = pending_count - 1; IF pending_count = 0 THEN aggregate + send`). Aggregated delivery = single `channel_deliveries` row containing merged content.
5. **Attachment size**: `BYTEA` comfortable up to ~10MB per row, ~1GB absolute. Above that, Large Objects. Decide threshold (5MB?) and code both branches in `internal/mailbox/attachments.go`.
6. **Outbox retention**: `agent_inbox`, `agent_outbox`, `channel_deliveries` grow unbounded. Weekly cron moves rows older than N days to `*_history` tables (or just `DELETE` with audit to cold storage).
7. **DB as SPOF**: already true for tasks; this widens surface. Standard HA mitigations (replication, failover) are out of scope for this doc but called out.
8. **Back-compat during migration**: feature flags per topic. In-flight tmux-dispatched messages finish naturally; no need for online schema change rollback.
9. **Testability**: inbox/outbox paths testable with `testcontainers` Postgres; the sidecar's pty side can be stubbed with a fake runner that writes a known JSONL transcript. Ships with integration tests that drive end-to-end message flows.

---

## 12. Recommended next steps

1. **Commit migration 009** after schema review. Extending `topic_agent_bindings` must be done carefully — existing callers in `internal/bot/` need updates.
2. **Build `internal/mailbox/` package**: typed wrappers for `INSERT INTO agent_inbox`, claim, ack, fail, fanout, attachment I/O. Unit tests against a real Postgres.
3. **Prototype end-to-end** for one agent: bot writes inbox, sidecar reads inbox + drives tmux, sidecar writes outbox from JSONL tail, relay fans out, dispatcher sends. Compare with legacy path on a test topic behind a feature flag.
4. **Outbox relay + dispatcher** as separate subcommands (`maquinista relay`, `maquinista dispatch`). Either embed in the bot process or run as sibling processes (same binary, different subcommands).
5. **Incrementally port commands** (`/status`, `/agents`) to read from DB views (`SELECT * FROM agent_inbox WHERE agent_id=…`) rather than in-memory state.
6. **Execute §10a removals** once the end-to-end path is stable: drop the non-interactive runner surface and any dead code it reaches. Small PR per row in §10a.

---

## Appendix A — File inventory of what changes / stays / goes

### Changes

- `internal/bot/handlers.go` — mailbox `INSERT` replaces direct tmux dispatch.
- `internal/bot/window_picker.go` → `agent_picker`; owns `topic_agent_bindings` writes.
- `internal/state/state.go` — in-memory cache over DB tables.
- `internal/agent/agent.go` — spawn creates `agent_settings` and starts the sidecar for the new pane.
- `internal/orchestrator/orchestrator.go` — liveness signal comes from the sidecar, not raw `tmux.WindowExists`.
- `internal/runner/` — keep `InteractiveCommand` only; see §10a for the non-interactive surface that leaves.
- `internal/monitor/` — folded into the sidecar; the JSONL tail becomes the input to `agent_outbox` inserts.
- `internal/queue/queue.go` — deleted; replaced by `internal/dispatcher/` reading `channel_deliveries`.
- `internal/state/session_map.go` — deleted; replaced by `agent_topic_sessions`.
- `hook/hook.go` — writes `agent_topic_sessions` row on SessionStart.

### New

- `internal/db/migrations/009_mailbox.sql`.
- `internal/mailbox/` — typed DB operations (enqueue, claim, ack, fail, fanout, attachments).
- `internal/sidecar/` — per-agent pty bridge + JSONL-tail → outbox.
- `internal/dispatcher/` — Telegram outbound from `channel_deliveries`.

### Removed

- Filesystem state: `state.json`, `session_map.json`, per-agent files. All migrated to DB.
- Per-user merge queue (`internal/queue/queue.go`) — merging concerns move into the dispatcher.
- Tmux window picker (replaced by agent picker; tmux panes still used internally by the sidecar but invisible to users).
- Non-interactive runner surface (see §10a for the full list).

---

## Appendix B — Glossary

- **Agent inbox**: `agent_inbox` rows keyed by `agent_id`, consumed by the agent's sidecar.
- **Agent outbox**: `agent_outbox` rows keyed by `agent_id`, consumed by the outbox relay.
- **Channel delivery**: `channel_deliveries` row keyed by `(outbox_id, user_id, thread_id)`, consumed by the Telegram dispatcher.
- **Sidecar**: per-agent process (or goroutine) that owns the pty bridge and the JSONL tail — the sole consumer of that agent's inbox.
- **Agent id**: stable PK of `agents`. Auto-generated as `t-<chat_id>-<thread_id>` at tier-3 spawn time; immutable.
- **Agent handle**: optional user-assigned alias on `agents.handle` (lowercase `[a-z0-9_-]{2,32}`, reserved prefix `t-` forbidden). Set via `/agent_rename`. Resolvable in `@mention` lookups alongside the id.
- **Per-topic agent**: under the pivot, one `agents` row per Telegram topic — one tmux window, one Claude process, one conversation.
- **Owner topic**: `topic_agent_bindings.binding_type = 'owner'` — can send, receives. Written by tier-3 spawn or by `/agent_default`.
- **Observer topic**: `binding_type = 'observer'` — receives only.
- **Conversation**: `conversations` row tying multiple agents' turns together for aggregated delivery.
- **Outbox pattern**: transactional enqueue — business state and message are inserted in the same TX.
- **Inbox pattern**: consumer idempotency — deduplicate by unique external key before processing.
- **Lease**: a claim expiring at `lease_expires`; expired leases auto-return rows to `pending` (relevant for sidecar crash recovery only).

---

## Appendix C — Programmatic job sources (batch + reactive)

The core architecture treats the inbox as the only way to drive an agent. This appendix covers two extensions that sit **in front of** the inbox: scheduled batch jobs (cron) and reactive webhook handlers. Both reduce to the same primitive — *insert an `agent_inbox` row* — and share the same response-routing mechanism. The appendix lays out registration, schema, auth, and failure handling; it is forward-looking and not part of migration 009.

### C.1 Common primitives

Both job sources share three concerns:

- **Authoring** an `agent_inbox` row with `from_kind='system'` (for cron) or `from_kind='webhook'` (new CHECK value) and a programmatic `external_msg_id` to guarantee idempotency.
- **Live-agent requirement**: α needs the target agent's pty up. The orchestrator grows a *lazy-spawn* rule: "if `agent_inbox` has a pending row for an agent that isn't live, spawn it." Scheduled jobs can also be configured with `warm_spawn_before='PT1M'` to spawn a minute before the fire time so the first message doesn't pay the boot cost.
- **Reply routing without a Telegram origin**: both sources declare a `reply_channel` as part of their registration. On enqueue, that reply target is written into the inbox row's `origin_*` columns (convention: `origin_channel='telegram'` + coordinates of the target topic). §8.2's existing origin-based fan-out delivers the agent's response there. If `reply_channel` is null, nothing is sent to Telegram — the agent's side effects (PR comments, Instagram uploads) are the output.

Shared CHECK widening in migration 010 (not 009 — these are post-core additions):

```sql
ALTER TABLE agent_inbox DROP CONSTRAINT agent_inbox_from_kind_check;
ALTER TABLE agent_inbox ADD CONSTRAINT agent_inbox_from_kind_check
  CHECK (from_kind IN ('user','agent','system','webhook','scheduled'));
```

### C.2 Batch jobs (cron-triggered)

**Example:** *every day at 08:00 UTC, run the `publish-daily-reel` skill on the `@creator` agent; post the confirmation into the `#social-ops` topic.*

#### Registration

```sql
CREATE TABLE scheduled_jobs (
  id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name                TEXT        NOT NULL UNIQUE,
  cron_expr           TEXT        NOT NULL,          -- e.g. '0 8 * * *'
  timezone            TEXT        NOT NULL DEFAULT 'UTC',
  agent_id            TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  prompt              JSONB       NOT NULL,          -- {type:'command', text:'/publish-daily-reel'}
  reply_channel       JSONB,                         -- {channel,user_id,thread_id,chat_id} or NULL
  warm_spawn_before   INTERVAL,                      -- spawn agent this far ahead of fire; NULL = lazy
  enabled             BOOLEAN     NOT NULL DEFAULT TRUE,
  next_run_at         TIMESTAMPTZ NOT NULL,
  last_run_at         TIMESTAMPTZ,
  last_inbox_id       UUID        REFERENCES agent_inbox(id) ON DELETE SET NULL,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_scheduled_due ON scheduled_jobs (next_run_at) WHERE enabled;
```

User-facing registration paths (all write to the same table):

- Slash command from a Telegram topic: `/schedule publish-daily-reel "0 8 * * *" @creator "/publish-daily-reel"` — the invoking topic becomes the default `reply_channel`.
- CLI: `maquinista schedule add --name … --cron … --agent … --prompt … [--reply-to topic-ref]`.
- Declarative YAML under `config/schedules/*.yaml` reconciled on startup (for infra-as-code).

#### Scheduler daemon

A single `maquinista scheduler` subcommand (HA-safe via `FOR UPDATE SKIP LOCKED`):

```
loop:
  BEGIN TX
  SELECT id, agent_id, prompt, reply_channel, cron_expr, timezone
    FROM scheduled_jobs
    WHERE enabled AND next_run_at <= NOW()
    ORDER BY next_run_at
    FOR UPDATE SKIP LOCKED LIMIT 1;
  if none: COMMIT; sleep until min(next_run_at, 60s); continue
  -- Lazy-spawn check (or warm-spawn enforcement above)
  IF agent_id NOT in live_agents: orchestrator.spawn(agent_id)
  INSERT INTO agent_inbox
    (agent_id, from_kind='scheduled', from_id=$job_id::TEXT,
     origin_channel=$reply_channel->>'channel',  -- may be NULL
     origin_user_id=$reply_channel->>'user_id',
     origin_thread_id=$reply_channel->>'thread_id',
     origin_chat_id=($reply_channel->>'chat_id')::BIGINT,
     external_msg_id='sched:'||$job_id||':'||$fire_ts,  -- idempotent on re-fire
     content=$prompt)
  ON CONFLICT (origin_channel, external_msg_id) DO NOTHING
  RETURNING id INTO $inbox_id;
  UPDATE scheduled_jobs SET next_run_at = nextAfter(cron_expr, timezone, NOW()),
         last_run_at = NOW(), last_inbox_id = $inbox_id
    WHERE id = $job_id;
  COMMIT;
```

`nextAfter()` is a Go-side computation (robfig/cron semantics); the DB only stores the next timestamp.

#### Operational notes

- **Missed fires**: if the scheduler is down at 08:00 and recovers at 08:05, `next_run_at <= NOW()` is still true — the job fires once. Multi-hour outages collapse to a single catch-up fire (by design; don't replay N days of missed reels).
- **Concurrent fires**: `FOR UPDATE SKIP LOCKED` + single scheduler replica = at-most-once. The `ON CONFLICT DO NOTHING` on `external_msg_id` is a second-line defense if two schedulers race.
- **Manual run**: `UPDATE scheduled_jobs SET next_run_at = NOW() WHERE name = …` is the "run now" button.

### C.3 Reactive jobs (webhook-triggered)

**Example:** *GitHub `pull_request.opened` webhook arrives → `@reviewer` runs the `/review-pr` skill with the PR number → review summary posts to the `#code-review` topic.*

#### Registration

```sql
CREATE TABLE webhook_handlers (
  id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name                TEXT        NOT NULL UNIQUE,
  path                TEXT        NOT NULL UNIQUE,    -- '/hooks/github/pr-review'
  secret              TEXT        NOT NULL,           -- HMAC key; store encrypted at rest
  signature_scheme    TEXT        NOT NULL DEFAULT 'github-hmac-sha256',
  event_filter        JSONB,                          -- jsonb_path_match predicate on payload
  agent_id            TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  prompt_template     TEXT        NOT NULL,           -- '/review-pr {{.pull_request.number}}'
  reply_channel       JSONB,                          -- same shape as scheduled_jobs
  rate_limit_per_min  INT         NOT NULL DEFAULT 60,
  enabled             BOOLEAN     NOT NULL DEFAULT TRUE,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_webhook_path ON webhook_handlers (path) WHERE enabled;
```

Registration paths mirror scheduled jobs: slash command, CLI, or declarative YAML.

#### HTTP ingress

`maquinista webhook-serve --addr :8080` subcommand:

```
POST /hooks/*  handler:
  h = SELECT * FROM webhook_handlers WHERE path=$path AND enabled LIMIT 1
  if not h: 404
  verify HMAC(body, h.secret, h.signature_scheme); reject on mismatch → 401
  delivery_id = header X-Hub-Signature-256 (or X-Delivery-Id)   -- dedupe key
  if h.event_filter and not payload matches h.event_filter: 204  -- ignored on purpose
  prompt = render(h.prompt_template, payload)  -- Go text/template against payload jsonb
  BEGIN TX
    -- Lazy-spawn
    IF h.agent_id NOT in live_agents: orchestrator.spawn(h.agent_id)
    INSERT INTO agent_inbox
      (agent_id=h.agent_id, from_kind='webhook', from_id=h.name,
       origin_channel=h.reply_channel->>'channel',
       origin_user_id=h.reply_channel->>'user_id',
       origin_thread_id=h.reply_channel->>'thread_id',
       origin_chat_id=(h.reply_channel->>'chat_id')::BIGINT,
       external_msg_id='hook:'||h.id||':'||delivery_id,
       content=jsonb_build_object('type','command','text',prompt,'payload',payload))
    ON CONFLICT (origin_channel, external_msg_id) DO NOTHING
    RETURNING id INTO inbox_id;
  COMMIT
  return 202 {"inbox_id": inbox_id}
```

#### Auth & safety

- **HMAC verification is non-negotiable**: every handler has a per-handler secret; signature scheme is pluggable (`github-hmac-sha256`, `svix`, `stripe`, `generic-hmac`).
- **Rate limit**: per-handler token bucket (in-memory; reset per process). Excess → 429.
- **Replay protection**: the `(origin_channel, external_msg_id)` unique index doubles as idempotency. A GitHub redelivery of the same `X-Delivery-Id` is dropped silently.
- **Payload size cap**: handler-level max body size (default 1MB); beyond that the webhook serves 413 and we rely on the source to page.
- **`event_filter`**: `jsonb_path_match` against the payload lets one handler row accept only a subset of events from a shared endpoint (e.g., only `pull_request.opened` from a generic GitHub `/hooks/github` endpoint).

#### HA

Behind an HTTP load balancer, multiple `maquinista webhook-serve` replicas are safe — DB idempotency is the source of truth. Ephemeral in-memory rate-limit drift across replicas is accepted.

### C.4 Response routing summary

| Source | `from_kind` | `origin_*` set to | Where reply lands |
|---|---|---|---|
| Human Telegram message | `user` | actual Telegram topic | back to that topic (+ observers) |
| Agent `[@mention]` | `agent` | NULL | only observers of callee; initiator's topic via re-enqueue (§8.2) |
| Scheduled job | `scheduled` | `reply_channel` or NULL | configured topic; silent run if NULL |
| Webhook | `webhook` | `reply_channel` or NULL | configured topic; silent run if NULL |

The fan-out logic in §8.2 handles all four cases without branching — the only thing that changes is who populated the inbox row.

### C.5 Observability

One unified view for both sources:

```sql
CREATE VIEW job_runs AS
SELECT
  i.id                  AS inbox_id,
  i.from_kind,
  i.from_id             AS source_id,         -- scheduled_jobs.id or webhook_handlers.id (string)
  i.agent_id,
  i.enqueued_at,
  i.processed_at,
  i.status,
  i.last_error,
  o.id                  AS outbox_id,
  o.content             AS agent_response
FROM agent_inbox i
LEFT JOIN agent_outbox o ON o.in_reply_to = i.id
WHERE i.from_kind IN ('scheduled','webhook');
```

Slash commands `/jobs` and `/hooks` list registered sources; `/job-runs <name>` reads the view above for the last N executions.

### C.6 What we do not build here

- **Chained triggers** (webhook A fires agent X which triggers cron B): already emergent from the DB — agents can `INSERT INTO scheduled_jobs` or mention other agents. No extra plumbing.
- **In-flight cancellation of a scheduled run**: out of scope; stop the agent if truly needed (`agents.stop_requested=TRUE`).
- **Polling sources** (e.g., "every 5 minutes, poll the Linear API"): implement as a scheduled job whose prompt instructs the agent to do the polling. No new primitive.
- **External queue adapters** (SQS, Kafka): a separate ingress process translates those into webhook POSTs against our own handlers; nothing to add here.

---

## Appendix D — Planner → coding-agent → PR cycle

The existing planner + task DAG (migrations 001, 004, 008 — `tasks`, `task_deps`, `task_context`, `agents.role`, the `refresh_ready_tasks` trigger that cascades `done → ready`, the `task_events` NOTIFY) is preserved under v2 unchanged. Appendix D adds the dispatch edge that turns a ready task into an inbox row for a freshly-minted per-task agent, and the PR-lifecycle bindings that close the loop.

### D.1 End-to-end flow

```
Human in #project topic: "plan: add user profile page"
      │  §8.1 routing ladder → @planner
      ▼
  @planner (α agent, long-lived)
      │  Tool calls against the `maquinista tasks` surface:
      │    INSERT INTO tasks (id, title, body, worktree_path,
      │                       metadata={'target_role':'implementor'})
      │    INSERT INTO task_deps (task_id, depends_on)
      │  Outbox: "plan ready — 5 tasks queued"
      ▼
  Migration-001 trigger `refresh_ready_tasks`
      │  Cascades pending → ready as deps complete (no new trigger needed).
      │  Migration-004 NOTIFY `task_events` already fires on status change.
      ▼
  Task scheduler daemon (new — `maquinista task-scheduler`)
      │  LISTEN task_events  (fallback: 10s poll)
      │  BEGIN TX
      │    SELECT t.id, t.worktree_path, t.metadata->>'target_role' AS role
      │      FROM tasks t
      │      WHERE t.status='ready'
      │        AND NOT EXISTS (SELECT 1 FROM agents a
      │                         WHERE a.task_id = t.id AND a.status != 'dead')
      │      FOR UPDATE SKIP LOCKED LIMIT 1;
      │    orchestrator.ensure_agent(role=$role, task_id=$t.id)
      │      → mints agent_id '@impl-<t.id>', inserts agents row with
      │        (task_id=$t.id, role=$role, tmux_*, status='working'),
      │        starts sidecar with working_dir = t.worktree_path,
      │        attaches #project topic as 'observer' in topic_agent_bindings.
      │    INSERT INTO agent_inbox
      │      (agent_id='@impl-<t.id>', from_kind='system',
      │       content={type:'task', task_id:$t.id,
      │                prompt:'/work-on-task '||$t.id});
      │    UPDATE tasks SET status='claimed', claimed_by='@impl-<t.id>',
      │                     claimed_at=NOW() WHERE id=$t.id;
      │  COMMIT;  -- NOTIFY agent_inbox_new
      ▼
  @impl-<task_id> sidecar drives the pty
      │  Agent runs `gh pr create …`; the `/work-on-task` skill
      │  captures the URL and calls the typed task tool:
      │    UPDATE tasks SET pr_url=$url, pr_state='open', status='review'
      │                 WHERE id=$task_id;
      │  Outbox row → #project topic (via observer) posts the PR link.
      ▼
  GitHub webhook → Appendix C.3
      │  pull_request.opened  → INSERT agent_inbox (@reviewer, '/review-pr <n>')
      │  pull_request.merged  → INSERT agent_inbox (@pr-closer, '/close-pr <n>')
      │  pull_request.closed  → same handler with decline path
      ▼
  @reviewer leaves comments / approves (no state mutation in v2).
  @pr-closer runs its skill:
      │  UPDATE tasks SET pr_state='merged', status='done'
      │                WHERE id=(SELECT id FROM tasks WHERE pr_url=…);
      │  -- migration-001 `refresh_ready_tasks` cascades dependents to 'ready'.
      │  UPDATE agents SET status='dead', stop_requested=TRUE
      │                WHERE id='@impl-<task_id>';
      │  Outbox: "task-7 merged; unblocked task-9, task-11"
      ▼
  DELETE FROM topic_agent_bindings WHERE agent_id='@impl-<task_id>';
  Sidecar exits on seeing the stop flag; tmux pane is torn down.
```

### D.2 Schema — migration 011

```sql
-- 011_task_pipeline.sql
ALTER TABLE tasks
  ADD COLUMN worktree_path TEXT,
  ADD COLUMN pr_url        TEXT,
  ADD COLUMN pr_state      TEXT
    CHECK (pr_state IN ('open','merged','closed')) ;

-- 'review' is a new task status; existing status column has no CHECK (see 001),
-- so no ALTER is needed — document the state machine here:
--   pending → ready → claimed → review → done
--                           ↘︎ failed
COMMENT ON COLUMN tasks.status IS
  'pending | ready | claimed | review | done | failed';

CREATE INDEX idx_tasks_pr_url ON tasks(pr_url) WHERE pr_url IS NOT NULL;

-- Unique live assignment: at most one non-dead agent per task.
CREATE UNIQUE INDEX uq_agents_task_live
  ON agents(task_id) WHERE task_id IS NOT NULL AND status != 'dead';
```

No new `assigned_agent_id` on tasks — the existing `agents.task_id` back-reference is the join key. The new partial unique index enforces "one live agent per task" at the DB level.

### D.3 Components (additions to §9 integration table)

| Layer | Impact |
|---|---|
| `cmd/maquinista/` or new `task-scheduler` subcommand | Dispatch loop above — `LISTEN task_events` + claim/enqueue. Runs single-replica. |
| `internal/orchestrator/orchestrator.go` | New `ensure_agent(role, task_id)` entry point: mints `@impl-<task_id>`, spawns α agent + sidecar, attaches origin topic as observer. |
| `internal/mailbox/` | Add typed helper `EnqueueTaskMessage(task_id, agent_id)` for scheduler use. |
| `internal/tools/tasks/` (new) | Typed MCP tool surface the planner and `/work-on-task` / `/close-pr` skills call — replaces hand-crafted SQL. |
| Skills | `/work-on-task`, `/review-pr` (already in C.3), `/close-pr`. Live under `~/.claude/skills/` in the agents' working dirs. |

### D.4 Failure modes

- **Agent crashes mid-task**: sidecar exits, `agents.status='dead'`. The unique-per-task index releases the task slot; a second scheduler pass picks it up and starts a fresh `@impl-<task_id>-retry<n>` (alias bump avoids collision with the dead agent row). `tasks.attempt` increments — existing column, already bounded by `max_attempts`.
- **PR closed without merge**: `pull_request.closed` webhook routes to `@pr-closer` with a "declined" variant; it sets `pr_state='closed'` and either re-opens the task (`status='ready'`) for another attempt or escalates to the originating topic for a human call.
- **Planner writes an invalid DAG**: the typed task tool validates cycles before commit (`WITH RECURSIVE` check in SQL). Planner gets a tool error and iterates.
- **DB outage**: identical to the §11 risk — everything degrades uniformly; no special handling for the task pipeline beyond what the mailbox already inherits.

### D.5 Relationship to migration order

Lands after Appendix C is live: the PR webhook handlers depend on `webhook_handlers` from migration 010. Suggested ordering:

1. Migration 009 (mailbox — §6) + core α path (§10 steps 1–5).
2. Migration 010 (webhook/batch sources — Appendix C).
3. Migration 011 (this appendix) + task-scheduler + typed task tools.

Volta-era planner and DAG stay untouched through all three; only the *dispatch edge* out of "task ready" moves — first from direct tmux spawn to inbox-enqueue in step 1, then to per-task α agents in step 3. Each step is independently shippable behind a feature flag.

### D.6 What we do not build here

- **Resource limits per task** (CPU, memory, wall-clock): leaned on OS / systemd slices if needed; out of scope for the doc.
- **Task re-planning mid-flight**: if requirements change, cancel the task (`status='failed'`, stop agent) and have the planner replan. No in-place mutation story.
- **Cross-task coordination beyond deps**: shared locks on hot files, serialization constraints — emerge from the DAG; if they can't be expressed as deps, escalate to the planner.
- **Auto-rebase on base-branch drift**: implementor agents handle it inside their skill via `gh`; no maquinista plumbing.
