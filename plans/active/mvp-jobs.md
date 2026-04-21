# MVP Jobs

Minimal, shippable scheduled-job execution that spawns a fresh agent per run.
Aligned to `planning-scheduling-bridge.md` but scoped to what can ship in one
session without touching planning sessions, approval gates, or DAG graphs.

## Current state

`scheduled_jobs` rows exist but point at a **persistent** `agent_id`. The cron
daemon (`cmd/maquinista/cmd_cron.go`) injects the job prompt into that agent's
inbox — the existing agent accumulates job runs alongside its normal
conversation. The Jobs tab is read-only.

## Goal

When a scheduled job fires, **spawn a fresh agent** from a soul template with
the job prompt as its first inbox message. The dashboard Jobs tab gets:
1. Create / edit job form (name, cron, soul template, prompt, context, cwd)
2. Enable / disable toggle per job
3. "Run now" button
4. Per-job execution history showing spawned agent and status

---

## Architecture

### Spawn path (reusing existing machinery)

Every fresh spawn goes through the same sequence already used by
`spawn_topic_agent.go` and `reconcile_agents.go`:

```
INSERT agents (status='stopped', soul_template_id)
soul.CreateFromTemplate(agentID, templateID)
tmux.NewWindow(session, agentID, cwd, runnerCmd, env)
UPDATE agents SET status='running', tmux_window=$windowID
sidecarMgr.Spawn(agentID)            ← starts inbox consumer goroutine
mailbox.EnqueueInbox(agentID, prompt) ← first inbox message
INSERT job_executions (job_id, agent_id)
UPDATE scheduled_jobs SET next_run_at=... , last_run_at=NOW()
```

No new spawn primitives are needed. The job runner composes existing pieces.

### Agent ID pattern

`job-<job_name_slug>-<8-char uuid suffix>` — readable in tmux window list and
`agents` table without ambiguity. Example: `job-daily-digest-a3f7b2e1`.

### Job execution status

`job_executions.status` mirrors the spawned agent's `agents.status`:

| agents.status | job_executions.status |
|---------------|-----------------------|
| running / working / idle | running |
| stopped / dead / archived | completed |

A background DB trigger (or the job runner on completion) flips to
`completed`/`failed`. For MVP the dashboard just reads `agents.status` directly
via a JOIN — no trigger needed.

---

## Schema changes

### Migration `030_mvp_jobs.sql`

```sql
-- Allow soul-template-based jobs (agent spawned fresh per run)
ALTER TABLE scheduled_jobs
  ALTER COLUMN agent_id DROP NOT NULL,
  ALTER COLUMN agent_id DROP DEFAULT,
  ADD COLUMN soul_template_id TEXT REFERENCES soul_templates(id),
  ADD COLUMN context_markdown TEXT NOT NULL DEFAULT '',
  ADD COLUMN agent_cwd       TEXT NOT NULL DEFAULT '';

-- agent_id OR soul_template_id must be set (backcompat: old rows keep agent_id)
ALTER TABLE scheduled_jobs
  ADD CONSTRAINT chk_job_has_target
    CHECK (agent_id IS NOT NULL OR soul_template_id IS NOT NULL);

-- Execution history: one row per job run
CREATE TABLE job_executions (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  job_id     UUID NOT NULL REFERENCES scheduled_jobs(id) ON DELETE CASCADE,
  agent_id   TEXT REFERENCES agents(id) ON DELETE SET NULL,
  started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  ended_at   TIMESTAMPTZ
);
CREATE INDEX idx_job_executions_job   ON job_executions(job_id);
CREATE INDEX idx_job_executions_start ON job_executions(started_at DESC);
```

No changes to `agents` table — `role='user'` is fine for job-spawned agents;
they participate in the same sidecar / inbox / outbox pipeline as dashboard
agents.

---

## Backend changes

### J.1 — `internal/jobreg`: relax agent_id validation

`validateSchedule` currently requires `agent_id`. Update to require either
`agent_id` or `soul_template_id`:

```go
// in validateSchedule:
if s.AgentID == "" && s.SoulTemplateID == "" {
    return errors.New("agent_id or soul_template_id required")
}
```

Add `SoulTemplateID`, `ContextMarkdown`, `AgentCWD` fields to `Schedule` and
`ScheduleRow`. Update `AddSchedule` upsert to include them.

### J.2 — `internal/jobreg/runner.go` → `DispatchJob`

New function called by both the cron daemon and "run now" endpoint:

```go
type DispatchConfig struct {
    Pool        *pgxpool.Pool
    Cfg         *config.Config  // for TmuxSessionName, DefaultRunner, MaquinistaBin
    BotState    *state.State
    SidecarMgr  SidecarSpawner  // interface: Spawn(agentID)
    DefaultCWD  string
}

type SidecarSpawner interface {
    Spawn(agentID string)
}

// DispatchJob spawns a fresh agent for the given job row and enqueues its
// prompt. Returns the job_execution id. Safe to call concurrently (FOR UPDATE
// SKIP LOCKED on scheduled_jobs prevents double-fire from the cron loop).
func DispatchJob(ctx context.Context, job ScheduleRow, cfg DispatchConfig) (string, error)
```

Implementation:

```go
func DispatchJob(ctx context.Context, job ScheduleRow, dc DispatchConfig) (string, error) {
    if job.SoulTemplateID == "" {
        // Legacy path: inject into persistent agent inbox
        return dispatchInboxInject(ctx, job, dc)
    }
    return dispatchFreshSpawn(ctx, job, dc)
}

func dispatchFreshSpawn(ctx context.Context, job ScheduleRow, dc DispatchConfig) (string, error) {
    slug := slugify(job.Name)         // "daily digest" → "daily-digest"
    suffix := uuid.New().String()[:8]
    agentID := fmt.Sprintf("job-%s-%s", slug, suffix)

    cwd := job.AgentCWD
    if cwd == "" {
        cwd = dc.DefaultCWD
    }

    // 1. agents row
    if _, err := dc.Pool.Exec(ctx, `
        INSERT INTO agents
            (id, tmux_session, tmux_window, role, status, runner_type,
             cwd, window_name, stop_requested)
        VALUES ($1,$2,'','user','stopped',$3,$4,$1,FALSE)
    `, agentID, dc.Cfg.TmuxSessionName, dc.Cfg.DefaultRunner, cwd); err != nil {
        return "", fmt.Errorf("insert agent: %w", err)
    }

    // 2. soul
    if err := soul.CreateFromTemplate(ctx, dc.Pool, agentID, job.SoulTemplateID,
        soul.Overrides{}); err != nil {
        return "", fmt.Errorf("soul: %w", err)
    }

    // 3. tmux window
    hasSoul := true
    runnerCmd, env := resolveRunnerCommand(dc.Cfg, agentID, cwd, hasSoul, "")
    windowID, err := tmux.NewWindow(dc.Cfg.TmuxSessionName, agentID, cwd, runnerCmd, env)
    if err != nil {
        return "", fmt.Errorf("tmux window: %w", err)
    }
    if err := waitForRunnerReady(dc.Cfg.TmuxSessionName, windowID, 15*time.Second); err != nil {
        log.Printf("dispatch job %s: agent %s not ready: %v", job.Name, agentID, err)
    }

    // 4. flip to running
    if _, err := dc.Pool.Exec(ctx, `
        UPDATE agents SET tmux_window=$2, status='running', last_seen=NOW()
        WHERE id=$1
    `, agentID, windowID); err != nil {
        return "", fmt.Errorf("update agent: %w", err)
    }

    // 5. sidecar
    dc.SidecarMgr.Spawn(agentID)

    // 6. enqueue prompt (prepend context_markdown if set)
    text := buildJobPrompt(job)
    tx, err := dc.Pool.Begin(ctx)
    if err != nil {
        return "", err
    }
    defer tx.Rollback(ctx)
    _, _, err = mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
        AgentID:  agentID,
        FromKind: "job",
        Content:  []byte(fmt.Sprintf(`{"type":"text","text":%q}`, text)),
    })
    if err != nil {
        return "", fmt.Errorf("enqueue: %w", err)
    }

    // 7. job_executions row
    var execID string
    if err := tx.QueryRow(ctx, `
        INSERT INTO job_executions (job_id, agent_id)
        VALUES ($1, $2) RETURNING id::text
    `, job.ID, agentID).Scan(&execID); err != nil {
        return "", fmt.Errorf("insert execution: %w", err)
    }

    // 8. advance next_run_at
    next, _ := computeNext(job.Cron, job.Timezone, time.Now())
    tx.Exec(ctx, `
        UPDATE scheduled_jobs SET next_run_at=$2, last_run_at=NOW() WHERE id=$1
    `, job.ID, next)

    return execID, tx.Commit(ctx)
}

func buildJobPrompt(job ScheduleRow) string {
    var b strings.Builder
    if job.ContextMarkdown != "" {
        b.WriteString("## Context\n\n")
        b.WriteString(job.ContextMarkdown)
        b.WriteString("\n\n---\n\n")
    }
    // job.Prompt is JSONB {"type":"text","text":"..."} or {"text":"..."}
    b.WriteString(extractPromptText(job.Prompt))
    return b.String()
}
```

`resolveRunnerCommand`, `waitForRunnerReady`, `tmux.NewWindow` are in
`cmd/maquinista/` today. Move `resolveRunnerCommand` and `waitForRunnerReady`
to a shared package `internal/agentspawn` so `jobreg/runner.go` can import
them without a circular dependency. (Currently only `cmd/maquinista` needs
them; the move is mechanical.)

### J.3 — `cmd/maquinista/cmd_cron.go`: wire DispatchJob

Replace the existing per-agent inbox inject with `jobreg.DispatchJob`. Wire in
`sidecarMgr` which is already available in the cron goroutine via the same
closure used by `runDashboardAgentReconcile`.

### J.4 — Dashboard API routes (new)

**`POST /api/jobs`** — create job
```
Body: { name, cron_expr, timezone, soul_template_id, prompt, context_markdown, agent_cwd }
```
Calls `jobreg.AddSchedule`. Returns created job row.

**`PATCH /api/jobs/[id]`** — edit job (same fields as POST)

**`PATCH /api/jobs/[id]/toggle`** — flip `enabled`
```
Body: { enabled: bool }
```

**`POST /api/jobs/[id]/run`** — trigger immediately (bypasses `next_run_at`)
Calls `jobreg.DispatchJob` directly. Returns `{ exec_id }`.

**`DELETE /api/jobs/[id]`** — delete job (cascades to `job_executions`)

**`GET /api/jobs/[id]/executions`** — last 25 executions with agent status
```sql
SELECT je.id, je.agent_id, je.started_at, je.ended_at,
       a.status AS agent_status, a.tmux_window
FROM job_executions je
LEFT JOIN agents a ON a.id = je.agent_id
WHERE je.job_id = $1
ORDER BY je.started_at DESC
LIMIT 25
```

**`GET /api/soul-templates`** — list templates for the create-job dropdown
```sql
SELECT id, name, role, tagline FROM soul_templates ORDER BY name
```

---

## Dashboard changes

### J.5 — Jobs page: `jobs-page-client.tsx`

**Job card** (replace current read-only card):
```
┌────────────────────────────────────────────────────────┐
│ [enabled] daily-digest   0 9 * * *  (America/Sao_Paulo)│
│ soul: planner  next: Apr 22 09:00  last: Apr 21 09:01  │
│                                  [Run now] [Edit] [···] │
├────────────────────────────────────────────────────────┤
│ ▼ Last runs                                            │
│   Apr 21 09:01  job-daily-digest-a3f7b2e1  ✓ idle      │
│   Apr 20 09:00  job-daily-digest-b82fc091  ✓ idle      │
└────────────────────────────────────────────────────────┘
```

Each run row links to `/agents/[agent_id]` (existing agent detail page).

**Execution history** is loaded lazily on expand (GET `/api/jobs/[id]/executions`).

**Toggle** calls `PATCH /api/jobs/[id]/toggle`.

**Run now** calls `POST /api/jobs/[id]/run`, toasts on success.

### J.6 — Create / edit job modal

Trigger: "New job" button in Jobs page header.

Fields:
- **Name** — text input
- **Cron** — text input with human-readable preview (`cronstrue` or simple
  lookup table: "every day at 9am", etc.)
- **Timezone** — select (default UTC, common zones listed first)
- **Soul template** — dropdown populated from `GET /api/soul-templates`
- **Prompt** — textarea (what the agent should do on each run)
- **Context / background** — collapsible textarea for markdown context
  (project README excerpts, constraints, recurring reminders). Prepended to
  prompt at dispatch time under a `## Context` heading.
- **Working directory** — text input, optional (defaults to daemon default)

On submit: `POST /api/jobs` or `PATCH /api/jobs/[id]`.

---

## `listJobs` query update

Extend to include `soul_template_id`, `context_markdown`, `agent_cwd`, and
execution count:

```sql
SELECT sj.id, sj.name, sj.cron_expr, sj.timezone,
       sj.agent_id, sj.soul_template_id, sj.enabled,
       sj.next_run_at, sj.last_run_at,
       sj.context_markdown, sj.agent_cwd,
       st.name AS soul_template_name,
       (SELECT COUNT(*) FROM job_executions WHERE job_id = sj.id) AS run_count
FROM scheduled_jobs sj
LEFT JOIN soul_templates st ON st.id = sj.soul_template_id
ORDER BY sj.enabled DESC, sj.next_run_at ASC
```

---

## Sequencing

```
J.1  jobreg schema + validation update     ← migration 030, jobreg.go changes
J.2  DispatchJob + agentspawn package      ← new internal/agentspawn, jobreg/runner.go
J.3  wire into cron daemon                 ← cmd_cron.go
J.4  dashboard API routes                  ← /api/jobs POST/PATCH/DELETE + /api/soul-templates
J.5  jobs-page-client.tsx: cards + history ← frontend
J.6  create/edit modal                     ← frontend
```

J.1–J.3 are pure backend and testable without the dashboard. J.4–J.6 build on
J.1–J.3 and can ship together.

---

## What this deliberately leaves out

- Planning sessions, DAG graphs, approval gates (`planning-scheduling-bridge.md` P.2–P.11)
- `auto_approve` flag — all job agents run freely; no plan-gate
- Telegram reply channel — no card sent to Telegram on job fire; agent output
  is visible in dashboard only
- Per-job skill injection / tool allowlists (planning-scheduling-bridge P.10)
- YAML-based job reconcile still works via the existing path (no changes to
  `jobreg.Reconcile` except the new optional fields)
- `warm_spawn_before` — not implemented (field stays in DB, ignored at dispatch)

Each omission maps to a named step in `planning-scheduling-bridge.md` and can
be layered on later without breaking the spawn model introduced here.

---

## Acceptance criteria

1. Create a scheduled job via the dashboard form (soul template = planner,
   cron = any future expression, prompt = "List the 3 most recent files in cwd").
2. Hit "Run now" — a new `job-<slug>-<uuid>` agent appears in the Agents tab
   within 20 seconds, status transitions running → idle after completing.
3. Job execution row appears in the Jobs tab execution history with correct
   agent link.
4. Toggle job disabled — next scheduled fire is skipped.
5. Existing YAML-defined scheduled_jobs (with `agent_id`) continue to work via
   the legacy inject path.
