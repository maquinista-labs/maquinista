# Planning ↔ Scheduling Bridge

Introduce **planning sessions** as a first-class session kind — fresh per
invocation, retained after completion — and wire them to a proper scheduled-job
execution engine. Separate the concerns cleanly: planning sessions produce task
graphs, scheduled jobs produce planning sessions (or direct task runs) on a
cron, and the dashboard surfaces each kind in its own UI panel.

Reference implementations studied:
- **Hermes** (`../hermes-agent`) — fresh agent per cron tick, silent-marker
  suppression, pre-run scripts, per-job skill injection
- **OpenClaw** (`../openclaw`) — isolated session per job, named-agent routing,
  task-flow state machine, per-job `toolsAllow` whitelist

---

## Concept map (revised)

Three kinds of agents/sessions exist in the `agents` table. None are
pre-created at startup — all are spawned on demand from soul templates.

| Kind | ID pattern | Role | Lifecycle | Telegram topic |
|------|-----------|------|-----------|----------------|
| **Planning session** | `plan-<uuid>` | `planning` (new) | Spawned fresh per invocation; `completed` when done | Yes — stays open after completion |
| **Task execution agent** | `impl-<task_id>` | `implementor` | Spawned per task; `completed` when done | No — output forwarded via observer binding |
| **Scheduled job execution** | produced by job runner | depends on soul template | Planning session spawned per run | Inherits from above |

Soul templates (`coordinator`, `planner`, `coder`) live in `soul_templates`
and act as catalogs. A template is instantiated into an agent row only when a
session is actually needed — via `/t_plan`, a scheduled job firing, or an
explicit dashboard spawn. No persistent agents sit idle between uses.

The key principle: **nothing is deleted when it finishes**. Sessions move to
`completed` (a new terminal status), stay visible in the dashboard, and their
Telegram topics (where applicable) remain open so the operator can review the
output.

---

## What changes in the planning flow

### Before (current)

`/t_plan` sends a prompt into the tmux window **already bound to the current
topic** (the persistent coordinator or planner). The plan approval card appears
in the same thread. The persistent agent's conversation is contaminated by the
planning exchange.

### After (this plan)

`/t_plan` **spawns a fresh planning agent** with the `planner` soul template,
creates a dedicated Telegram topic for it immediately (not via the 15s
provisioner loop), and sends the planning prompt there. The approval card
appears in the new topic. When the planning session ends (approved, rejected,
or timed out) the agent status becomes `completed`. The Telegram topic stays
open.

```
operator: /t_plan "implement weekly sync feature"
       │
       ▼
spawn new agent
  id:   plan-<uuid>
  soul: planner template
  role: planning
       │
       ├─ create Telegram topic synchronously → "Planning: weekly sync feature"
       │     topic_agent_bindings owner row
       │
       ├─ send buildPlanningPrompt() to new tmux window
       │
       ▼
planner outputs PLAN_JSON: in new topic
       │
       ▼
approval card sent to new topic thread
       │
operator taps Approve
       │
       ├─ tasks created as status="draft"
       ├─ draft-release → status="ready"
       ├─ task scheduler → EnsureAgent → impl-<task_id> agents
       │    each impl agent gets observer binding → new topic thread
       │
       └─ planning agent: status="completed"
          Telegram topic stays open (read-only history)
```

---

## Architecture

### Session kinds in agents table

Add a `session_kind TEXT` column (migration):

```sql
ALTER TABLE agents
  ADD COLUMN session_kind TEXT NOT NULL DEFAULT 'persistent'
  CHECK (session_kind IN ('persistent','planning','implementor','scheduled'));
```

Add `completed` to the valid status set (update the check constraint or
comment — `arch/agent-lifecycle.md` already has `archived` as a soft-delete;
`completed` is the task-done equivalent):

```sql
-- agents.status: idle|working|dead|stopped|archived|completed
-- completed = terminal but preserved for audit/review
```

Update `listAgents` query in the dashboard to group/filter by `session_kind`.

### Telegram topic lifecycle change

`RunTopicProvisioner → closeOrphanedTopics` currently closes topics for agents
with `status IN ('archived','dead')`. **Do not close topics for `completed`
agents.** The operator needs to be able to scroll back through the planning
conversation and see the full output.

Update the orphan query:
```sql
WHERE b.binding_type = 'owner'
  AND (a.id IS NULL OR a.status IN ('archived','dead'))
  -- removed: 'completed' is kept open
```

### Scheduled jobs: spawn fresh, not inject

`scheduled_jobs.agent_id` currently points at a persistent agent.
Rename/repurpose: replace `agent_id` with `soul_template_id` (migration). When
the job runner fires, it **spawns a new planning agent** from that soul
template — the same path as `/t_plan`. The spawned agent's ID is recorded in
`job_executions` (new table below).

```sql
-- migration: replace agent_id FK with soul_template_id; add auto_approve
ALTER TABLE scheduled_jobs
  DROP COLUMN agent_id,
  ADD COLUMN soul_template_id TEXT REFERENCES soul_templates(id),
  ADD COLUMN auto_approve     BOOLEAN NOT NULL DEFAULT FALSE;
-- soul_template_id NULL → use default planner template
-- auto_approve FALSE → operator must approve the plan before tasks run (safe default)
-- auto_approve TRUE  → plan released automatically; no Telegram card sent
```

### job_executions table (new)

The existing `job_runs` is a VIEW over `agent_inbox/outbox` — not enough for
linking executions to spawned planning sessions. Add a real table:

```sql
CREATE TABLE job_executions (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  job_id          UUID        NOT NULL REFERENCES scheduled_jobs(id) ON DELETE CASCADE,
  agent_id        TEXT        REFERENCES agents(id) ON DELETE SET NULL,
  status          TEXT        NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running','completed','failed','suppressed')),
  plan_approved   BOOLEAN,           -- NULL until operator acts
  task_ids        TEXT[]      DEFAULT '{}',
  error           TEXT,
  started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  ended_at        TIMESTAMPTZ,
  reply_channel   JSONB              -- where approval card was sent
);
CREATE INDEX idx_job_executions_job ON job_executions(job_id);
CREATE INDEX idx_job_executions_agent ON job_executions(agent_id);
```

---

## Implementation plan

### P.1 — Add `completed` status and `session_kind` column

**New migration:** `internal/db/migrations/030_session_kinds.sql`

```sql
ALTER TABLE agents
  ADD COLUMN IF NOT EXISTS session_kind TEXT NOT NULL DEFAULT 'planning'
  CHECK (session_kind IN ('planning','implementor','scheduled'));

-- Backfill existing impl agents
UPDATE agents SET session_kind = 'implementor'
  WHERE role = 'implementor' OR task_id IS NOT NULL;
```

No persistent archetype rows exist. Soul templates (`soul_templates`) are the
catalog; agent rows are created only when a session is spawned.

Update `arch/agent-lifecycle.md` to document `completed`.

Update `topic_provisioner.go → closeOrphanedTopics` to exclude `completed`.

**Acceptance:** orphan-close no longer fires for completed agents; no seed
agents appear in the dashboard on fresh install.

---

### P.2 — Planning session spawn

**New file:** `internal/agent/planning.go`

```go
// SpawnPlanningSession creates a fresh planning agent and its Telegram topic
// in one atomic sequence. Returns the new agent ID and Telegram thread ID.
func SpawnPlanningSession(ctx context.Context, cfg PlanningSpawnConfig) (agentID string, threadID int, err error) {
    agentID = "plan-" + uuid.New().String()[:8]

    // 1. Create agents row with session_kind='planning', status='spawning'
    // 2. Clone planner soul template into agent_souls
    // 3. Create Telegram forum topic synchronously via bot API
    // 4. Write topic_agent_bindings owner row
    // 5. Launch tmux window via SpawnWithLayout (shared scope, same repo root)
    // 6. Return agentID + threadID
}

type PlanningSpawnConfig struct {
    Pool         *pgxpool.Pool
    TmuxSession  string
    Runner       runner.AgentRunner
    ClaudeMDPath string
    BotAPI       BotAPIClient   // for synchronous topic creation
    ChatID       int64
    UserID       int64
    Description  string         // used as Telegram topic name
    ProjectID    string
    JobID        *string        // set when triggered by a scheduled job
    AutoApprove  bool           // skip approval card; release tasks immediately
}
```

**Modify `handlePlanCommand`** (`internal/bot/plan_commands.go`):

```go
// Before: resolve existing window, send prompt there
// After:
func (b *Bot) executePlan(msg *tgbotapi.Message, description string) {
    agentID, threadID, err := agent.SpawnPlanningSession(ctx, agent.PlanningSpawnConfig{
        Description: description,
        ChatID:      msg.Chat.ID,
        UserID:      msg.From.ID,
        ProjectID:   project,
        ...
    })
    // send planning prompt to new agent's window
    prompt := buildPlanningPrompt(description, project)
    b.sendPromptToTmux(agentID, prompt)
    // confirm in original topic
    b.reply(msg.Chat.ID, getThreadID(msg),
        fmt.Sprintf("Planning session started → see topic "%s"", description))
}
```

The monitor now needs to watch ALL planning agent windows (not just a fixed
window). It already discovers windows from the `agents` table by polling; the
`session_kind='planning'` filter makes the right subset obvious.

**Acceptance:** `/t_plan "foo"` creates a new Telegram topic "Planning: foo",
the plan exchange happens there, the original topic gets a short "started"
acknowledgement. Existing sessions remain in the Agents tab.

---

### P.3 — Planning session completion

**File:** `internal/bot/plan_commands.go` → `handlePlanApprove/Cancel`

When the plan is approved or cancelled, transition the planning agent to
`completed` (not `dead`/deleted):

```go
// after tasks are created and released:
pool.Exec(ctx, `
    UPDATE agents SET status='completed', stop_requested=TRUE
    WHERE id = $1
`, planAgentID)
// stop_requested=TRUE tells reconcileAgentPanes to NOT respawn this window
```

The tmux window will eventually die (Claude finishes the conversation). The DB
row stays permanently. The Telegram topic stays open.

Draft gate: planning sessions create tasks as `status="draft"`. Release path
depends on source:

- **Manual `/t_plan`** — always requires operator Approve tap (approval card
  sent to planning topic).
- **Scheduled job, `auto_approve=false`** — same as manual; card sent to
  `reply_channel` topic.
- **Scheduled job, `auto_approve=true`** — tasks released immediately after
  `PLAN_JSON:` is parsed; no card sent. The planning agent transitions directly
  to `completed`. This is the path for fully automated jobs (e.g. daily email
  digests) where human review adds no value.

---

### P.4 — Observer bindings for task agents

**File:** `cmd/maquinista/cmd_task_scheduler.go`

Currently `EnsureAgent` is called with no observer params, so task agents are
silent in Telegram. Thread the planning session's topic through:

```go
// Store on the planState: the planning session's own threadID + chatID
// Pass to EnsureAgent for each task spawned from this plan:
orchestrator.EnsureAgentParams{
    Pool:             pool,
    Spawner:          spawner,
    Role:             role,
    TaskID:           taskID,
    ObserverUserID:   planState.UserID,
    ObserverThreadID: planState.ThreadID,   // the planning session's topic
    ObserverChatID:   &planState.ChatID,
}
```

Each `impl-*` agent posts its output back to the planning session's Telegram
topic. The operator checks one topic and sees: the original plan, the approval
action, and then each task agent's progress — all in one thread.

---

### P.5 — Spawn paths beyond `/t_plan`

#### A. Dashboard "New Plan" button

**Files:** `internal/dashboard/web/src/components/dash/spawn-agent.tsx` (extend
or add `spawn-plan.tsx`), dashboard API route

Add a "New Plan" button in the dashboard header or Agents tab toolbar. On
click: modal asking for description + project. POST to `/api/plans` → calls
`SpawnPlanningSession` → returns new agent ID + Telegram thread URL.

The new planning session appears in the Agents tab immediately (SSE push).
The Telegram topic link is shown in the success toast so the operator can jump
to it.

For operators who don't use Telegram, the dashboard "Plan" sessions tab (P.7)
shows the planning agent's outbox inline — no Telegram required.

#### B. From a conversation with another agent

An operator can tell the coordinator: "@coordinator plan the auth feature". The
coordinator (via A2A or tool use) calls the planning session spawn API and
reports back the new topic link. This reuses the same `SpawnPlanningSession`
path — no special case in the bot.

This is enabled by the existing A2A conversation infrastructure
(`internal/routing`) but requires the coordinator's soul to know it can spawn
planning sessions. Not in scope for v1; noted here for sequencing.

#### C. Jobs tab "Run now"

The Jobs tab shows each scheduled job with a "Run now" button. POST to
`/api/jobs/:id/run` → calls the same dispatch path as the cron daemon. A new
planning session is spawned; its ID appears in `job_executions` and links back
to the Jobs tab row.

---

### P.6 — Job runner daemon (revised: spawn, not inject)

**New files:** `internal/jobruns/runner.go`, `cmd/maquinista/cmd_jobruns.go`

```go
func dispatchPlanJob(ctx context.Context, cfg Config, job ScheduledJobRow) {
    // 1. Spawn fresh planning agent (same as P.2 SpawnPlanningSession)
    //    soul_template = job.SoulTemplateID ?? "planner"
    agentID, threadID, err := agent.SpawnPlanningSession(ctx, agent.PlanningSpawnConfig{
        Description: job.Prompt["text"].(string),
        ChatID:      job.ReplyChannel["chat_id"].(int64),
        JobID:       &job.ID,
        AutoApprove: job.AutoApprove,   // skip approval card if true
        ...
    })

    // 2. Insert job_executions row
    execID := insertJobExecution(ctx, pool, job.ID, agentID, threadID)

    // 3. Send planning prompt into new window
    sendPromptToTmux(agentID, buildPlanningPrompt(...))

    // 4. Advance next_run_at
    advanceNextRun(ctx, pool, job)
}
```

`FOR UPDATE SKIP LOCKED` on `scheduled_jobs` fetch prevents double-fire.
Wire into `maquinista start` as a goroutine; also expose as
`maquinista jobruns` subcommand.

**Migration:** replace `scheduled_jobs.agent_id` with `soul_template_id`:

```sql
-- 030b_scheduled_jobs_soul_template.sql
ALTER TABLE scheduled_jobs
  ADD COLUMN soul_template_id TEXT REFERENCES soul_templates(id);
-- backfill: map existing agent_id → that agent's soul template
UPDATE scheduled_jobs sj
  SET soul_template_id = (
    SELECT template_id FROM agent_souls WHERE agent_id = sj.agent_id
  );
ALTER TABLE scheduled_jobs DROP COLUMN agent_id;
```

---

### P.7 — Dashboard: Agents tab — session kind differentiation

**File:** `internal/dashboard/web/src/components/dash/agents-list-client.tsx`
and `agent-card.tsx`

The `listAgents` query already returns `role`. Add `session_kind` to the
projection:

```sql
SELECT a.id, a.session_kind, a.role, a.status, ...
```

In `AgentCard`, render a secondary badge based on `session_kind`:

| `session_kind` | badge | color |
|---------------|-------|-------|
| `planning` | `#planning` | purple |
| `implementor` | `#task` | blue |
| `scheduled` | `#job` | amber |

Group agents in the list into collapsible sections:

```
▼ Planning sessions (3)
  [plan-a1b2 ✓ approved · "weekly sync"]
  [plan-c3d4 ⏳ pending approval · "auth rework"]
  [plan-e5f6 ✓ approved · "CI pipeline"]

▼ Completed tasks (12)
  [impl-task-01 ✓]  [impl-task-02 ✓]  …  [show more]
```

Planning session cards show extra fields: description (from `agents.handle`
or metadata), approval status (`plan_approved` from `job_executions`), task
count.

**The `listAgents` SQL needs a join** to pull plan description and task count:

```sql
LEFT JOIN job_executions je ON je.agent_id = a.id
LEFT JOIN (
  SELECT claimed_by, COUNT(*) AS task_count
  FROM tasks WHERE status = 'done'
  GROUP BY claimed_by
) done_tasks ON done_tasks.claimed_by = '@' || a.id
```

---

### P.8 — Dashboard: Jobs tab

**New page:** `internal/dashboard/web/src/app/(dash)/jobs/page.tsx` (already
exists as a stub — populate it)

**Two sections:**

#### Section A — Scheduled job definitions

```sql
SELECT id, name, cron_expr, soul_template_id, enabled,
       next_run_at, last_run_at,
       (SELECT COUNT(*) FROM job_executions WHERE job_id = sj.id) AS total_runs,
       (SELECT COUNT(*) FROM job_executions
        WHERE job_id = sj.id AND status = 'failed') AS failed_runs
FROM scheduled_jobs sj
ORDER BY next_run_at
```

Columns: Name · Schedule · Next run · Last run · Total / Failed · Actions

Actions: Enable/disable toggle · Edit cron · Run now · Delete

Job definition form includes an **Auto-approve** toggle (off by default). When
on, the job row shows an "AUTO" badge in the definitions table so operators can
see at a glance which jobs run without approval.

#### Section B — Execution history

```sql
SELECT je.id, je.job_id, sj.name AS job_name,
       je.agent_id, je.status, je.plan_approved,
       je.started_at, je.ended_at,
       array_length(je.task_ids, 1) AS task_count
FROM job_executions je
JOIN scheduled_jobs sj ON sj.id = je.job_id
ORDER BY je.started_at DESC
LIMIT 100
```

Columns: Job name · Started · Duration · Status · Tasks · Approved?

Each row links to the planning session agent (Agents tab detail view) and to
the task list filtered by `metadata->>'job_id'`.

**SSE:** push `job_executions` insert events to keep the Jobs tab live without
polling. Wire into the existing `agent_outbox_new` NOTIFY or add a dedicated
`NOTIFY job_execution_new` trigger.

---

### P.9 — `/schedule` command and `scheduled_jobs` schema cleanup

**File:** `internal/bot/schedule_commands.go`

Update parser to use `soul_template_id` instead of `agent_id`:

```
/schedule <name> "<cron>" --soul planner [--auto] plan "<description>"
/schedule <name> "<cron>" --soul planner [--auto] "<raw prompt>"
```

`--soul` defaults to `planner` if omitted. Resolves against `soul_templates.id`
or `soul_templates.name`.

`--auto` sets `auto_approve=true` — tasks run without operator approval. Omit
for jobs where human review matters.

`reply_channel` defaults to current chat + thread.

---

## Sequencing

```
P.1  completed status + session_kind column     ← DB foundation, no behavior change
P.2  SpawnPlanningSession + /t_plan rewrite     ← core behavior change; depends P.1
P.3  planning session completion lifecycle      ← depends P.2
P.4  observer bindings for task agents          ← depends P.2 (needs planning threadID)
P.5a dashboard "New Plan" button               ← depends P.2
P.6  job runner daemon (spawn model)            ← depends P.2
P.7  agents tab session kind UI                 ← depends P.1; can stub P.6 data
P.8  jobs tab                                   ← depends P.6 + job_executions table
P.5b jobs tab "Run now"                         ← depends P.6 + P.8
P.9  /schedule command cleanup                  ← depends P.6
P.10 skills + tool allowlists                  ← depends P.2 (SpawnPlanningSession); integrates agentskills Phase 1
P.11 task DAG graph + mid-graph approval gates ← depends P.6 (job_executions) + P.8 (jobs tab)
```

**v0 (P.1 + P.2 + P.3):** `/t_plan` spawns a fresh session, gets its own
topic, stays `completed` after. Persistent agents are no longer touched by
planning. Visible immediately in Agents tab (no special grouping yet).

**v1 (+ P.4 + P.7):** Task agents post back to the planning topic. Dashboard
groups agents by session kind. The operator sees the full picture in one place.

**v2 (+ P.5a + P.6 + P.8):** Scheduled jobs spawn fresh planning sessions per
run. Jobs tab shows definitions + execution history. Dashboard "New Plan" button
works.

**v3 (+ P.9 + P.10):** Skills injected into planning/job agents at spawn time.
Tool allowlists stored and surfaced via soul constraints. `/schedule` accepts
`--skill` and `--allow-tool` flags. Requires agentskills Phase 1 to be landed.

**v4 (+ P.11):** Job execution detail shows a live DAG graph. Mid-graph approval
gates pause at any node; operator approves from dashboard inline panel or
Telegram card. Planner soul emits `requires_approval` automatically for
irreversible side effects (email send, social post, deploy, financial action).

---

## What this plan adopts from Hermes / OpenClaw

| Pattern | Source | How adopted |
|---------|--------|-------------|
| Fresh session per job run | Hermes | Planning sessions are always fresh (P.2/P.6) |
| Session stored after completion | Neither — both discard | **New**: `completed` status, retained indefinitely |
| Named agent routing | OpenClaw | `soul_template_id` selects which soul the spawned agent gets |
| Task-flow state (waiting/blocked) | OpenClaw | Draft gate + `job_executions.plan_approved` + per-node `awaiting_approval` (P.11) |
| DAG graph visualization | OpenClaw (TaskFlowRecord) | Live React Flow canvas on execution detail page (P.11) |
| Auto-approve (no human gate) | Hermes + OpenClaw | `scheduled_jobs.auto_approve=true` releases tasks immediately — opt-in, default safe |
| [SILENT] suppression | Hermes | Future: planning agent can output `[NO_PLAN]` to skip approval card |
| Failure alert routing | OpenClaw | `job_executions.status='failed'` + reply_channel warning |
| Per-job skill injection | Hermes | `scheduled_jobs.skills[]` → `SpawnPlanningSession` loads SKILL.md content into soul (P.10) |
| Per-job tool allowlist | OpenClaw | `scheduled_jobs.tools_allow[]` → `agents.tools_allow` → soul Boundaries constraint (P.10) |

## What remains unique to Maquinista

- **DAG task graphs with git worktree isolation** — competitors produce text output; this plan produces actual code on branches.
- **Operator approval gate** — mandatory human review before any task agents start work.
- **All outputs retained** — planning session transcripts, task agent outputs, and the Telegram topics stay open. Competitors discard session state after delivery.
- **Unified Postgres backing** — `scheduled_jobs`, `job_executions`, `agents`, `tasks`, `task_deps` all queryable together. No split between JSON files (Hermes) and SQLite (OpenClaw).

---

### P.11 — Job execution graph: task DAG visualization + mid-graph approval gates

Two concerns bundled because they share the same data model and UI surface:

1. **Graph view** — operator opens a job execution and sees its task DAG with
   live status for each node.
2. **Approval gates** — certain tasks in the DAG can require operator sign-off
   before their downstream dependents run. Distinct from `auto_approve` (which
   gates the whole plan); this gates individual steps mid-execution.

#### Task model additions

```sql
-- 030d_task_approval_gates.sql
ALTER TABLE tasks
  ADD COLUMN requires_approval BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN approved_at       TIMESTAMPTZ,
  ADD COLUMN approved_by       TEXT;    -- user_id who tapped Approve

-- Widen status state machine to include awaiting_approval:
-- pending → ready → awaiting_approval → claimed → review → done | failed
-- (also add 'draft' for plan-gated tasks created by planning sessions)
COMMENT ON COLUMN tasks.status IS
    'State machine: draft → pending → ready → [awaiting_approval →] claimed '
    '→ review → done | failed. awaiting_approval: task deps done, '
    'requires_approval=true, waiting for operator. draft: created by planning '
    'session but not yet released (plan not approved).';

-- Audit trail for approval actions
CREATE TABLE task_approvals (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  task_id     TEXT        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  action      TEXT        NOT NULL CHECK (action IN ('approved','rejected')),
  user_id     TEXT        NOT NULL,
  message     TEXT,                    -- optional operator comment
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_task_approvals_task ON task_approvals(task_id);
```

The `requires_approval` flag is set by the **planner** in `PLAN_JSON`. Example
plan output:

```json
{
  "tasks": [
    { "id": "t1", "title": "Fetch open PRs and draft digest", "requires_approval": false },
    { "id": "t2", "title": "Format and render email HTML", "requires_approval": false,
      "deps": ["t1"] },
    { "id": "t3", "title": "Send email to team@company.com", "requires_approval": true,
      "deps": ["t2"] }
  ]
}
```

Task `t3` will not run until the operator taps Approve in the graph view or
Telegram. The planner soul template is instructed to emit `requires_approval`
on any task that has an irreversible external side effect (email send, social
post, deploy).

#### State machine change

`refresh_ready_tasks` trigger today cascades `done → ready`. With approval
gates, the cascade needs a detour:

```sql
CREATE OR REPLACE FUNCTION refresh_ready_tasks()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status = 'done' THEN
    -- Tasks whose all deps are now done:
    -- if requires_approval=true → awaiting_approval
    -- otherwise → ready (existing behaviour)
    UPDATE tasks t
    SET status = CASE
      WHEN t.requires_approval AND t.approved_at IS NULL
        THEN 'awaiting_approval'
      ELSE 'ready'
    END
    WHERE t.status = 'pending'
      AND t.id IN (
        SELECT task_id FROM task_deps WHERE depends_on = NEW.id
      )
      AND t.id NOT IN (
        SELECT td.task_id FROM task_deps td
        JOIN tasks dep ON dep.id = td.depends_on
        WHERE dep.status != 'done'
      );
  END IF;
  RETURN NEW;
END;
$$;
```

When a task enters `awaiting_approval`:
1. A `NOTIFY task_approval_needed` fires (new trigger).
2. The relay picks it up and sends an approval card to the planning topic
   (same `reply_channel` as the job execution).
3. On operator Approve: `approved_at = NOW()`, `approved_by = user_id`,
   status → `ready` → taskscheduler claims it normally.
4. On Reject: status → `failed`, downstream tasks stay `pending` (blocked).

`task_approvals` row written on both actions for audit.

#### API

```
GET  /api/jobs/executions/:exec_id/graph
```

Returns the full DAG for a job execution as a graph payload:

```json
{
  "nodes": [
    {
      "id": "t1",
      "title": "Fetch open PRs and draft digest",
      "status": "done",
      "requires_approval": false,
      "agent_id": "impl-t1",
      "started_at": "...",
      "done_at": "...",
      "pr_url": null
    },
    {
      "id": "t3",
      "title": "Send email to team@company.com",
      "status": "awaiting_approval",
      "requires_approval": true,
      "approved_at": null,
      "agent_id": null
    }
  ],
  "edges": [
    { "from": "t1", "to": "t2" },
    { "from": "t2", "to": "t3" }
  ]
}
```

Query:

```sql
SELECT
  t.id, t.title, t.status, t.requires_approval,
  t.approved_at, t.approved_by, t.claimed_by AS agent_id,
  t.claimed_at AS started_at, t.done_at, t.pr_url
FROM tasks t
WHERE t.metadata->>'job_execution_id' = $1
  -- tasks store execution context in metadata at creation time
ORDER BY t.created_at;
-- edges:
SELECT task_id AS "from", depends_on AS "to"
FROM task_deps
WHERE task_id IN (SELECT id FROM tasks WHERE metadata->>'job_execution_id' = $1);
```

`tasks.metadata` already exists as JSONB — store `job_execution_id` and
`plan_agent_id` there at task creation time in `handlePlanApprove`.

Approval actions:

```
POST /api/jobs/executions/:exec_id/tasks/:task_id/approve
POST /api/jobs/executions/:exec_id/tasks/:task_id/reject
```

Both require operator session. Write `task_approvals` row, flip status,
`NOTIFY task_graph_updated` so the dashboard refreshes via SSE.

#### Dashboard — execution detail page

**Route:** `/jobs/executions/:exec_id`

Reachable from: Jobs tab execution history row → click → opens detail page.

Layout:
```
┌─────────────────────────────────────────────────────────┐
│  Daily PR Digest  ·  run #4  ·  2026-04-19 06:00        │
│  Status: running  ·  3/5 tasks done  ·  ⏱ 4m 12s       │
├─────────────────────────────────────────────────────────┤
│                                                          │
│  [Fetch PRs ✓] ──► [Render HTML ✓] ──► [Send email ⏳] │
│                                              │           │
│                                         AWAITING         │
│                                         APPROVAL         │
│                                       [Approve] [Reject] │
│                                                          │
├─────────────────────────────────────────────────────────┤
│  Task detail (click any node)                           │
│  impl-t2 · done · 1m 32s · PR #142                     │
└─────────────────────────────────────────────────────────┘
```

**Graph rendering:** use `@xyflow/react` (React Flow) for the DAG canvas —
it's already the standard for node-edge UIs in Next.js apps and handles
layout via `elkjs` or `dagre`. Nodes are custom React components; edges show
dep direction.

**Node states → visual treatment:**

| Status | Color | Animation |
|--------|-------|-----------|
| `draft` | gray, dashed border | — |
| `pending` | gray | — |
| `ready` | blue | — |
| `claimed` | blue | pulse ring |
| `awaiting_approval` | amber | pulse ring + lock icon |
| `review` | purple | — |
| `done` | green | ✓ |
| `failed` | red | ✗ |

**Approval interaction:** clicking an `awaiting_approval` node expands an
inline panel (not a modal) with:
- Task title + body
- "What will this do?" — task context from `task_context` table
- **Approve** / **Reject** buttons → POST to approval API
- Optional comment field

After action: node color updates via SSE push (`NOTIFY task_graph_updated`).

**Live updates:** the graph subscribes to the existing agent SSE stream
filtered to the execution's task IDs. No polling.

#### Telegram approval card for mid-graph gates

Same card format as plan approval, different copy:

```
⏸ Task approval needed

Job: Daily PR Digest (run #4)
Step: Send email to team@company.com

Dependencies completed ✓
Ready to execute — tap to proceed.

[✓ Approve]  [✗ Reject]
```

Card sent to the planning topic for this execution (stored in
`job_executions.reply_channel`). Callback data includes `task_id` and
`exec_id` so the handler can route correctly without ambiguity.

#### Planner soul guidance

Add to the planner soul template `Extras` (seeded in `seed_agents.go`):

```
When emitting PLAN_JSON, mark requires_approval=true on any task that:
- sends a message, email, or notification to an external party
- publishes content to a social platform or public URL
- deploys to production
- deletes data that cannot be recovered
- charges money or initiates a financial transaction
All other tasks default to requires_approval=false.
```

This makes the approval gate emergent from task semantics rather than
requiring the operator to configure it per task.

---

### P.10 — Skills and tool allowlists for planning sessions and scheduled jobs

Both reference implementations restrict what a cron-spawned agent can do:

| Pattern | Source | Mechanism |
|---------|--------|-----------|
| Skill injection | Hermes (`_build_job_prompt`) | SKILL.md content prepended to system prompt at spawn time |
| Tool allowlist | OpenClaw (`toolsAllow` in cron-tool.ts) | Restricts which tool IDs the agent may call |

Maquinista adopts both, bridging into the `agentskills-integration` plan
(Phase 1 of that plan must land first).

#### Schema additions

```sql
-- 030c_job_skills_allowlists.sql
ALTER TABLE scheduled_jobs
  ADD COLUMN skills      TEXT[]  NOT NULL DEFAULT '{}',
  ADD COLUMN tools_allow TEXT[]  NOT NULL DEFAULT '{}';
-- Empty arrays = no restriction (current behaviour preserved).
-- skills: list of installed skill names to inject into the spawned agent's soul
-- tools_allow: whitelist of tool IDs the spawned agent may invoke;
--              empty = all tools permitted (mirrors OpenClaw default)
```

The `agents` table also gets a `tools_allow` column so the restriction travels
with the agent row and can be enforced by the runner without re-reading the job:

```sql
ALTER TABLE agents
  ADD COLUMN tools_allow TEXT[] NOT NULL DEFAULT '{}';
-- Empty = unrestricted. Checked by the agent runner before each tool call.
```

#### `SpawnPlanningSession` signature additions

```go
type PlanningSpawnConfig struct {
    // …existing fields…
    Skills     []string   // skill names to inject (from agent_skill_assignments or job override)
    ToolsAllow []string   // allowed tool IDs; nil/empty = unrestricted
}
```

Spawn sequence additions (after soul clone, before tmux launch):

```go
// 2b. If cfg.Skills is non-empty, load SKILL.md content for each name
//     via internal/skill.LoadForAgent() — returns []SkillEntry.
//     Append to soul.Extras or pass through PromptLayers.Skills.
// 2c. Write agents.tools_allow = cfg.ToolsAllow for enforcement at runtime.
pool.Exec(ctx, `UPDATE agents SET tools_allow = $1 WHERE id = $2`,
    pq.Array(cfg.ToolsAllow), agentID)
```

Skill loading calls into `internal/skill` (the agentskills plan's package):

```go
entries, err := skill.LoadForAgent(ctx, skill.LoadConfig{
    SkillNames:  cfg.Skills,         // explicit list from job/session config
    Pool:        cfg.Pool,
    AgentID:     agentID,            // also loads agent_skill_assignments rows
    MaxTotalChars: 18_000,           // default budget
})
// entries are passed into soul/compose.go PromptLayers.Skills
```

This means a planning session can receive:
- **Assigned skills** (from `agent_skill_assignments` for the spawned agent ID — normally empty for fresh agents)
- **Job-level skill override** (from `scheduled_jobs.skills`, passed via `cfg.Skills`)

Job-level wins: if the job specifies `skills=["code-review"]`, that skill
is always injected regardless of assignment table state.

#### `/schedule` command extension

```
/schedule "daily review" "0 9 * * *" --soul planner \
  --skill code-review --skill github-monitor \
  --allow-tool bash --allow-tool read_file \
  plan "review open PRs and flag anything stale"
```

Parser additions in `internal/bot/schedule_commands.go`:

```go
// New flags parsed from /schedule message:
type ScheduleCreateOpts struct {
    // …existing…
    Skills     []string   // --skill <name> (repeatable)
    ToolsAllow []string   // --allow-tool <id> (repeatable)
}
```

Stored directly into `scheduled_jobs.skills` and `scheduled_jobs.tools_allow`.

#### Dashboard — Jobs tab form (P.8 extension)

Add two optional fields to the "Create / Edit job" form:

**Skills** — multi-select of installed skills (populated from
`GET /api/skills` which returns all filesystem-discovered skills via the
agentskills plan's scan). Shows trust badge (builtin / trusted / community)
next to each. Saved into `scheduled_jobs.skills[]`.

**Allowed tools** — multi-select or freeform tag input from a fixed list of
known tool IDs (`bash`, `read_file`, `write_file`, `glob`, `grep`, `git_status`,
`git_diff`, …). Empty = unrestricted (show "All tools" placeholder). Saved into
`scheduled_jobs.tools_allow[]`.

The form can defer both fields behind an "Advanced" accordion — they are
optional and most operators will not set them.

#### Runtime enforcement (future — not in Phase 1)

The tool allowlist is stored; enforcement requires the agent runner (tmux +
Claude Code) to read `agents.tools_allow` and pass `--allowedTools` flags (if
Claude Code supports it) or inject a soul constraint block. This is deferred
to the agent runner integration phase. For now: store the list, surface it in
UI, pass it to the soul prompt as a `Boundaries` extra ("You may only call
these tools: bash, read_file").

Soul constraint injection (`internal/soul/compose.go`):

```go
if len(a.ToolsAllow) > 0 {
    layers.Extras = append(layers.Extras,
        fmt.Sprintf("Allowed tools (do not call others): %s",
            strings.Join(a.ToolsAllow, ", ")))
}
```

---

## Out of scope

- Coordinator spawning planning sessions via A2A (P.5b — noted, deferred).
- [NO_PLAN] silent suppression for automated runs that find nothing to do.
- Per-job model override (add `model TEXT` to `scheduled_jobs` later).
- TTL-based auto-archive of completed sessions (add after v2 ships and
  retention concerns become concrete).
- Webhook-triggered plans (`webhook_handlers` table handles that separately).
