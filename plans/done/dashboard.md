# Dashboard (Next.js, supervised by the Go binary)

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres is
> the system of record**. No markdown files, no JSON on disk, no
> dotfiles for persistent state.

## Context

Today the only way to see what maquinista is doing is:

- `tmux attach` to the session — fine for one agent, useless on mobile.
- `psql` the mailbox tables — bad ergonomics, not observable over cell.
- Telegram bot admin commands — discoverable but tied to chat scroll,
  no visuals, no cost view, no checkpoint surface.

The operator — usually me, on a phone, during a deploy or a school run
— needs a glance-view of the fleet: who's running, who's stuck, what
each agent last said, how much it's cost, and one-tap actions
(approve / interrupt / rewind). Maquinista's dashboard's primary axis
is **agent** (one tmux window, per-agent soul / memory / checkpoint /
worktree), with inbox/outbox/conversations as cross-cutting feeds.

## Goals

1. Operator gets a **mobile-first**, live, readable view of the fleet
   without opening tmux or psql.
2. Ships with the Go binary and launches via `./maquinista dashboard
   start|stop|status`. Operator never runs `node` or `npm` by hand at
   runtime — the Go CLI supervises the Node child process.
3. Frontend built on a stack the operator is productive in and that
   **extends cleanly** as the dashboard grows (React + TypeScript +
   Next.js + shadcn/ui).
4. **Every user journey is covered by an automated integration test**
   that boots the Go binary against an ephemeral Postgres and drives
   the real UI. This is non-negotiable; regressions are caught by CI.
5. Phases are small and independently shippable; each phase lands via
   a handful of ~100-line commits, each green on `make test`.

## Non-goals (for v1)

- Multi-tenant auth / roles. Phase 6 adds single-operator auth; SaaS
  multi-tenancy is punted to `active/productization-saas.md`.
- Flow-graph / decision-transcript canvas (see ClawMetry). Conversation
  view + checkpoint timeline together give 80% of the observability.
- Docker / container admin. Out of scope — maquinista is one binary.
- AI chat *with the dashboard itself*. Every agent already accepts
  messages from the composer; dashboard-qua-chatbot is redundant.

---

## Decision needed — sign off before we build

Three stack choices gate implementation. Change any by editing this
section and re-running the plan.

### Decision 1 — Frontend framework

Fixed constraints:

- **React + TypeScript.** Operator is fluent in React; TS is table
  stakes in 2026.
- **shadcn/ui** for components. De-facto dashboard kit (copy-paste
  Radix + Tailwind, owned code). Compatible with every option below;
  not the differentiator.
- **Node runtime is acceptable at serve time.** Operator already has
  Node installed for Claude Code CLI (see README prerequisites). The
  Go binary supervises the Node child so the operator's mental model
  stays "one command: `./maquinista dashboard start`".
- **Must embed cleanly in release artifact.** Build output gets
  `go:embed`-ed into the Go binary and extracted to a runtime cache
  dir on first start. Operator never clones the web source.

Candidate frameworks (React + TS + shadcn):

| Option | Pros | Cons | Fit for embed |
|---|---|---|---|
| **A. Next.js 16 (App Router, RSC, `output: 'standalone'`)** — **RECOMMENDED** | Largest React ecosystem in 2026; easiest to onboard contributors. Full framework surface — App Router + Server Components + Server Actions + Route Handlers + middleware — so Phase 5 actions, Phase 6 auth, and future extensions (billing portal, webhook UIs, SaaS multi-tenancy) don't require swapping frameworks. First-class shadcn/ui integration; every premium shadcn dashboard template targets Next. Built-in streaming Route Handlers for SSE, Server Actions for form posts (no hand-rolled CSRF), `next/image` + bundle splitting out of the box. Mature auth via Better Auth / Auth.js / NextAuth. Image, font, and route prefetch optimisations that a hand-rolled SPA re-implements badly. | Node runtime required at serve time (mitigated — Node already required for Claude Code). `.next/standalone` output adds ~50 MB to the embedded asset vs. ~2 MB for a Vite SPA. Two processes to supervise (Go daemon + Node child) — the supervision is written once in `cmd_dashboard.go` and reused. Cold-start slower than a static SPA (~500 ms vs ~50 ms); negligible for a long-lived dashboard. | **Good.** `output: 'standalone'` produces a self-contained server directory. `//go:embed` the tarball, extract to cache dir on first `start`, exec `node server.js`. One extract; warm starts reuse the cache. |
| **B. Vite 7 + React 19 + TanStack Router + TanStack Query** | Minimal, boring, famously fast dev server. Static SPA build; plain `dist/` embeds cleanly. No Node at runtime. | Hand-roll everything Next gives for free: auth middleware, SSE plumbing, API routes (either Go or separate Node server), form CSRF. Extensibility ceiling lower — adding SSR-only surfaces (billing, marketing) later means a framework swap. Smaller dashboard-template ecosystem vs Next. | Excellent for embed (~2 MB dist) but the saving is paid back in framework surface we re-implement. |
| **C. TanStack Start (SPA mode)** | Type-safe end-to-end; Vite-based; Vite 7 version shipping through 2026. Official dashboard template exists. | Still pre-1.0 in April 2026; API churn risk. Smaller ecosystem than Next. SPA mode is "off-label" in an SSR-first framework. | Good in SPA mode. |
| **D. Remix / React Router 7 (SPA mode)** | Proven at scale. SPA mode (`ssr: false`) builds static. | Same "framework we only half-use" problem as C. Smaller shadcn ecosystem vs Next. | OK. |
| **E. Refine + Vite + shadcn** | Batteries-included admin framework: auth, data providers, CRUD scaffolding. | Our domain *doesn't* match: we're watching live mailboxes and streaming SSE, not CRUDing resources. Refine's data-provider abstraction fights real-time. | OK. Heavy for what we'd use. |
| **F. Astro + React islands + shadcn** | Smallest bundle; ships HTML + selective hydration. | Dashboard is highly interactive and live-updated — the "island" model adds friction for global SSE and route-spanning state. | OK. Wrong shape for a live dashboard. |

**Recommendation: Option A — Next.js 16 (App Router).** Pays the
Node-runtime tax once, in exchange for a framework that absorbs
every extension we'll want (auth middleware, Server Actions,
streaming Route Handlers, RSC, i18n, image optimisation) without
replatforming. shadcn/ui portability means we keep UI-layer
optionality regardless.

**Pick one before we start:** ☑ A (recommended) ☐ B ☐ C ☐ D ☐ E ☐ F

### Decision 2 — E2E / integration testing framework

| Option | Pros | Cons |
|---|---|---|
| **Playwright (chosen)** | Industry default by 2026 (Puppeteer's weekly downloads were overtaken in 2024; Puppeteer is now in maintenance mode). First-class TypeScript. Auto-waiting defaults kill flaky tests. Trace viewer (`playwright show-trace`) + `codegen` cut debugging time by an order of magnitude. Cross-browser (Chromium/Firefox/WebKit) — WebKit catches the mobile-Safari bugs that hit our users. Parallel execution built-in. | Slightly larger install footprint (~200 MB incl. browser binaries); mitigated by `--with-deps chromium` only. |
| Puppeteer | Simpler API surface if you already know it. Chrome-only by default. | Maintenance-mode release cadence, no built-in test runner (needs Jest/Mocha), no auto-wait — flakes. No WebKit coverage. |
| Cypress | Great interactive runner; solid for smaller apps. | One-browser-at-a-time; iframes; real-browser clock model fights our SSE streams; slower in CI. |

**Decision: Playwright.** Trade-off accepted: ~200 MB CI image
growth in exchange for auto-waiting, trace viewer, WebKit coverage,
and a tool that's actively maintained.

**Confirm before we start:** ☑ Playwright ☐ Puppeteer ☐ Cypress

### Decision 3 — Process model for `./maquinista dashboard`

Command shape is fixed: `./maquinista dashboard start|stop|status`.
With Next.js chosen (Decision 1), the dashboard is a Node child
process the Go CLI supervises. Implementation variants:

| Option | Pros | Cons |
|---|---|---|
| **Standalone supervisor, separate PID file** (`~/.maquinista/dashboard.pid`, default `:8900`; Go CLI extracts the embedded Next build on first start, spawns `node server.js`, pipes stdout/stderr into `~/.maquinista/logs/dashboard.log`) | Clean lifecycle — `start`/`stop` mirror main daemon. Can run dashboard without the bot (useful for observing a Pi-hosted daemon from a laptop). Go CLI restarts the Node child on crash (bounded retry). | Two processes share a DB pool via `DATABASE_URL` (fine — Next connects directly via `pg`). If the main daemon is offline, action endpoints have nothing to act on; dashboard degrades gracefully to read-only. |
| Embedded via `maquinista start --dashboard` | One PID-observed parent; no extra command needed. | Can't observe a remote daemon without the daemon. `dashboard start/stop` becomes weird — a flag on the main process. |
| Both (flag on `start` + standalone subcommand) | Maximum flex. | Two wire paths to maintain, two integration-test matrices. |

**Recommendation: standalone supervisor.** Cleanest fit for the
operator's `start`/`stop` mental model and enables "observe a
remote Pi daemon from my laptop". `--dashboard` on `maquinista
start` can be added later as sugar over the same supervisor.

**Pick one before we start:** ☑ Standalone (recommended) ☐ Embedded ☐ Both

---

## Chosen stack (assuming the recommendations above)

- **Frontend framework:** Next.js 16 (App Router, React 19, TypeScript
  5.7, `output: 'standalone'`).
- **Components:** shadcn/ui (Radix primitives + Tailwind 4).
- **Data fetching:** Server Components for first paint; TanStack
  Query on the client for SSE-invalidated reads and mutations.
- **Charts:** Recharts.
- **Live updates:** Streaming Route Handler at `/api/stream` — uses
  `ReadableStream` + `text/event-stream`; listens to PG NOTIFY
  channels the daemon already emits (`agent_inbox_new`,
  `agent_outbox_new`, `channel_delivery_new`, `agent_stop`).
  TanStack Query's `invalidateQueries` on each SSE event gives
  refresh for free.
- **Auth:** Better Auth (Phase 6). Small, TS-native, supports
  password + TOTP + magic link; trivial to wire a "Telegram magic
  link" provider that reuses the existing bot.
- **Postgres access:** `pg` (node-postgres) with a module-level pool;
  shared connection string with the Go daemon.
- **Node runtime:** `node:22` (LTS through late 2027). Bundled via
  `output: 'standalone'` — operator's system `node` is not called
  directly; we ship the binary we tested against *if* Node is
  missing. Phase 0 decides whether to embed Node itself or require
  it as a prerequisite (see Open questions).
- **Go side:** supervisor only — `cmd/maquinista/cmd_dashboard.go`
  extracts the embedded `.next/standalone` tarball to
  `~/.maquinista/dashboard/<version>/`, spawns the Node server,
  writes the PID file, tails logs, restarts on crash (bounded).
- **Tests:** Playwright (integration/E2E) + Vitest + React Testing
  Library (component/unit) + Go unit tests (supervisor) +
  `internal/dbtest.PgContainer` (shared Postgres fixture).

## CLI surface

```text
maquinista dashboard start   [--listen :8900] [--dev] [--no-embed]
maquinista dashboard stop
maquinista dashboard status
maquinista dashboard logs    [--follow]
```

Behaviour:

- `start`
  1. Reads `~/.maquinista/dashboard.pid`; refuses to start if live.
  2. Extracts embedded `.next/standalone` tarball to
     `~/.maquinista/dashboard/<version>/` (skips if already extracted
     for this binary's version).
  3. Spawns `node server.js` with `PORT`, `DATABASE_URL`,
     `MAQUINISTA_CONFIG`, `HOSTNAME` (per Next standalone contract)
     injected. Logs to `~/.maquinista/logs/dashboard.log` via piped
     stdio.
  4. Writes the child PID to the PID file.
  5. Supervises: on unexpected exit, restart with exponential
     backoff capped at 5 retries in 60 s; after that, leave
     stopped and log loudly.
- `stop` — reads PID, sends SIGTERM to the child, waits up to 10 s
  for clean shutdown, SIGKILLs if needed, removes the PID file.
  Tolerates stale/missing PID.
- `status` — `running (PID N, listen X, uptime Y, version V)` or
  `not running`. Non-zero exit on "not running".
- `logs [--follow]` — tails `~/.maquinista/logs/dashboard.log`.
- `--dev` — skips extraction; runs `next dev` against
  `internal/dashboard/web/` in the working copy. For local
  iteration only; CI and release always use embedded mode.
- `--no-embed` — runs `next start` against a pre-built
  `internal/dashboard/web/.next/standalone`; skips the extract
  step. For CI perf and local debugging.

Config (in `internal/config/config.go`, section `Dashboard`):

```go
type Dashboard struct {
    Listen       string // default "127.0.0.1:8900"
    AuthMode     string // "none" | "password" | "telegram" — Phase 6
    ThemeDefault string // "system" | "dark" | "light"
    NodeBin      string // default "node"; override for non-PATH installs
}
```

Env vars: `MAQUINISTA_DASHBOARD_LISTEN`,
`MAQUINISTA_DASHBOARD_AUTH`, `MAQUINISTA_DASHBOARD_THEME`,
`MAQUINISTA_DASHBOARD_NODE_BIN`.

## Architecture

```
┌────────────────────────────────────────────────────────────────┐
│ browser (mobile/desktop)                                       │
│  Next.js App Router · shadcn/ui · Tailwind · TanStack Query    │
└───────────▲──────────────────────────────────▲─────────────────┘
            │ RSC / fetch(/api/…)              │ GET /api/stream (SSE)
┌───────────┴──────────────────────────────────┴─────────────────┐
│ node (Next.js standalone server) — child of `maquinista         │
│ dashboard`                                                      │
│  app/                   Server Components + Client Components   │
│  app/api/…/route.ts     JSON Route Handlers (agents, inbox, …)  │
│  app/api/stream/route.ts  SSE via ReadableStream                │
│  lib/db.ts              pg Pool (direct to Postgres)            │
│  lib/auth.ts            Better Auth (Phase 6)                   │
│  middleware.ts          auth gate (Phase 6)                     │
└───────────▲────────────────────────────────────────────────────┘
            │ supervised via PID + stdio pipes
┌───────────┴────────────────────────────────────────────────────┐
│ maquinista dashboard (Go CLI) — thin supervisor                │
│  cmd/maquinista/cmd_dashboard.go                                │
│  internal/dashboard/supervisor.go   extract + spawn + restart   │
│  internal/dashboard/embed.go        //go:embed standalone.tgz   │
└────────────────────────────────────────────────────────────────┘

            ┌────────────────────────────────┐
            │ maquinista (main Go daemon)    │  — independent lifecycle
            │ bot · orchestrator · monitor   │    same DATABASE_URL
            └────────────────┬───────────────┘
                             │
┌────────────────────────────▼───────────────────────────────────┐
│ Postgres (system of record)                                    │
│  agents, agent_inbox, agent_outbox, agent_souls, agent_memory, │
│  scheduled_jobs, webhook_handlers, agent_turn_costs (Ph. 4)    │
└────────────────────────────────────────────────────────────────┘
```

The dashboard process **never** writes to tmux directly. Action
endpoints (Phase 5) write to `agent_inbox` with
`origin_channel='dashboard'`; the daemon's per-agent sidecar
consumes them, same path as Telegram. The dashboard can therefore
run on a different host from the daemon — they coordinate through
Postgres, per §0.

## Phases

Each phase is ordered to keep commits small (≤150 lines of
production code per commit; tests can be larger). Every commit is
green on `make test`. Feature-specific commits are called out with
`→` bullets so the commit plan is part of the spec.

### Phase 0 — Go supervisor + CLI harness (no UI yet)

**Goal:** `./maquinista dashboard start` spawns a trivial child
process (a stub `node -e 'http.createServer(…).listen(PORT)'` or a
placeholder binary), supervises it, responds to `stop`/`status`.
Healthcheck lives on the child. No Next.js code yet.

→ **Commit 0.1** `cmd/maquinista/cmd_dashboard.go` — skeleton with
`start/stop/status/logs` subcommands; PID-file helpers mirror
`cmd_start.go`; no child spawn yet. Unit tests for PID-file
lifecycle (create/stale-cleanup/respect-live-pid).

→ **Commit 0.2** `internal/dashboard/supervisor.go` — `Supervisor`
type: `Start(ctx, cmd, env, logPath)`, `Stop(ctx)`, `Status()`;
backoff with counter; stdio pipe to log file. Unit tests with a
`sleep 3600` stub confirm lifecycle.

→ **Commit 0.3** Wire supervisor into `cmd_dashboard.go`. Child is
a one-liner Node healthcheck server
(`node -e "require('http').createServer((q,r)=>r.end('{\\"ok\\":true}')).listen(process.env.PORT)"`).
Integration test: `dashboard start` → `curl :8900/api/healthz` →
`dashboard stop`. Asserts PID file is gone after stop.

→ **Commit 0.4** Graceful shutdown: SIGTERM cascade, 10 s grace
before SIGKILL; test via `signal.Notify` + `t.Cleanup`.

→ **Commit 0.5** `dashboard logs [--follow]` subcommand; test via
`os.Pipe` + line assertions.

→ **Commit 0.6** `make dashboard-dev` target; docs stub in
`docs/dashboard.md` (one page — CLI surface + prerequisites).
`Dashboard` struct added to `internal/config/config.go` with
env-var plumbing and a test.

Gate: green integration test covering start/stop/status/logs
against the Node stub.

### Phase 1 — Next.js scaffold, shadcn, embed, first route

**Goal:** `./maquinista dashboard start` extracts the embedded
standalone build, runs `next start`, serves `/` as a branded shell
with bottom nav (Agents / Inbox / Conversations / Jobs). Dev mode
(`--dev`) runs `next dev` in-tree. Playwright covers the shell.

→ **Commit 1.1** `internal/dashboard/web/` — `npx create-next-app@
latest web --ts --tailwind --app --src-dir --import-alias '@/*' --
use-npm`. Pin versions in `package.json`. Set `output:
'standalone'` in `next.config.mjs`. `.gitignore` for
`node_modules`, `.next`, `playwright-report`.

→ **Commit 1.2** Tailwind 4 wiring verified; `npx shadcn@latest
init` + add base components: `button`, `card`, `badge`,
`dropdown-menu`, `sheet`, `tabs`, `skeleton`, `toast`. Commit
generated files (shadcn's "you own the code" model).

→ **Commit 1.3** `src/app/layout.tsx` (root layout with bottom
nav, sticky header, theme provider) + four placeholder route
groups: `app/(dash)/agents/page.tsx`,
`app/(dash)/inbox/page.tsx`, `app/(dash)/conversations/page.tsx`,
`app/(dash)/jobs/page.tsx`. Theme toggle via `next-themes`.

→ **Commit 1.4** `app/api/healthz/route.ts` — replaces the Phase-0
stub. Returns `{ok:true, version, uptime}`. `make
dashboard-web-build` produces `.next/standalone`.

→ **Commit 1.5** `internal/dashboard/embed.go` — `//go:embed
web/dist/standalone.tgz` (generated by `make
dashboard-web-package`); `Extract(version, dest)` helper;
integrity check via SHA-256 manifest. Unit tests with a small
fixture tarball.

→ **Commit 1.6** Supervisor now spawns the extracted `node
server.js`. Go integration test: `dashboard start` →
`curl :8900/api/healthz` (served by Next) → `dashboard stop`.

→ **Commit 1.7** Playwright install + first E2E:
`tests/e2e/shell.spec.ts` — boots Go CLI against
`dbtest.PgContainer`, navigates to `/`, asserts header + four
bottom-nav items; traces on failure. `make dashboard-e2e`
target; CI job added.

Gate: opening `/` on a phone shows the empty shell; Playwright
trace clean; Lighthouse PWA-ready ≥ 80.

### Phase 2 — Read-only Agents view with live SSE

**Goal:** the Agents list renders real rows from Postgres, with
status dots, and updates within 1 s when an agent's state changes.

**Agent card shape**:

```
┌─────────────────────────────┐
│ ● maquinista     claude 4.6 │   status dot · name · runner+model
│   last seen 2s ago          │   relative time from agents.last_seen
│   "Fixing webhook dedup…"   │   latest outbox excerpt, 80 chars
│   #main  #planner  43¢ today│   badges: role, soul template, cost
└─────────────────────────────┘
```

**Status dot logic**:
- green — `status='running'` AND `last_seen < 30s`
- amber — `status='running'` AND `last_seen ≥ 30s`
- red   — `status='running'` AND (`stop_requested` OR missing tmux_window)
- gray  — `status IN ('stopped','archived')`

→ **Commit 2.1** `lib/db.ts` — module-level `pg` Pool keyed on
`DATABASE_URL`; graceful close on `process.on('SIGTERM')`. Vitest
covers singleton behaviour.

→ **Commit 2.2** `app/api/agents/route.ts` — Route Handler returns
`[{id, runner, model, status, last_seen, last_outbox_excerpt,
role, soul_template, unread_inbox_count}]`. Joins `agents`,
`agent_settings`, most recent `agent_outbox`. Covered by a
Playwright API-level test (raw `fetch` against `/api/agents`,
seeded via SQL).

→ **Commit 2.3** Server Component `app/(dash)/agents/page.tsx`
renders the list from `/api/agents` at first paint. Client child
`AgentCard` consumes the row. Snapshot test via Vitest + RTL.

→ **Commit 2.4** `app/api/stream/route.ts` — streaming Route
Handler. Opens a dedicated `pg` listener client, subscribes to
`agent_inbox_new`, `agent_outbox_new`, `agent_stop`, emits SSE
frames. `ReadableStream` with proper teardown on `abort`.
Vitest covers frame format; Playwright covers connection.

→ **Commit 2.5** `lib/sse.ts` — client hook `useDashStream()`.
Wraps `EventSource` with auto-reconnect + exponential backoff;
on each event, `queryClient.invalidateQueries(['agents', id])`.
Vitest covers reconnect semantics with a fake EventSource.

→ **Commit 2.6** TanStack Query wired on the client: `useAgents()`
(30 s stale, 5 m gc), used by `AgentsPage` after first paint to
keep the list fresh. SSR payload hydrates the query cache.

→ **Commit 2.7** Playwright `tests/e2e/agents-live.spec.ts` —
seeds an agent row → opens `/agents` → asserts card renders →
inserts an `agent_outbox` row via `pgx` → asserts excerpt on the
card updates within 2 s. Template for every subsequent E2E.

Gate: phone shows agent list; a Telegram reply updates the card
text within 2 s without refresh; e2e green.

### Phase 3 — Agent detail with tabs (inbox, outbox, conversation)

**Goal:** tapping an agent opens `/agents/[id]` with three tabs:
inbox, outbox, conversation view with chat bubbles.

→ **Commit 3.1** `app/api/agents/[id]/route.ts` (details);
`app/api/agents/[id]/inbox/route.ts` and `.../outbox/route.ts`
with `?before=<cursor>&limit=50`. Shared pagination util in
`lib/paginate.ts`. Playwright API tests.

→ **Commit 3.2** `app/(dash)/agents/[id]/layout.tsx` + nested
routes for `inbox`, `outbox`, `conversation`. shadcn `Tabs`.
Inbox/outbox use `useInfiniteQuery`; intersection-observer
triggers next page.

→ **Commit 3.3** `app/api/conversations/[id]/route.ts` — threaded
timeline merging inbox + outbox by `created_at` within a
`conversation_id`. Bubbles right-aligned for the agent, left
for counterpart.

→ **Commit 3.4** `ConversationView` Client Component — chat-style
layout; sticky composer footer stub (disabled until Phase 5);
`pb-[env(safe-area-inset-bottom)]`.

→ **Commit 3.5** SSE deltas wire into inbox/outbox queries —
inserting a row scrolls feed or shows "N new messages ↓" pill
if the operator has scrolled up.

→ **Commit 3.6** Playwright `agent-detail.spec.ts` — tap-through,
tab switches, infinite scroll, live SSE insertion.

### Phase 4 — KPIs, costs, jobs, system health

**Goal:** Agents page gains a KPI strip; Jobs page lists scheduled
jobs + webhook handlers; a System Health panel exposes pool / tmux
/ disk stats.

Migrations (Go-side, separately committable):

→ **Commit 4.1** `internal/db/migrations/024_agent_turn_costs.sql`
— per-turn cost capture.

→ **Commit 4.2** `internal/db/migrations/025_model_rates.sql` —
seed rates so historical costs survive a price change.

→ **Commit 4.3** `internal/monitor/cost.go` — daemon-side capture
of `usage.*_tokens` from the claude runner's stdout; inserts
`agent_turn_costs` rows. Unit tests over a stdout fixture.

Then the UI (Next side):

→ **Commit 4.4** `app/api/kpis/route.ts` — aggregated today/
yesterday/month (active agents, inbox in-flight, tokens, cost).

→ **Commit 4.5** `<KpiStrip />` (Server Component) + Recharts
cost-by-model donut. E2E: seed 20 turns, assert tile matches SQL
sum.

→ **Commit 4.6** `app/api/jobs/route.ts` + `app/(dash)/jobs/
page.tsx` listing `scheduled_jobs` and `webhook_handlers`. Row
tap opens a drawer with the prompt template + last N inbox rows.

→ **Commit 4.7** `app/api/health/route.ts` +
`<SystemHealthCard />`: pool stats (via `pg.Pool.totalCount`
etc.), tmux window count (shelled from Node via a small
`tmux list-windows` wrapper in the daemon exposed via a
`/health/tmux` Go endpoint the Next side calls), PID uptime,
bot connection, disk used by worktrees (`fs.statfs`).

→ **Commit 4.8** Playwright: 20 fake turns → KPI assertion +
toggle a job off from UI + verify `enabled=FALSE`.

### Phase 5 — Action surface (write)

**Goal:** operator can reply, interrupt, kill, respawn, rewind,
void, reroute, and pin memory from the dashboard.

Implemented as **Server Actions** where possible (progressive
enhancement, no hand-rolled CSRF — Next 16 Server Actions ship
with `next-action` signed tokens); Route Handlers where an
explicit HTTP surface is needed (e.g. Telegram bot reuses the
endpoint).

```
POST /api/agents/[id]/inbox         composer (Server Action)
POST /api/agents/[id]/interrupt     (Server Action)
POST /api/agents/[id]/kill          (Server Action)
POST /api/agents/[id]/respawn       (Server Action)
POST /api/agents/[id]/rewind        {checkpoint_id, mode}
POST /api/outbox/[id]/void
POST /api/outbox/[id]/reroute       {target_agent}
POST /api/memory/[id]/pin           {pinned: bool}
```

Commits 5.1–5.8 map 1:1. Each commit adds:
- The Server Action / Route Handler + Vitest unit test.
- The React affordance (sticky composer, long-press menu, toast-
  confirm modal).
- A Playwright spec that drives the affordance end-to-end and
  asserts the row appears in `agent_inbox` or the target table.

Quick-reply presets (`agent_settings.roster ->> 'quickReplies'`)
land in Commit 5.9 as chips above the composer.

### Phase 6 — Auth, audit, external exposure

Pick one per deployment: `none` / `password` / `telegram`. Default
`none` + loopback; `telegram` is the maquinista-native option
(operator messages the bot `/dash`, receives a magic link with a
10-minute token — no new credentials to remember).

Backed by **Better Auth** — modern, TS-native, pluggable
providers. Custom "Telegram magic-link" provider wraps the
existing bot.

→ **Commit 6.1** `migration 026_dashboard_auth.sql` —
operator_credentials, dashboard_sessions, dashboard_audit,
dashboard_rate_buckets.

→ **Commit 6.2** `lib/auth.ts` — Better Auth config; password
provider (Argon2id).

→ **Commit 6.3** Telegram magic-link provider: Go bot handler in
`internal/bot/handlers.go` for `/dash`; Next callback
`app/auth/magic/route.ts` exchanges token for session.

→ **Commit 6.4** `middleware.ts` gates every `/api/*` and
`/(dash)/*` route; `app/auth/page.tsx` login form.

→ **Commit 6.5** Audit logging wrapper around every Phase 5
endpoint + `<AuditPage />` under `/audit`.

→ **Commit 6.6** Per-operator + per-IP rate limits (60 writes/min
default); sliding-window in Postgres.

→ **Commit 6.7** Playwright specs per auth mode — none, password
(good/bad password, lockout), telegram (mock the bot webhook).

## Testing strategy

| Layer | Tool | Lives in | Runs in CI |
|---|---|---|---|
| Go unit (supervisor, embed) | `go test` | `internal/dashboard/*_test.go`, `cmd/maquinista/cmd_dashboard_test.go` | yes, `make test` |
| Go integration (real Postgres) | `dbtest.PgContainer` | `cmd/maquinista/cmd_dashboard_integration_test.go` | yes, under `make test-integration` (Docker-gated) |
| JS unit / component | Vitest + RTL | `web/**/*.test.tsx`, `web/lib/**/*.test.ts` | yes, `make dashboard-web-test` |
| E2E / user journey | **Playwright** | `web/tests/e2e/*.spec.ts` | yes, `make dashboard-e2e` |

**Playwright harness** (the spine of the plan):

```ts
// web/tests/e2e/support/maquinistad.ts
export async function startDashboard(t: TestInfo) {
  const pg = await startPostgresContainer();          // testcontainers
  await runMigrations(pg.url);
  const proc = spawn('./maquinista',
    ['dashboard', 'start', '--listen', '127.0.0.1:0', '--no-embed'],
    { env: { ...process.env, DATABASE_URL: pg.url } });
  const listen = await readLineMatching(
    proc.stdout, /listen=(\S+)/);
  t.teardown(async () => { proc.kill(); await pg.stop(); });
  return { url: `http://${listen}`, pg };
}
```

`--no-embed` points the supervisor at a pre-built `.next/
standalone` in the working tree — CI avoids the extract step per
test.

Every E2E spec: `startDashboard(t)` → `page.goto(url)` → seed DB
via `pg.client` → assert → mutate → assert delta.

Fixtures live in `web/tests/e2e/fixtures/*.sql` and apply via raw
SQL (keeps the harness thin).

**Coverage floor:** every Phase-N "Gate" item has a Playwright
spec. CI fails if a Phase-N commit lands without its spec.

## Files

New (Go side):

```
cmd/maquinista/cmd_dashboard.go                start/stop/status/logs
cmd/maquinista/cmd_dashboard_test.go           PID-file + lifecycle
cmd/maquinista/cmd_dashboard_integration_test.go
internal/dashboard/supervisor.go               spawn + restart
internal/dashboard/supervisor_test.go
internal/dashboard/embed.go                    //go:embed + extract
internal/dashboard/embed_test.go
internal/db/migrations/024_agent_turn_costs.sql   Phase 4
internal/db/migrations/025_model_rates.sql        Phase 4
internal/db/migrations/026_dashboard_auth.sql     Phase 6
internal/monitor/cost.go                          Phase 4
docs/dashboard.md                              operator docs
```

New (Next.js side — under `internal/dashboard/web/`):

```
package.json                         pinned Next 16, React 19, TS 5.7
next.config.mjs                      output: 'standalone'
tsconfig.json
tailwind.config.ts
playwright.config.ts
middleware.ts                        Phase 6
src/app/layout.tsx                   root layout, theme, bottom nav
src/app/(dash)/agents/page.tsx       Phase 2
src/app/(dash)/agents/[id]/...       Phase 3 (layout + tabs)
src/app/(dash)/conversations/...     Phase 3
src/app/(dash)/jobs/page.tsx         Phase 4
src/app/(dash)/audit/page.tsx        Phase 6
src/app/api/healthz/route.ts         Phase 1
src/app/api/agents/route.ts          Phase 2
src/app/api/agents/[id]/route.ts     Phase 3
src/app/api/agents/[id]/inbox/route.ts, outbox/route.ts
src/app/api/conversations/[id]/route.ts
src/app/api/kpis/route.ts            Phase 4
src/app/api/jobs/route.ts            Phase 4
src/app/api/health/route.ts          Phase 4
src/app/api/stream/route.ts          Phase 2 (SSE)
src/app/actions/*.ts                 Phase 5 (Server Actions)
src/app/auth/...                     Phase 6
src/components/ui/*                  shadcn components (owned)
src/components/dash/*                feature components
src/lib/db.ts                        pg Pool singleton
src/lib/sse.ts                       client SSE hook
src/lib/query.ts                     TanStack Query setup
src/lib/auth.ts                      Better Auth (Phase 6)
src/lib/paginate.ts                  shared cursor pagination
src/lib/types.ts                     shared DTOs
tests/e2e/shell.spec.ts              Phase 1
tests/e2e/agents-live.spec.ts        Phase 2
tests/e2e/agent-detail.spec.ts       Phase 3
tests/e2e/kpis.spec.ts, jobs.spec.ts Phase 4
tests/e2e/actions.spec.ts            Phase 5
tests/e2e/auth-*.spec.ts             Phase 6
tests/e2e/support/maquinistad.ts     shared harness
tests/e2e/fixtures/*.sql             seeding
```

Modified:

```
cmd/maquinista/main.go         register dashboardCmd
internal/config/config.go      Dashboard{} section + env vars
Makefile                       dashboard-web-install/build/dev/test/e2e
                               + dashboard-web-package (build .next
                               standalone.tgz consumed by //go:embed)
.gitignore                     internal/dashboard/web/{node_modules,
                               .next,playwright-report}
.github/workflows/*.yml        install Node 22 + playwright browsers +
                               make dashboard-web-package before go build
README.md                      one-liner pointer to docs/dashboard.md
                               + note on Node prerequisite
```

## Verification per phase

Matches the "Gate" lines at the end of each phase above. All gates
are Playwright specs; phase is not merged until its spec is green.

## Rejected alternatives

- **Vite + React + TanStack (SPA).** Simpler to embed (~2 MB vs
  ~50 MB) and zero Node at runtime, but re-implements auth,
  middleware, API routes, SSE plumbing, and form CSRF from scratch.
  Extensibility ceiling bites the moment we add a billing portal,
  i18n, or marketing surface. See Decision 1.
- **Next.js (static export, `output: 'export'`).** Disables the
  half of Next that makes it interesting (middleware, RSC, Route
  Handlers, Server Actions). We'd pay Next's config surface for a
  Vite SPA's feature set. See Decision 1.
- **TanStack Start SPA mode.** Promising but pre-1.0 in April 2026;
  framework is SSR-first and SPA mode is off-label. Re-evaluate
  post-1.0.
- **Refine.** Opinionated around CRUD; fights live-mailbox shape.
- **Astro + islands.** Wrong fit for a highly-interactive, globally
  live-updated dashboard.
- **Puppeteer.** Maintenance-mode by 2026; Playwright overtook it
  on downloads in 2024, and auto-wait + WebKit coverage close
  flake classes Puppeteer cannot. See Decision 2.
- **Cypress.** Great interactive runner, but one-browser-at-a-time
  and its synthetic clock model fights our SSE streams.

## Open questions

1. **Ship Node, or require it on PATH?** Operator already has Node
   for Claude Code (README prereq). Decision: **require Node 22 on
   PATH** for v1; add an optional embedded Node tarball behind
   `--embed-node` if operators ask. Phase 0 refuses to start with a
   clear error if `node --version` is missing or < 20.
2. **Tailwind 4 vs 3.** Tailwind 4 is the default for new shadcn
   projects in 2026. Plan pins v4; fallback to v3.4 if CI tooling
   lags at scaffolding time.
3. **pg vs postgres.js.** `pg` is the boring choice and matches the
   Go daemon's driver semantics (`pgx` over the same wire protocol).
   Plan pins `pg` for Phase 2; `postgres.js` considered if `pg`'s
   LISTEN ergonomics bite.
4. **TanStack Query vs SWR.** Both work with Next. TanStack has
   richer mutation + infinite-query APIs (infinite scroll in
   Phase 3). Plan pins TanStack Query.
5. **SSE vs WebSocket.** SSE via streaming Route Handler is clean
   for Phase 2–4 reads. Phase 5 writes are request/response; if
   long-running writes grow ("rewind in progress… done"),
   reconsider WS.
6. **Operator identity & SaaS.** `active/productization-saas.md`
   will need Phase 6's auth primitives. Keep `operator_id` in
   `dashboard_audit` opaque so migration to tenants is rename-only.
7. **Charting.** Recharts covers donut + line + sparkline. If we
   need richer (cumulative cost lines, response-time histograms),
   `visx` or `uplot` are candidates.
8. **Export.** CSV export of costs + audit log probably yes;
   conversations no (privacy). Defer to Phase 4/6.

### Phase 7 — Telegram `/dashboard` command with Cloudflare quick tunnel

**Goal:** operator sends `/dashboard [duration]` from mobile and receives a
public URL (e.g. `https://random-name.trycloudflare.com`) they can open
directly in a mobile browser, with no SSH setup. The tunnel self-destructs
after the requested duration.

**Prerequisite:** `cloudflared` installed on the host machine:
```bash
curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 \
  -o /usr/local/bin/cloudflared && chmod +x /usr/local/bin/cloudflared
```
No Cloudflare account needed — quick tunnels are ephemeral and anonymous.

**Command surface:**
```
/dashboard          → start tunnel, default 15 min
/dashboard 30m      → start tunnel, 30 minutes
/dashboard 1h       → start tunnel, 1 hour
/dashboard 0        → start tunnel, no expiry (explicit)
/dashboard_stop     → tear down tunnel immediately
```

If a tunnel is already running, `/dashboard` returns the existing URL and
remaining time instead of starting a new one.

**Architecture:**

```
Telegram /dashboard
  └─→ Bot.handleDashboard()
        1. b.tunnel.IsRunning() → return existing URL + TTL if yes
        2. ensure dashboard process is up (exec `maquinista dashboard start`)
        3. b.tunnel.Start(ctx, duration)
              spawns: cloudflared tunnel --no-autoupdate --url localhost:8900
              scans stderr for: https://[a-z0-9-]+\.trycloudflare\.com
              returns URL once found (timeout 10 s)
        4. schedules auto-stop via context.WithTimeout(duration)
        5. replies with URL + inline [Open] button
              on expiry → bot sends follow-up "Tunnel expired. /dashboard to reopen."
```

**New file — `internal/tunnel/manager.go`:**

```go
type Manager struct {
    mu      sync.Mutex
    cmd     *exec.Cmd
    url     string
    cancel  context.CancelFunc
    notify  func(msg string) // callback → sends Telegram message on expiry
}

func (m *Manager) Start(ctx context.Context, dur time.Duration) (string, error)
func (m *Manager) Stop()
func (m *Manager) URL() string
func (m *Manager) IsRunning() bool
func (m *Manager) RemainingTime() time.Duration
```

`Start()` implementation:
1. Spawns `cloudflared tunnel --no-autoupdate --url localhost:8900` via
   `exec.CommandContext` with a derived context.
2. Scans `stderr` line-by-line with a 10 s deadline until the regex
   `https://[a-z0-9-]+\.trycloudflare\.com` matches.
3. If `dur > 0`, wraps the process context with `context.WithTimeout(dur)`.
   When the timeout fires, `cloudflared` is killed and `notify()` is called.
4. If `dur == 0`, the process runs until `Stop()` is called or the bot shuts down.

**Changes to existing files:**

| File | Change |
|---|---|
| `internal/bot/bot.go` | Add `tunnel *tunnel.Manager` field; init in `New()`; call `Stop()` in shutdown |
| `internal/bot/commands.go` | Add `"/dashboard"` and `"/dashboard_stop"` cases to `handleCommand()` |
| `internal/bot/dashboard_commands.go` | New file: `handleDashboard()`, `handleDashboardStop()` |

**`handleDashboard()` sketch:**

```go
func (b *Bot) handleDashboard(msg *tgbotapi.Message) {
    // parse optional duration from msg.CommandArguments()
    // if tunnel running: reply with URL + remaining time, return
    // ensure dashboard process is up
    // start tunnel with parsed duration (default 15m)
    // build reply with URL string + InlineKeyboardMarkup url-button
}
```

**Edge cases:**

| Scenario | Handling |
|---|---|
| `cloudflared` not in PATH | Reply with install one-liner |
| URL not found within 10 s | Reply with error; kill the stuck process |
| Dashboard process not running | Auto-start via `maquinista dashboard start` before tunneling |
| `/dashboard` called while tunnel active | Return existing URL + `expires in Xm` |
| Bot restart | Tunnel dies with process; fresh `/dashboard` starts a new one |
| `dur == 0` + bot restart | Same — no persistent tunnel state needed |

**Security note:** Quick tunnel URLs are public and unauthenticated. The
obscurity of a random subdomain is the only barrier until Phase 6 auth ships.
Running `/dashboard` before Phase 6 is intentional operator risk — documented
and opt-in.

→ **Commit 7.1** `internal/tunnel/manager.go` — `Manager` with `Start/Stop/
URL/IsRunning/RemainingTime`; unit tests covering: happy path URL parse,
10 s timeout, duration expiry, `Stop()` idempotence, `cloudflared` not
on PATH error.

→ **Commit 7.2** `internal/bot/dashboard_commands.go` — `handleDashboard()`
and `handleDashboardStop()`; mock `Manager` interface in tests; covers: fresh
start default duration, custom duration parse, already-running reply, stop.

→ **Commit 7.3** Wire `Manager` into `Bot` struct; register commands in
`commands.go`; integration test: spawn real `cloudflared` (skipped if not
on PATH) → assert URL returned → assert tunnel reachable → assert expiry
message sent after TTL.

Gate: sending `/dashboard 2m` from Telegram returns a working URL; the bot
sends "Tunnel expired" 2 minutes later; `/dashboard` after expiry opens a
new tunnel.

## Interaction with other active plans

- `active/multi-agent-registry.md` — agent list reads the same
  `agents + agent_settings` view; archive action mirrors CLI once
  Phase 5 ships.
- `active/agent-soul-db-state.md` — soul tab renders `agent_souls`;
  Phase 5 soul-edit is a textarea-per-section Server Action;
  injection scanner runs server-side.
- `active/agent-memory-db.md` — memory tab gets two sub-tabs
  (blocks, archival); pin toggle is Phase 5. Blocks UI blocks on
  that plan's Phase 0.
- `active/agent-to-agent-communication.md` — conversation view
  already handles `from_kind='agent'`; `kind='a2a'` threads render
  both participants.
- `active/checkpoint-rollback.md` — checkpoint tab is a vertical
  timeline; rewind is a confirm-then-POST flow. Blocked on
  per-agent sidecar for the rewind write path.
- `active/per-agent-sidecar.md` — dashboard action endpoints drop
  `agent_inbox` rows; sidecar consumes them. Dashboard ships
  before sidecar lands (single-consumer path today is fine);
  Phase 5 read-back ("your message was picked up") lights up once
  sidecar is real.
- `active/productization-saas.md` — depends on Phase 6 auth.

---

**Before I touch code I need sign-off on Decisions 1, 2, 3 above.**
Reply with picks (defaults: `A / Playwright / Standalone`) and I
start at Phase 0 Commit 0.1.
