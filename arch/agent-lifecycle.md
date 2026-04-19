# Agent Lifecycle

## Status model

An agent row (`agents.status`) moves through a defined set of states.
Not all transitions are valid; the diagram below shows allowed paths.

```
             ┌─────────┐
             │ spawning │  ← pre-registered before tmux window exists
             └────┬─────┘
                  │ tmux window up + runner ready
                  ▼
    ┌─────────────────────────┐
    │  running / idle / working│  ← live states; mailbox consumer delivers here
    └───────────┬─────────────┘
                │
        ┌───────┴────────┐
        │                │
        ▼                ▼
    ┌────────┐       ┌──────────┐
    │ stopped│       │ archived │  ← soft delete; row kept for history
    └────┬───┘       └──────────┘
         │
         │ reconcile / respawn
         ▼
    (back to spawning)
```

`dead` is a terminal state for agents that exhausted retries or were
hard-killed by the operator. Distinct from `archived` (operator intent)
vs `dead` (system gave up).

`stop_requested = TRUE` is a soft flag. The reconcile loop skips agents
with this flag set, keeping the tmux pane parked rather than respawning.

## Spawn paths

There are three ways an agent comes into existence:

**Tier-3 implicit spawn** (`cmd/maquinista/spawn_topic_agent.go`)
- Triggered by the first Telegram message to an unbound topic.
- Agent id is deterministic: `t-<chatID>-<threadID>`.
- Pre-registers a row with `status='spawning'` before creating the tmux
  window — this is the duplicate-spawn guard (rapid second message sees
  the marker and reuses the id instead of racing).
- Creates soul from default template, seeds memory blocks.
- Writes `topic_agent_bindings` owner row via routing ladder.

**Dashboard spawn** (`internal/dashboard/web/src/lib/actions.ts:spawnAgentFromDashboard`)
- Operator fills a form in the UI.
- DB-only: inserts `agents` row with `status='stopped'` and
  `tmux_window=''`, plus an `agent_souls` row cloned from the chosen
  template.
- No tmux window, no runner, no Telegram topic at creation time.
- Reconcile loop (`runDashboardAgentReconcile`) provisions the tmux pane
  within ~5 s.
- `RunTopicProvisioner` creates the Telegram forum topic within ~15 s.

**Operator spawn** (`maquinista agent spawn` / orchestrator)
- Direct CLI or orchestrator-driven. Inserts row and launches tmux window
  in one step via `agent.SpawnWithLayout`.

## Reconcile loop

`reconcileAgentPanes` runs at startup and every 5 s via
`runDashboardAgentReconcile`. It finds agents in live/stopped states
whose `tmux_window` is absent or stale and calls `respawnAgent` for each.

`respawnAgent` resolves workspace layout → ensures tmux session →
resolves runner command → `tmux.NewWindow` → waits for runner ready →
updates `agents.tmux_window`.

Resume semantics: if `agents.session_id` is set (written by the
SessionStart hook), the runner is launched with `--resume <session_id>`
so Claude's conversation history survives restarts.

## Workspace scopes

See [workspaces.md](workspaces.md).

## TODO

- [ ] Document `task_id IS NOT NULL` agents (orchestrator-owned subtask agents)
- [ ] Document `stop_requested` → graceful shutdown sequence
- [ ] Document `dead` transition conditions
- [ ] Session resume flow end-to-end (hook → session_id → --resume)
