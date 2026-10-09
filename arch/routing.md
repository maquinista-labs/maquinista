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

## Per-user repo binding (MAQ-45)

`USER_REPOS` maps each allowed user to the repo(s) they may drive, as
comma-separated `<userID>:<repoRoot>` pairs (repeat a user to grant
several repos; `~` is expanded, paths are normalized):

```
USER_REPOS=111:/srv/repo-a,222:/srv/repo-b,222:/srv/repo-c
```

Users with no entry are unrestricted — the pre-MAQ-45 single-operator
behavior. A bound user may only reach agents rooted in one of their
repos; the check **fails closed** on agents with an unresolvable repo
(no `workspace_repo_root`, no `cwd`). Enforcement points:

| Ladder step | Where | Check |
|-------------|-------|-------|
| Tier 1 mention | `routing.Resolve` | resolved agent's repo ∈ user's repos, else `ErrRepoForbidden` |
| Tier 2 owner binding | `routing.Resolve` | bound agent's repo, same |
| Tier 3 spawn (fresh) | `newTopicAgentSpawner` | agent cwd = user's first listed repo |
| Tier 3 spawn (reuse/respawn) | `newTopicAgentSpawner` | existing agent's repo ∈ user's repos, else `ErrRepoForbidden` |
| Tier 4 picker list | `bot.showAgentPicker` | foreign agents filtered out of the keyboard |
| Tier 4 confirm | `routing.ConfirmPickerChoice` | chosen agent's repo, `ErrRepoForbidden` writes nothing |
| `/agent_default` | `routing.SetUserDefault` | target agent's repo, same |
| `/agent_spawn` | `bot.handleAgentSpawnCommand` | restricted users spawn into their own repo, never the orchestrator cwd |

The repo an agent "belongs to" is `agents.workspace_repo_root`, falling
back to `agents.cwd` (`routing.AgentRepoRoot`). The policy itself is a
`routing.RepoPolicy` func built from config in `bot.repoPolicyFromConfig`;
rejections wrap `routing.ErrRepoForbidden` and render a user-facing
"outside your allowed repos" message — a routing rejection, not a trust
assumption. Tier 3's repo only exists once the SpawnFunc has chosen it,
so the spawner enforces the binding there itself. Consequence: whoever
spawned a topic's agent first fixes the topic's repo; a user bound to a
different repo must use a different topic. Non-goals (ADR-0009 F1+): a
tenant column/seam, sandboxing, BYOK.

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
- [x] Multi-user repo isolation (MAQ-45, `USER_REPOS`) — same-group
  shared-topic semantics: topic repo fixed by first spawner
