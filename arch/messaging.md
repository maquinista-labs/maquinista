# Agent Messaging

## One inbox and one outbox per agent

Every agent has a single `agent_inbox` and a single `agent_outbox`. There
is no per-channel inbox. Telegram, the dashboard, and agent-to-agent
mentions all write to the same inbox table; the agent reads from one place
and writes to one place.

```
Telegram message  ──┐
Dashboard message ──┼──→  agent_inbox  ──→  [mailbox consumer]  ──→  tmux PTY
A2A mention       ──┘                                                     │
                                                                          ↓
                                                               [monitor transcript]
                                                                          │
                                                                          ↓
                                                                   agent_outbox
                                                                   /           \
                                                        [dashboard]           [relay]
                                                      reads directly       fans out to
                                                                      channel_deliveries
                                                                              │
                                                                        [dispatcher]
                                                                              │
                                                                       Telegram API
```

## Inbox row anatomy

```sql
agent_inbox (
    id              UUID        -- stable handle for in_reply_to FK
    agent_id        TEXT        -- target agent
    origin_channel  TEXT        -- 'telegram' | 'dashboard' | 'a2a'
    origin_user_id  TEXT        -- Telegram user id, operator string, or agent id
    origin_thread_id TEXT       -- Telegram forum topic thread id (telegram only)
    origin_chat_id  BIGINT      -- Telegram group chat id (telegram only)
    external_msg_id TEXT        -- idempotency key (origin_channel, external_msg_id) UNIQUE
    content         JSONB       -- {type:"text", text:"..."} or {control:"interrupt"}
    status          TEXT        -- pending → processing → processed | dead
)
```

The `origin_*` fields are only meaningful for the channel that wrote the
row. Dashboard rows have `origin_thread_id = NULL`. A2A rows have
`origin_channel = 'a2a'` and `origin_user_id` = the sending agent's id.

## Outbox row anatomy

```sql
agent_outbox (
    id              UUID
    agent_id        TEXT        -- producing agent
    in_reply_to     UUID        -- FK → agent_inbox.id of the triggering message
    content         JSONB       -- {type:"text", text:"...", role:"assistant"|"thinking"}
    mentions        JSONB       -- [{agent_id, text}] parsed from content
    status          TEXT        -- pending → routing → routed
)
```

`in_reply_to` is the critical routing hint. The relay uses it to find the
inbox row and extract `origin_chat_id + origin_thread_id` for Telegram
origin delivery.

## How a message moves through the system

1. **Ingest** — Telegram bot handler or dashboard API writes a row to
   `agent_inbox` with `status='pending'`. A `NOTIFY agent_inbox_new` fires.

2. **Consume** — `mailbox_consumer` (single global goroutine) wakes on the
   notification, claims the row (`FOR UPDATE SKIP LOCKED`), records the
   row id in `ActiveInboxMap[agentID]`, sends the text to the agent's tmux
   window via `SendKeysWithDelay`, then acks the row.

3. **Respond** — Claude runs inside tmux, produces transcript output. The
   monitor's transcript tailer sees the assistant turn, calls
   `OutboxWriter`, which appends a row to `agent_outbox`. It reads
   `ActiveInboxMap[agentID]` to stamp `in_reply_to`. A `NOTIFY
   agent_outbox_new` fires.

4. **Relay** — `maquinista relay` daemon wakes, claims the outbox row,
   runs `fanoutDeliveries`:
   - **Origin leg** — if `in_reply_to` points to a `telegram` inbox row,
     insert one `channel_deliveries` row using `origin_chat_id +
     origin_thread_id`.
   - **Binding leg** — for each `topic_agent_bindings` row with
     `binding_type IN ('owner','observer')`, insert one
     `channel_deliveries` row. This leg fires regardless of `in_reply_to`.
   - Also parses `[@handle: ...]` mentions and enqueues them as new
     `agent_inbox` rows for the target agents.

5. **Dispatch** — `maquinista dispatch` daemon wakes, claims each
   `channel_deliveries` row, calls `TelegramClient.SendMessage(chatID,
   threadID, text)`.

6. **Dashboard** — reads `agent_outbox` directly via
   `GET /api/agents/:id/outbox`. Does not go through
   `channel_deliveries`. Status filter: none — all rows regardless of
   relay status are visible.

## Channel delivery vs direct read

| Consumer | Path | Needs relay+dispatcher? |
|----------|------|------------------------|
| Telegram | `channel_deliveries` → dispatcher → Bot API | Yes |
| Dashboard | `agent_outbox` direct SQL query | No |
| A2A | relay parses mentions → new `agent_inbox` rows | Yes (relay only) |

This means the dashboard shows agent responses immediately once the outbox
row exists, even if the relay and dispatcher are not running.

## Telegram topic bindings

`topic_agent_bindings` maps agents to Telegram forum topics. The relay
uses the binding leg to deliver responses even when there is no triggering
inbox row (e.g. a cron-triggered agent with no inbound message).

```sql
topic_agent_bindings (
    agent_id    TEXT
    binding_type TEXT   -- 'owner' | 'observer'
    user_id     TEXT    -- Telegram user id (required for relay fanout)
    thread_id   TEXT    -- Telegram forum topic message_thread_id
    chat_id     BIGINT  -- Telegram group chat id
)
-- UNIQUE (user_id, thread_id) WHERE binding_type = 'owner'
```

Dashboard-spawned agents have no binding at creation time. The
`RunTopicProvisioner` background goroutine (15 s interval):
- **Creates** topics for agents without an owner binding.
- **Closes** topics and removes bindings for agents that are archived,
  dead, or deleted so the relay stops delivering to them and the Telegram
  group stays clean.

## in_reply_to is a routing hint, not required

The relay's binding leg does not need `in_reply_to`. An agent with an
owner binding will always deliver to its Telegram topic regardless. The
origin leg (stamped via `ActiveInboxMap`) is an optimization that routes
responses back to the specific Telegram topic that sent the triggering
message — useful when the same agent is reachable from multiple topics.

## Key files

| File | Role |
|------|------|
| `internal/mailbox/mailbox.go` | Typed wrappers for all inbox/outbox DB ops |
| `internal/mailbox/active_inbox.go` | `ActiveInboxMap` — tracks active inbox row per agent for `in_reply_to` stamping |
| `cmd/maquinista/mailbox_consumer.go` | Claims inbox rows, drives to tmux, updates ActiveInboxMap |
| `internal/monitor/outbox.go` | `NewDBOutboxWriter` — appends outbox rows from transcript events |
| `internal/relay/relay.go` | Fans out outbox rows to `channel_deliveries` |
| `internal/dispatcher/dispatcher.go` | Sends `channel_deliveries` rows via Telegram Bot API |
| `internal/bot/topic_provisioner.go` | Creates Telegram topics + bindings for dashboard agents |
| `internal/bot/handlers_inbox.go` | Telegram → `agent_inbox` ingestion |
| `internal/dashboard/web/src/app/api/agents/[id]/inbox/route.ts` | Dashboard → `agent_inbox` ingestion |
| `internal/dashboard/web/src/app/api/agents/[id]/outbox/route.ts` | Dashboard reads `agent_outbox` directly |
| `internal/db/migrations/009_mailbox.sql` | Schema for all mailbox tables |
