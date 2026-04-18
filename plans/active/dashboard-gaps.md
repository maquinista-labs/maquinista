# Dashboard gaps — cross-agent views and other unshipped UI

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres
> is the system of record**.

## Context

`plans/active/dashboard.md` Phases 0–6 shipped the per-agent axis
(`/agents`, `/agents/[id]` with inbox / outbox / chat tabs), the KPIs
bar, the jobs page, the composer + interrupt / kill / respawn
actions, and Phase 6 auth + audit + rate limit. Three follow-up
plans exist (`dashboard-cost-sse`, `dashboard-rewind-actions`,
`dashboard-telegram-auth`) but each is scoped tightly to its own
feature.

A handful of **cross-agent** surfaces were scaffolded in Phase 1 as
placeholder pages and never wired to real data. They render static
copy that lies (`"Wired up in Phase 3"`) when the cross-agent
queries and endpoints were actually deferred. From the operator's
side the symptom is: per-agent views work, but the top-level Inbox
and Conversations nav entries show empty stubs with no explanation.

This plan collects every such gap and lands them one commit at a
time. It is a **living triage doc** — as new gaps surface during
operator use, append a G.N entry under `## Scope` and add a
reference line under `### Triage backlog`.

## Scope

One small commit per gap, each with a Playwright spec and green on
`make test` + `make dashboard-e2e`. Endpoints follow the existing
conventions (`src/lib/queries.ts` for read, `src/lib/actions.ts`
for write, `src/app/api/...` route handlers, shadcn for UI).

### Commit G.1 — Global Inbox feed

Today: `src/app/(dash)/inbox/page.tsx` renders a placeholder
paragraph with `data-testid="inbox-placeholder"`. The sidebar links
to it.

What it should be: a unified feed of `agent_inbox` rows across all
agents that are still in flight, so the operator sees everything
waiting to be handled without clicking each agent.

- `listGlobalInbox(pool, opts)` in `src/lib/queries.ts`:
  ```sql
  SELECT i.id, i.agent_id, a.name AS agent_name,
         i.from_kind, i.from_id, i.content, i.status,
         i.enqueued_at
  FROM agent_inbox i
  JOIN agents a ON a.id = i.agent_id
  WHERE i.status IN ('pending','processing')
  ORDER BY i.enqueued_at DESC
  LIMIT $1
  ```
  Default limit 100. Accept optional `status` filter so future
  callers can widen to include `done` / `failed`.
- `GET /api/inbox` route handler — calls the helper, returns
  `{items: InboxRow[]}`.
- Replace the placeholder page with a server component that reads
  via `listGlobalInbox(getPool())` and hands off to a small client
  component (`<GlobalInboxList>`) that uses the existing
  `InboxRow` presentational card from the per-agent Inbox tab.
  Each row links to `/agents/[id]` with the inbox tab active.
- Empty state: "No pending or processing messages." (not a
  placeholder — an actual empty-state card).
- Playwright: seed three inbox rows across two agents with mixed
  statuses (`pending`, `processing`, `done`), visit `/inbox`,
  assert exactly two rows are rendered, assert the `done` row is
  absent, tap a row and assert we land on `/agents/[id]`.

### Commit G.2 — Global Conversations feed

Today: `src/app/(dash)/conversations/page.tsx` mirrors the Inbox
placeholder (`data-testid="conversations-placeholder"`).

What it should be: a list of recent conversations (one row per
`conversation_id`) merging `agent_inbox` + `agent_outbox`, with
the latest message as a preview. Tapping a row opens the detail
view (which already exists per-conversation via
`/api/conversations/[id]`).

- `listConversations(pool, opts)` in `src/lib/queries.ts`:
  ```sql
  WITH last_msg AS (
    SELECT conversation_id, agent_id,
           MAX(at) AS last_at,
           (ARRAY_AGG(content ORDER BY at DESC))[1] AS preview,
           COUNT(*) AS msg_count
    FROM (
      SELECT conversation_id, agent_id, content, enqueued_at AS at
      FROM agent_inbox WHERE conversation_id IS NOT NULL
      UNION ALL
      SELECT conversation_id, agent_id, content, created_at AS at
      FROM agent_outbox WHERE conversation_id IS NOT NULL
    ) m
    GROUP BY conversation_id, agent_id
  )
  SELECT lm.conversation_id, lm.agent_id, a.name AS agent_name,
         lm.last_at, lm.preview, lm.msg_count
  FROM last_msg lm
  JOIN agents a ON a.id = lm.agent_id
  ORDER BY lm.last_at DESC
  LIMIT $1
  ```
  Default limit 50.
- `GET /api/conversations` — returns `{items: ConversationRow[]}`.
- Client page: server component fetch + `<ConversationList>`
  client component. Each row shows agent name, preview (truncated
  to two lines), relative timestamp, message count. Tap opens
  `/agents/[id]?tab=chat&conversation=<id>` (the agent detail
  chat tab already supports the `conversation` query param from
  Phase 3).
- Playwright: seed two conversations across two agents, visit
  `/conversations`, assert both rows, assert order (newest first),
  tap one and assert we navigate to the chat tab with the right
  conversation filter applied.

### Commit G.3 — Rename agent from the UI

Today: the `agents` table PK is `id` (e.g. `t-<chat>-<thread>`),
which is stable but hostile to humans. Migration 014 added a
nullable `handle` column (the friendly `@mention` alias,
`^[a-z0-9_-]{2,32}$`, unique case-insensitive). The CLI already
accepts `--handle` at `agent add`, but there is no rename path —
not in the CLI, not in the dashboard. The agent detail page shows
`id` as the title, which the operator cannot change.

What it should be: the operator opens an agent's detail page,
taps the title (or a pencil affordance next to it), types a new
display name, and the agent is renamed everywhere the UI shows
it. `id` stays untouched (it is referenced by inbox / outbox /
conversations / soul rows), tmux session / window names stay
untouched, shell history stays untouched. Only the operator-
facing label changes.

Implementation notes:

- Reuse `handle`. Do not add a new column — `handle` already
  exists, is already unique, and is already what Telegram uses
  for `@mention` routing. The dashboard just needs to let the
  operator write it and read it back as the display name.
- `renameAgent(pool, id, handle)` in `src/lib/actions.ts`:
  `UPDATE agents SET handle = $2 WHERE id = $1`. Let the DB's
  existing unique index on `lower(handle)` surface collisions —
  return `409` on constraint violation, `400` on regex-invalid
  input, `404` on unknown id.
- `POST /api/agents/[id]/rename` — body `{handle: string | null}`.
  `null` clears the handle (revert to id as display). Emits an
  `agent.renamed` audit row.
- Every place the UI currently prints `a.id` as a title gets a
  `displayName(a)` helper that returns `a.handle ?? a.id`. Files
  touched: `agent-card.tsx`, `agents-page-client.tsx`,
  `agent-detail-header.tsx` (Phase 3), any conversation / inbox
  row that prints the agent label.
- Title-tap affordance on the detail page opens a shadcn
  `<Dialog>` with a single input + Save / Clear buttons. Disable
  Save while input fails the regex.
- Playwright: seed an agent with `handle=null`, open its detail
  page, assert title equals `id`, click the pencil, type
  `my-coder`, save, assert title switches to `my-coder`, reload
  and assert it persists, then try to set the same handle on a
  second agent and assert the 409 error toast.

### Commit G.4 — Seed default agents on first boot

Today: a fresh install boots with zero agents. The operator has
to run `maquinista agent add <id>` three times before anything
productive happens. The ops plans reference roles like
"coordinator", "planner", and "coder" but no convention seeds
them.

What it should be: on `maquinista start`, if the default agents
are missing, create them with role-appropriate souls; if the
rows already exist, leave them alone and let the existing
`reconcileAgentPanes` path (in `cmd/maquinista/reconcile_agents.go`)
bring their tmux windows back up. Reconcile already handles the
"row exists, tmux pane missing" case — it respawns the runner and
resumes the session via `session_id` when set.

Seed set (id, handle, role hint, soul summary):

| id                | handle         | role           | soul (core_truths summary) |
|---|---|---|---|
| `seed-coordinator`| `coordinator`  | fleet router   | "You triage incoming user goals, split them into tasks, and route each to the right specialist agent. You do not write code yourself." |
| `seed-planner`    | `planner`      | spec writer    | "You turn user intent into a step-by-step plan before any code is written. You cite file paths and line numbers. You never skip the thinking step." |
| `seed-coder`      | `coder`        | implementer    | "You implement the planner's spec exactly. You run tests after every change. You ask before making scope-expanding refactors." |

Implementation notes:

- New migration `028_seed_default_agents.sql`:
  1. Insert three rows into `soul_templates` (ids `coordinator`,
     `planner`, `coder`) with the copy above. `is_default`
     stays FALSE — the existing `default` template keeps its
     role as the fallback.
  2. **Do not** insert `agents` rows from SQL — agent creation
     requires tmux session / window resolution that lives in Go.
- New helper `seedDefaultAgents(ctx, cfg, pool)` in
  `cmd/maquinista/seed_agents.go`. Runs from `cmd_start.go`
  **before** `reconcileAgentPanes`. For each of the three ids:
  - `SELECT 1 FROM agents WHERE id = $1` — skip if present.
  - Otherwise call the existing `agent.Add` path (same one the
    CLI uses), passing `--soul-template <id>`, `--handle <h>`,
    and a `cwd` of the operator's configured workspace root.
    Insert with `status='stopped'` so the normal reconcile loop
    picks the row up and brings the pane online.
- Idempotency: the existence check means repeated boots are
  no-ops. Respawn logic is unchanged — reconcile already
  distinguishes "missing pane, recoverable row" from "archived"
  / "stop_requested".
- Opt-out: `MAQUINISTA_SKIP_SEED_AGENTS=1` env var bypasses the
  helper. Useful for tests and for operators who want a clean
  slate.
- Telemetry: `log.Printf` each decision ("seeding coordinator",
  "coordinator already exists, skipping"). The reconcile loop
  already prints its own respawn log.

Tests:

- Go unit test in `cmd/maquinista/seed_agents_test.go`:
  - Empty DB → three agent rows inserted, three soul rows linked
    to the correct templates, `handle` set correctly.
  - Pre-existing `seed-coordinator` row → only the other two
    get inserted. The pre-existing row is not modified.
  - `MAQUINISTA_SKIP_SEED_AGENTS=1` → zero rows inserted.
- Go integration test that runs the full startup flow on an
  ephemeral Postgres and asserts (a) the three agents exist, (b)
  `reconcileAgentPanes` brings their tmux panes up, (c) a second
  start does not duplicate them.
- Playwright: on a fresh boot, visit `/agents` and assert all
  three default agents are listed with their handles as the
  display names (leans on G.3 landing first so the display helper
  is in place).

### Triage backlog

Gaps discovered but not yet scoped into a commit. Move up into
`### Commit G.N` with a real spec before implementing.

- (seed: add entries here as we find them — e.g. missing empty
  states, dead nav entries, copy that says "Phase X" but refers
  to shipped work, broken keyboard shortcuts, etc.)

## Non-goals

- No new write actions. Those live in
  `active/dashboard-rewind-actions.md`.
- No live-update plumbing for the new feeds. Polling via the
  existing query refetch interval is fine; live updates arrive
  via `active/dashboard-cost-sse.md` pattern once SSE generalises.
- No redesign of the per-agent tabs — this plan only fills
  top-level cross-agent surfaces.

## Test coverage

Every commit ships with (a) a unit test for the new query helper
in `internal/dashboard/web/src/lib/queries.test.ts` and (b) a
Playwright spec in `tests/e2e/` covering at minimum the happy
path, the empty state, and one navigation assertion.

## Rollout

Commits are independent and shippable in any order. Land G.1
first — it is the most-visible gap and unblocks operator trust
that the nav entries are real.
