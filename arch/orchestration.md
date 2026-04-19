# Orchestration

## The three-agent trio

The default fleet seeded at startup (`seedDefaultAgents`) consists of:

| Agent | Role | Responsibility |
|-------|------|----------------|
| `coordinator` | coordinator | Routes incoming requests, decides which agent to delegate to |
| `planner` | planner | Decomposes goals into tasks, manages task graph |
| `coder` | coder | Executes individual coding tasks |

These are regular user agents — same spawn path, same inbox/outbox — just
with distinct souls that give them specialized behavior.

## Tasks

Tasks (`tasks` table) are the unit of work. An orchestrator creates tasks,
assigns them to agents, and tracks their completion.

```
tasks (
    id          UUID
    project_id  TEXT
    agent_id    TEXT    -- assigned agent (nullable = unassigned)
    task_id     TEXT    -- parent task for subtask trees (nullable)
    title       TEXT
    body        TEXT    -- full spec / instructions
    status      TEXT    -- pending | claimed | running | done | failed
)
```

Subtask agents (`agents.task_id IS NOT NULL`) are spawned by the
orchestrator for a specific task and are torn down when the task
completes. They are excluded from the regular reconcile loop.

## Orchestrator engine

`orchestrator.Run` (`internal/orchestrator/`) is an optional daemon
started with `maquinista start --orchestrate`. It:

1. Polls `tasks` for `status='pending'` rows belonging to its project.
2. Claims a task (`status='claimed'`).
3. Spawns a subtask agent with `task_id` set.
4. Sends the task body as the first inbox message.
5. Monitors the agent; on completion marks task `done`.

The engine runs alongside the bot in the orchestrator process. Max
concurrent agents is configurable (`--orchestrate-max-agents`).

## Planner

`/plan [project]` in Telegram creates a dedicated Telegram forum topic
and a planner agent window. The planner receives free-form goals and
produces structured task drafts. `/plan release` hands off to the
orchestrator for execution.

The planner is an older, higher-level UX layer on top of the same
inbox/outbox + task infrastructure.

## Job registry

`jobreg` (`internal/jobreg/`) is the declarative scheduled-job system.
Operators write YAML files under `config/schedules/` and
`config/hooks/`. On startup (and periodically), `jobreg.Reconcile`
upserts these into the `job_registry` table. The scheduler daemon
(`maquinista scheduler`) fires them on their cron expressions by
injecting messages into agent inboxes.

## TODO

- [ ] Document task status machine in full
- [ ] Document coordinator delegation protocol (soul-level, not code-level)
- [ ] Document planner → task graph → orchestrator handoff
- [ ] Subtask agent teardown / cleanup
- [ ] Multi-project orchestrator support
- [ ] plans/active/multi-agent-registry.md implementation status
