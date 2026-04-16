# Agent-to-agent communication

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

### How openclaw does it

openclaw routes all inter-agent traffic through a single in-process
queue rather than a dedicated A2A layer. The key pieces:

- **Transport** — a TypeScript in-process lane-aware FIFO. Lanes:
  `main` (default cap 4), `subagent` (cap 8), `cron`. Not persisted.
  Inbound messages live only in the queue + the session transcript
  JSONL (`~/.openclaw/agents/<agentId>/sessions/<sessionId>.jsonl`).
- **Addressing** — by **session key**, not by agent id:
  `"main"`, `"group:<id>"`, `"subagent:<uuid>"`. `sessions_list`
  discovers them at runtime.
- **Primary operations** (agent-facing tools):
  - `sessions_spawn(task, agentId?, model?, timeoutSeconds?, …)` —
    non-blocking; returns `{status: "accepted", runId, childSessionKey}`.
  - `sessions_send(sessionKey, body, timeoutSeconds?)` —
    `timeoutSeconds>0` blocks for the reply (sync), `=0` fire-and-forget.
  - `sessions_list({kind, recent})` / `sessions_history(sessionId)` —
    discovery + context reconstruction.
- **Delivery** — push-based for sub-agent *completion* (child posts an
  `announce` message back to the parent session on exit). Inbound
  messages are drained when the receiving lane has a free worker.
- **Conversation persistence** — append-only JSONL per session; no
  relational model.
- **Depth / fan-out caps** — `maxSpawnDepth=2` (main→sub→sub-sub leaf
  is not allowed to spawn), `maxChildrenPerAgent=5`. Auto-archive at
  60 min idle.
- **Modes** — `collect` (coalesce queued messages into one followup
  turn), `steer` (inject mid-run), `followup`, `interrupt`. Debounce
  `1000 ms` before firing a followup.

See `/openclaw/docs/concepts/queue.md`, `messages.md`,
`tools/subagents.md`, `session-tool.md`.

### Where maquinista stands today

The mailbox subsystem (migration 009) already covers much of what
A2A needs:

- `agent_inbox` — `from_kind` constraint allows `'agent'` (010);
  `from_id` stores the source identifier; `content` is JSONB;
  lease-based claim via `FOR UPDATE SKIP LOCKED`.
- `agent_outbox` — `mentions` (JSONB array) already carries routing
  hints; today populated by `@agent-id` parsed from the reply.
- `channel_deliveries` — per-recipient fanout of an outbox row.
- `conversations` — per-thread aggregation (`origin_inbox_id`,
  `pending_count`).
- `routing.Resolve` (`internal/routing/routing.go:71-100`) — four-tier
  ladder (mention, owner binding, global default, picker). Already
  returns the agent id an inbound message routes to.

What's missing for agent-to-agent:

1. **Producer code path.** Nothing writes `from_kind='agent'` rows
   into `agent_inbox`. When an agent mentions `@alice` in its outbox,
   the reply goes to the human channel, not to alice's inbox.
2. **Reply semantics.** No `in_reply_to` linkage threading an inter-
   agent exchange; the current `agent_outbox.in_reply_to` refs
   `agent_inbox` but there's no matching cross-agent conversation id.
3. **Sync vs async.** Maquinista's whole pipeline is async (inbox →
   claim → tmux → outbox → deliver). openclaw's `sessions_send` with
   `timeoutSeconds` blocks for a reply; that pattern needs a decision.
4. **Discovery + addressing tools.** No agent-facing `agents_list` or
   `send_to_agent` tool yet.
5. **Conversation model.** `conversations` is keyed on the origin
   inbox; a multi-turn agent-to-agent exchange needs its own
   conversation row not tied to a human-originated message.

## Scope

Three phases. Phase 1 makes agent-to-agent messages work with
fire-and-forget semantics reusing the existing mailbox. Phase 2 adds
conversations and reply threading. Phase 3 layers sync request/response
on top. Phase 4 is sub-agent spawning (deferred, notes only).

### Phase 1 — Fire-and-forget agent-to-agent via mailbox

Goal: `@alice` mentioned in `@maquinista`'s reply becomes a real
message in alice's inbox.

Wire a **fan-out step** between outbox routing and channel delivery:

1. `internal/routing/fanout.go` (new) — for each `agent_outbox` row
   entering `routing`, inspect `mentions []TEXT`. For each mention `m`:
   - Resolve `m` → agent id (use existing routing helpers).
   - If `m` is an **agent id** (row exists in `agents`) and not the
     source agent, insert an `agent_inbox` row:
     - `from_kind='agent'`, `from_id=outbox.agent_id`,
     - `content=outbox.content` (same JSONB payload),
     - `origin_channel='a2a'`, `external_msg_id=outbox.id::text`
       (dedupes on replay),
     - `conversation_id=outbox.conversation_id` (Phase 2 promotes this
       to a dedicated a2a conversation).
2. Human-targeted channels (telegram/discord/…) continue to fan out as
   today via `channel_deliveries`. An outbox row can produce both — a
   reply that says "`@alice` take a look, @otavio" delivers to Telegram
   **and** enqueues alice's inbox in the same transaction.
3. The inbox consumer (`cmd_start.go:224`) already pipes `agent_inbox`
   rows to tmux. No changes: alice's window sees an incoming message
   prefixed with `[from @maquinista]` (a small formatting helper in
   `internal/bot/handlers_inbox.go` discriminates by `from_kind`).

**Wake mechanism is already in place.** Migration 009 installed
`NOTIFY agent_inbox_new` triggers. The existing inbox consumer uses
`LISTEN agent_inbox_new` to pick up new rows immediately without
polling — this Phase piggybacks on it for free. Openclaw's in-process
lane queue (`command-queue.ts:31-112`) plays the analogous role; the
Postgres NOTIFY path is the durable, cross-process equivalent and
already battle-tested in maquinista.

New mention types in outbox:

- `@agent-id` — send to that agent's inbox (this plan).
- `@everyone` — broadcast to all `role='user' AND task_id IS NULL`
  agents (excluding sender). Fan-out becomes N inbox rows. Rate-limit
  per-source to avoid loops.
- `@human` — force-route to original origin channel even if the reply
  is agent-originated (keeps humans in the loop during a2a threads).

`from_kind='agent'` messages are treated as authoritative — they
bypass `routing.Resolve`'s global-default tier (sender already
specified the target). The routing ladder is only for inbound-from-
external messages.

### Phase 2 — Conversations + reply threading

Problem: today `conversations` rows are created per human-originated
inbox row. An agent-to-agent exchange has no human origin and needs
its own conversation grouping so context is queryable.

Schema changes:

```sql
-- migration 017_a2a_conversations.sql
ALTER TABLE conversations
  ADD COLUMN kind TEXT NOT NULL DEFAULT 'external'
    CHECK (kind IN ('external','a2a','broadcast','system')),
  ADD COLUMN participants TEXT[] NOT NULL DEFAULT '{}',
  ADD COLUMN topic TEXT,
  ADD COLUMN parent_conversation_id UUID REFERENCES conversations(id);

CREATE INDEX conversations_participants_idx ON conversations USING GIN (participants);
```

Behavior:

- First agent-to-agent message from `A` → `B` creates
  `conversations(kind='a2a', participants={A,B})` if no open one
  exists between them (match on participants set, `closed_at IS NULL`).
- Reply message (`agent_outbox.in_reply_to` set to the inbox id) reuses
  the same conversation_id.
- `@everyone` broadcasts create `kind='broadcast'` conversations —
  each recipient's reply threads back into it.
- A human can "fork" an a2a conversation into an external one by
  `/show @alice @maquinista` in Telegram; the bot reads the open a2a
  conversation, renders its outbox history, and posts it to the
  channel. (Nice-to-have; not load-bearing.)

The existing `conversation_id` FK on inbox/outbox is already present —
this phase just starts using it correctly for a2a rows.

### Phase 3 — Sync request/response

Two available implementations — pick based on how much we're willing
to bend the existing async model.

**Option A — Polling wrapper around the async pipeline.**

Add a runner tool `ask_agent(target, prompt, timeoutSeconds=30)`:

1. Enqueue an outbox row with `mentions=[target]` + `sync_token=UUID`.
2. Phase-1 fan-out inserts the inbox row with the same `sync_token` in
   `content->>'sync_token'`.
3. The tool waits on `LISTEN agent_outbox_new` (migration 009
   trigger, already shipped) and filters by
   `agent_outbox WHERE in_reply_to_sync_token=X AND agent_id=target`.
4. First match → return a **structured result** (shape stolen from
   hermes `delegate_tool.py:498`, which returns
   `{status, summary, api_calls, duration_seconds}` so the caller can
   branch on partial completion):

```json
{
  "status": "ok" | "timed_out" | "target_offline" | "rate_limited",
  "conversation_id": "uuid",
  "outbox_id": 12345,
  "body": "...peer's reply rendered to plain text...",
  "duration_seconds": 4.2,
  "attempts": 1
}
```

Timeout → return `status='timed_out'`; the async reply still
eventually lands in the same conversation (caller can fetch via
`conversation_history`).

Pros: reuses every existing primitive. No new transport.
Cons: latency bounded by tmux pipe + claude turn time (tens of
seconds). Sync expectations with multi-minute claude runs are
unrealistic; document this loudly.

**Option B — Direct agent-to-agent without going through tmux.**

Introduce a lightweight in-process pub/sub (similar to openclaw's
lane queue) for sync calls. The receiving agent's sidecar (planned
in `maquinista-v2.md` §7) listens on a pgsql NOTIFY channel and
serves requests without a full tmux round-trip.

Pros: true sync, millisecond latency.
Cons: requires sidecar work that hasn't happened. Decouples from
tmux so the pane user can't observe a2a traffic live.

**Recommendation**: ship Option A first. It's ~200 lines and gives
agents a useful "ask a peer" primitive today. Revisit Option B when
the sidecar lands.

### Phase 4 — Sub-agent spawning (deferred)

Mirror openclaw's `sessions_spawn` + hermes' `delegate_task`: spawn a
short-lived task-scoped agent, wait for it to finish, receive its
result.

Maquinista already has the foundation:

- `internal/orchestrator/ensure_agent.go` spawns task-scoped agents in
  worktrees.
- `task_deps` table threads parent/child relationships.

A `spawn_subagent(task, soul_template?, timeout?, context?)` tool
would:

1. Create a `tasks` row with `parent_task_id=current`.
2. Call `ensure_agent` to spawn a worker.
3. Wait on `LISTEN tasks_status_changed` (to be added) for
   `completed`/`failed`, bounded by `timeout`.
4. Return a **structured result** (same shape as `ask_agent`, plus
   `api_calls` and `exit_status`), stolen from hermes
   `delegate_tool.py:399-498`.

**Guardrails (blocked-tools enforcement — merge from hermes
`delegate_tool.py:32-38`).** Children spawned via `spawn_subagent`
start with a **restricted toolset**. The sub-agent's soul template is
augmented at spawn with a `blocked_tools` list; the runner filters
these from its tool manifest before the first turn:

```
blocked_tools_for_subagents = [
  "spawn_subagent",        // no recursion
  "send_to_agent",         // no cross-agent side effects unless parent grants
  "ask_agent",             // no sync A2A from children
  "memory_remember",       // no writes to shared parent memory
  "memory_share",          // ditto
  "soul_edit",             // no self-identity mutation
  "telegram_send",         // no direct human side effects
  "discord_send",          // ditto
]
```

Depth + fan-out caps (openclaw `subagent-spawn.ts:346-450` enforces
the same limits at spawn time, returning `status='forbidden'`):

- `maxSpawnDepth = 2` — root user-agent → task worker → no further.
- `maxChildrenPerAgent = 5` — per-parent active-children cap.
- Both checked via `countActiveRunsForSession(parentAgentID)` before
  insert; overflow returns `status='forbidden'` immediately.

**Credential leasing** (hermes `delegate_tool.py:421-431`). Parent
can pass a subset of its secrets / API keys to the child through an
explicit `lease` parameter; the child inherits only what's leased.
Secrets live in `agent_settings.roster->>'credentials'` today; a
future `credential_leases(parent_agent, child_agent, key_id,
granted_at, revoked_at)` audit table keeps the trail.

Auto-archive worktree on completion (already implemented). Not
included in this plan's deliverables — recorded here to fix the shape
before someone builds it ad-hoc.

## Protocol comparison — openclaw + hermes → maquinista

| openclaw / hermes concept | maquinista equivalent (this plan) |
|---|---|
| openclaw `sessions_spawn` / hermes `delegate_task` | Phase 4: task-scoped agent + `tasks` row |
| openclaw `sessions_send(key, body)` async | Phase 1: outbox `@mention` → inbox fan-out |
| openclaw `sessions_send(…, timeoutSeconds)` sync | Phase 3 Option A: polling wrapper |
| openclaw `sessions_list` | new `agents_list` tool (read `agents` + `agent_souls`) |
| openclaw `sessions_history(id)` | new `conversation_history(id)` (read `agent_inbox`/`outbox` joined on conversation_id) |
| openclaw lane queue (`main`, `subagent`, `cron`) | existing `LISTEN agent_inbox_new` + inbox `status`/`claimed_at` lease |
| JSONL session transcript | `agent_inbox` + `agent_outbox` rows |
| openclaw `announce` on sub-agent completion | Phase 4: final outbox row with `from_kind='agent'` |
| hermes delegate blocked-tools list | Phase 4: `blocked_tools_for_subagents` (above) |
| hermes delegate heartbeat thread | N/A — Postgres NOTIFY is the heartbeat; parent doesn't block a gateway |
| hermes delegate credential leasing | Phase 4: `lease` parameter + `credential_leases` audit |
| hermes delegate structured result | `ask_agent` / `spawn_subagent` return shape (Phase 3 + 4) |
| openclaw `maxSpawnDepth`, `maxChildrenPerAgent` | Phase 4: enforced in orchestrator (values 2 / 5) |
| openclaw `collect`/`steer`/`followup` modes | **not ported**; tmux pane is inherently serial |
| openclaw `session.dmScope` | N/A; maquinista routing ladder decides dm scope differently |
| openclaw debounce before followup | N/A; no followup coalescing yet — revisit if needed |

Architectural note: maquinista has one transport advantage over both
references. openclaw's in-process lane queue dies when the node
process does; hermes' delegation blocks the parent process for the
child's duration. Maquinista's `agent_inbox` + `LISTEN` path is
durable (rows survive restart) and decoupled (parent isn't holding a
socket). That's the one piece worth **not** borrowing from either.

## Runner-facing tool surface (summary)

New agent tools (Phase 1–3):

```
agents_list()                                  → [{id, soul_tagline, status, last_seen}]
send_to_agent(target, body, reply_to?)         → {outbox_id, conversation_id}   (async)
ask_agent(target, body, timeout_seconds=30)    → {body, conversation_id, status='ok'|'timed_out'}  (Phase 3)
conversation_history(conversation_id, limit=20) → [{from, body, at}]
```

Ship as CLI shell wrappers first (cheapest), migrate into the sidecar
when it exists.

## Loop prevention

Agents mentioning each other in replies is a live foot-gun. Guardrails:

1. **Per-agent outbox rate limit** — `cfg.A2A.MaxPerMinute` (default 20)
   checked before Phase-1 fan-out inserts. Excess rows still deliver
   to humans, but `@agent` mentions are dropped with a warning.
2. **Conversation depth counter** — `conversations.depth INTEGER
   DEFAULT 0`. Each agent reply increments; > `cfg.A2A.MaxDepth`
   (default 12) ends the conversation (`closed_at=now()`) and posts a
   system note to the origin channel.
3. **Mention-echo suppression** — if `outbox.in_reply_to` points to an
   inbox row whose `from_id` equals the recipient agent in mentions,
   skip that mention. Prevents trivial A→B→A→B ping-pong.

## Observability

- `agent_inbox`/`agent_outbox` already logged; the Telegram admin
  commands (`/show`, `/inbox`) extend to accept a conversation id.
- A new CLI: `maquinista conversation show <id>` renders the thread.
- Metrics: counter per `from_kind`, per `status` transition; histogram
  of conversation depths.

## Files

New:

- `internal/db/migrations/017_a2a_conversations.sql`
- `internal/routing/fanout.go` — outbox → inbox fan-out for
  agent mentions.
- `internal/a2a/a2a.go` — `SendToAgent`, `AskAgent` (Phase 3), helpers
  for conversation lookup/creation.
- `cmd/maquinista/cmd_conversation.go` — `show`, `list`, `close`.

Modified:

- `internal/mailbox/mailbox.go` — accept `from_kind='agent'` with
  source agent id; existing schema already allows it.
- `internal/bot/handlers_inbox.go` — render incoming `from_kind='agent'`
  messages with a prefix so the tmux pane's human operator can tell
  who's talking.
- `internal/config/config.go` — new `A2A` section (rate limits,
  depth cap, broadcast toggle).

## Verification per phase

- **Phase 1** — from Telegram, tell `@maquinista`: *"Say hi to @alice."*
  Alice's tmux window receives `[from @maquinista] hi alice`. Alice
  replies; her outbox goes to Telegram (same origin), not back into
  an infinite loop.
- **Phase 2** — `SELECT id, kind, participants, closed_at FROM
  conversations WHERE kind='a2a';` shows one row per distinct pair.
  `conversation show <id>` renders the thread interleaved.
- **Phase 3 (Option A)** — have `@maquinista` run
  `ask_agent('alice', 'what is 2+2?', 30)`; tool returns alice's reply
  body within timeout. Rerun with a prompt that makes alice stall
  `> 30 s`; tool returns `status='timed_out'`, and alice's reply
  eventually threads back into the conversation async.
- **Loop prevention** — seed a conversation at depth 11, have each
  agent mention the other; conversation closes at depth 12 and a
  system-note outbox row lands with `content->>'reason'='depth_cap'`.

## Open questions

1. **Broadcasts (`@everyone`).** Off-by-default? Operator opt-in via
   `agent_settings.roster->>'canBroadcast'`? Else a helpful agent
   tries to rally "all hands" and floods the fleet.
2. **Cross-agent memory citation.** If `@alice` sends a memory id to
   `@maquinista`, should `@maquinista` be able to read it? Default no
   (see `agent-memory-db.md` open question 3); opt-in via explicit
   `memory_share(target, memory_id)` call.
3. **Sync tool inside claude's turn.** `ask_agent` with a 30 s timeout
   blocks the outer agent's turn — will users tolerate that? Or should
   sync only exist in the sidecar world (Phase 3 Option B)?
4. **`@human` mention semantics.** Does it override the normal
   conversation-owner routing, or just add the origin channel to a
   potentially agent-only exchange? Leaning additive.
5. **Sub-agent model selection (Phase 4).** openclaw lets the spawner
   pick the child's model. Do we expose that, or always use the child
   agent's own configured runner? Prefer the latter — respects the
   child agent's soul/runner choice.
