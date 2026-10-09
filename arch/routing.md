# Message Routing

## The routing ladder

When a Telegram message arrives, `routing.Resolve` walks four tiers in
order and returns on the first match. If all tiers miss, the caller
receives `ErrRequirePicker` and surfaces a UI picker.

```
Tier 1 — Mention
  Message contains @handle or @agent-id
  → resolve handle to canonical id, deliver to that agent
  → short-circuits all other tiers

Tier 2 — Owner binding
  topic_agent_bindings WHERE user_id=$user AND thread_id=$thread AND binding_type='owner'
  → deliver to bound agent
  → steady state after any prior routing established the topic

Tier 3 — Spawn
  No binding found
  → call SpawnFunc (newTopicAgentSpawner)
  → per-user live-session cap check (MAQ-46; see below)
  → creates agent id = t-<chatID>-<threadID>
  → writes owner binding
  → deliver to new agent

Tier 4 — Picker
  SpawnFunc unavailable (nil) or returns ErrRequirePicker
  → bot shows an inline keyboard of live agents
  → operator selects one
  → writes owner binding for future messages
```

The tier-3 spawn id (`t-<chatID>-<threadID>`) is deterministic so
re-spawning the same topic always reuses the same agent row and its
conversation history.

## Per-user concurrent session cap (tier-3 guard)

Before a tier-3 spawn brings a new pane up, `routing.CheckSessionCap`
counts the user's live agent rows — agents joined through owner bindings
(`topic_agent_bindings.binding_type='owner'`) whose status is live
(`running`/`working`/`idle`/`spawning`). At or above the cap, the spawn is
rejected with a `*routing.SessionCapError` whose message is surfaced to
the user verbatim (count, max, `/agent_kill` escape hatch).

- Cap comes from `MAQUINISTA_MAX_AGENTS_PER_USER` (default 2); `<= 0`
  disables the guard.
- The incoming agent id is excluded from the count, so reusing an
  already-live topic agent or respawning a stopped one never blocks —
  only *new* panes are gated. The reuse fast paths in the spawner run
  before the cap check anyway.
- Best-effort by design (cheap runaway brake, not an accounting system):
  two simultaneous spawns in different topics can race past it by one.
- `agents` has no `user_id` column; owner bindings are the user→agent
  link until a real multi-user ownership model exists.

## Routing state

All routing state lives in Postgres:

| Concept | Table | Key |
|---------|-------|-----|
| Handle → agent id | `agents.handle` | lower(handle) UNIQUE |
| Topic owner | `topic_agent_bindings` | (user_id, thread_id) WHERE binding_type='owner' UNIQUE |
| Topic observers | `topic_agent_bindings` | binding_type='observer' |

The in-memory `state.State.ThreadBindings` map was a legacy read-through
cache. Post-migration-009 it is backed by the DB; the JSON file is
retired. See `plans/archive/json-state-migration.md`.

## Setting a default

`/agent_default @handle` calls `routing.SetUserDefault`, which:
1. Resolves `@handle` to a canonical agent id.
2. Deletes any existing owner binding for `(user_id, thread_id)`.
3. Inserts a new owner binding.

This is how an operator wires a dashboard-spawned agent to a Telegram
topic after the fact.

## A2A routing

Agent-to-agent messages bypass the routing ladder. The relay parses
`[@handle: text]` mentions in outbox rows and directly inserts into
`agent_inbox` with `origin_channel='a2a'`. The target agent is resolved
by `agents.id OR LOWER(agents.handle)`.

See [messaging.md](messaging.md) for the full relay flow.

## TODO

- [ ] Document tier-4 picker state machine (agentPickerStates)
- [ ] Document `/observe` observer binding path
- [ ] Document conversation threading for A2A (ensureA2AConversation)
- [ ] Multi-user scenarios — two users in the same group, same thread
