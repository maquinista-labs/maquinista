# Dashboard (React SPA, embedded)

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
2. Ships as part of the single Go binary. One command: `./maquinista
   dashboard start`.
3. Frontend built on a stack the operator is productive in (React +
   TypeScript + shadcn/ui).
4. **Every user journey is covered by an automated integration test**
   that boots the Go binary against an ephemeral Postgres and drives
   the real UI. This is non-negotiable; regressions are caught by CI.
5. Phases are small and independently shippable; each phase lands via
   a handful of ~100-line commits, each green on `make test`.

## Non-goals (for v1)

- Multi-tenant auth / roles. Phase 4 adds single-operator auth; SaaS
  multi-tenancy is punted to `active/productization-saas.md`.
- Flow-graph / decision-transcript canvas (see ClawMetry). Conversation
  view + checkpoint timeline together give 80% of the observability.
- Docker / container admin. Out of scope — maquinista is one binary.
- AI chat *with the dashboard itself*. Every agent already accepts
  messages from the composer; dashboard-qua-chatbot is redundant.

---

## Decision needed — sign off before we build

Two stack choices gate implementation. Both are written up here so we
can agree before a single file is created. Change either by editing
this section and re-running the plan.

### Decision 1 — Frontend framework

Fixed constraints:

- **React + TypeScript.** Operator is fluent in React; TS is table
  stakes in 2026.
- **shadcn/ui** for components. It's the de-facto dashboard kit in
  2026 (copy-paste Radix + Tailwind, owned code, no runtime dep lock-
  in). Compatible with every option below; not the differentiator.
- **Builds to static files** that `go:embed` can pull into the binary.
  No Node runtime at serve time — we won't ship a JS server.
- **Small output** (< 300 KB gzip for the shell; code-split feature
  chunks) so the phone loads over cell.

Candidate frameworks (React + TS + shadcn):

| Option | Pros | Cons | Fit for embed |
|---|---|---|---|
| **A. Vite 7 + React 19 + TanStack Router + TanStack Query** | Minimal, boring, famously fast dev server. Static SPA build; plain `dist/` embeds cleanly. TanStack Router gives type-safe routes; TanStack Query handles server state + SSE cache invalidation. Every shadcn template "just works". | No SSR (not needed — auth gates the whole thing). Client-side routing means a first-paint flash on slow devices; mitigated by a static shell. | **Excellent.** `dist/` is ~15 files, all hashed, go:embed one-liner. Zero Node at runtime. |
| **B. Next.js 16 (static export, `output: 'export'`)** | Huge ecosystem, first-class shadcn, RSC story if we ever need it. App Router is familiar. | Static-export mode disables the half of Next.js that makes it interesting (middleware, RSC, route handlers, image optimisation). We'd be using 30% of Next for 100% of its config surface. Bigger bundle than Vite for the same features. | Good. Static export embeds. But we're paying Next's complexity tax to use a subset. |
| **C. TanStack Start (SPA mode) + shadcn** | Type-safe end-to-end; same router as A but with SSR-capable primitives we could grow into. Vite-based, fast. Official dashboard template with shadcn exists. | Still pre-1.0 in early 2026; API churn risk. SPA mode exists but the framework is SSR-first — we'd be "off-label". | Good in SPA mode. Adds framework surface we don't need today. |
| **D. Remix / React Router 7 (SPA mode)** | Same router codebase as TanStack Router-adjacent world, proven at scale. SPA mode (`ssr: false`) builds static. | Same "framework surface we don't use" problem as B and C. Smaller dashboard-template ecosystem vs. shadcn than Vite. | OK. No compelling advantage over A for our shape. |
| **E. Refine + Vite + shadcn** | Batteries-included admin framework: auth, data providers, CRUD scaffolding. Could shave weeks if our domain matched Refine's opinions. | Our domain *doesn't* match: we're not CRUDing resources, we're watching live mailboxes and streaming SSE. Refine's data-provider abstraction fights real-time. | OK. Heavy for what we'd use. |
| **F. Astro + React islands + shadcn** | Smallest bundle; ships HTML + selective hydration. Great for marketing-shaped apps. | Dashboard is highly interactive and live-updated — the "island" model adds friction for global SSE + route-spanning state. | OK. Wrong shape for a live dashboard. |

**Recommendation: Option A — Vite + React + TanStack Router + TanStack
Query.** Lowest framework surface, fastest dev loop, cleanest embed,
boring in the best way. shadcn/ui is pure components — portable
across all six options, so we keep optionality on the UI layer.

**Pick one before we start:** ☐ A (recommended) ☐ B ☐ C ☐ D ☐ E ☐ F

### Decision 2 — E2E / integration testing framework

User's original ask: Puppeteer. Current state of the art:

| Option | Pros | Cons |
|---|---|---|
| **Playwright (chosen)** | Industry default by 2026 (Puppeteer's weekly downloads were overtaken in 2024; Puppeteer is now in maintenance mode). First-class TypeScript. Auto-waiting defaults kill flaky tests. Trace viewer (`playwright show-trace`) + `codegen` cut debugging time by an order of magnitude. Cross-browser (Chromium/Firefox/WebKit) — WebKit catches the mobile-Safari bugs that hit our users. Parallel execution built-in. | Slightly larger install footprint (~200 MB incl. browser binaries); mitigated by `--with-deps chromium` only. |
| Puppeteer | Simpler API surface if you already know it. Chrome-only by default. | Maintenance-mode release cadence, no built-in test runner (needs Jest/Mocha), no auto-wait — flakes. No WebKit coverage. |
| Cypress | Great interactive runner; solid for smaller apps. | One-browser-at-a-time; iframes; real-browser clock model fights our SSE streams; slower in CI. |

**Decision: Playwright.** Per operator direction (revision of original
ask). Trade-off accepted: ~200 MB CI image growth in exchange for
auto-waiting, trace viewer, WebKit coverage, and a tool that's
actively maintained.

**Confirm before we start:** ☐ Playwright ☐ Puppeteer ☐ Cypress

### Decision 3 — Process model for `./maquinista dashboard`

The command shape is fixed: `./maquinista dashboard start|stop|status`.
Implementation has two sensible shapes:

| Option | Pros | Cons |
|---|---|---|
| **Standalone process, separate PID file** (`~/.maquinista/dashboard.pid`, default `:8900`) | Clean lifecycle — `start`/`stop` mirror main daemon. Can run dashboard without the bot (useful for observing a Pi-hosted daemon from a laptop). Trivial to reason about: one PID, one listener. | Two processes share a DB pool via `DATABASE_URL` (fine). If the main daemon is offline, action endpoints (composer, kill, rewind) have nothing to act on; dashboard degrades gracefully to read-only. |
| Embedded goroutine inside `maquinista start` | One PID; no extra command needed. Matches the Phase 3 action path (shares in-process state with the bot). | Can't observe a remote daemon. `dashboard start/stop` becomes weird — you're really controlling a flag on the main process. Harder to hot-reload during development. |
| Both (flag on `start` + standalone subcommand) | Maximum flex. | Two wire paths to maintain, two integration-test matrices. |

**Recommendation: standalone process.** It's the cleanest fit for the
operator's `start`/`stop` mental model and it enables the "observe a
remote Pi daemon from my laptop" case that
`active/pi-integration.md` implies. We can always add `--dashboard`
to `maquinista start` later; we cannot remove it once it's there.

**Pick one before we start:** ☐ Standalone (recommended) ☐ Embedded ☐ Both

---

## Chosen stack (assuming the recommendations above)

- **Frontend:** Vite 7 + React 19 + TypeScript 5.7 + shadcn/ui
  (Radix + Tailwind 4) + TanStack Router + TanStack Query + Zustand
  for the small amount of genuinely-client UI state (theme,
  disclosure, drawer).
- **Charts:** Recharts (shadcn's default, already battle-tested with
  the component set).
- **Live updates:** Server-Sent Events (`/dash/api/stream`). Simpler
  than WebSocket, mobile-friendly, one-way fits our "server pushes
  events, client re-queries" model. TanStack Query's
  `invalidateQueries` on each SSE event gives us the refresh story
  for free.
- **Backend:** Go `net/http` with `chi` router (already a transitive
  dep via other packages — confirm during Phase 0). JSON API under
  `/dash/api/*`. Static assets served from `internal/dashboard/web/
  dist/` via `//go:embed`.
- **Tests:** Playwright (integration/E2E) + Vitest + React Testing
  Library (component/unit) + Go `httptest` (API handlers) +
  `internal/dbtest.PgContainer` (migrations against real Postgres).
- **CLI:** `./maquinista dashboard start|stop|status` as a standalone
  process with its own PID file.

## CLI surface

```text
maquinista dashboard start   [--listen :8900] [--dev]
maquinista dashboard stop
maquinista dashboard status
```

Behaviour:

- `start` writes `~/.maquinista/dashboard.pid`. Refuses to start if a
  live PID is already there (same pattern as `cmd_start.go`).
- `stop` reads the PID file, sends SIGTERM, waits up to 10s, SIGKILLs
  if needed, removes the PID file. Tolerates a missing/stale file.
- `status` prints `running (PID N, listen X, uptime Y)` or `not
  running`. Non-zero exit on "not running" for scripting.
- `--dev` switches asset resolution from `//go:embed` to a reverse
  proxy at `http://127.0.0.1:5173` (the Vite dev server). One flag
  covers local iteration; CI and production always use the embed.

Config (in `internal/config/config.go`, section `Dashboard`):

```go
type Dashboard struct {
    Listen   string // default "127.0.0.1:8900"
    AuthMode string // "none" | "password" | "telegram" — Phase 4
    ThemeDefault string // "system" | "dark" | "light"
}
```

Env vars: `MAQUINISTA_DASHBOARD_LISTEN`,
`MAQUINISTA_DASHBOARD_AUTH`, `MAQUINISTA_DASHBOARD_THEME`.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ browser (mobile/desktop)                                     │
│  ┌─ SPA shell (React) ──────────────────────────────────┐    │
│  │  TanStack Router · Query · shadcn · Tailwind · SSE   │    │
│  └──────────────────────────────────────────────────────┘    │
└──────────────▲─────────────────────────────▲────────────────┘
               │ JSON  GET /dash/api/*       │ SSE /dash/api/stream
┌──────────────┴─────────────────────────────┴────────────────┐
│ maquinista dashboard (Go process)                            │
│  internal/dashboard/                                         │
│   server.go    — chi router, static embed, auth middleware   │
│   api.go       — JSON handlers (agents, inbox, outbox, ...)  │
│   stream.go    — SSE multiplexer over LISTEN agent_inbox_new,│
│                  agent_outbox_new, channel_delivery_new,     │
│                  agent_stop (migration 009 already emits)    │
│   auth.go      — Phase 4                                     │
│   web/         — Vite project (src, tests, playwright, dist) │
│                  dist/ embedded via //go:embed               │
└──────────────┬───────────────────────────────────────────────┘
               │ pgx pool (shared with main daemon)
┌──────────────▼───────────────────────────────────────────────┐
│ Postgres (system of record)                                  │
└──────────────────────────────────────────────────────────────┘
```

The dashboard process **never** writes to tmux directly. Action
endpoints (Phase 3) write to `agent_inbox` with
`origin_channel='dashboard'`; the per-agent sidecar consumes them,
same path as Telegram. This is why the dashboard can run on a
different host from the daemon.

## Phases

Each phase is ordered to keep commits small (target ≤150 lines of
production code per commit; tests can be larger). Every commit is
green on `make test`. Feature-specific commits are called out with
`→` bullets so the commit plan is part of the spec.

### Phase 0 — Scaffolding and CLI harness (no UI yet)

**Goal:** `./maquinista dashboard start` binds a port and serves a
single `/dash/api/healthz` JSON endpoint. No React, no embed yet.

→ **Commit 0.1** `cmd/maquinista/cmd_dashboard.go` — skeleton with
`start/stop/status` subcommands; PID file helpers mirror
`cmd_start.go`; no DB yet. Unit tests for PID-file lifecycle.

→ **Commit 0.2** `internal/dashboard/server.go` — chi router, one
`/dash/api/healthz` handler returning `{"ok":true,"version":...}`.
`httptest` covers it. Add `Dashboard` struct to `internal/config/
config.go` with env-var plumbing and a test.

→ **Commit 0.3** Graceful shutdown: `http.Server.Shutdown(ctx)` on
SIGTERM; test with `signal.Notify` + `t.Cleanup`.

→ **Commit 0.4** `make dashboard-dev` target; docs stub in
`docs/dashboard.md` (just the CLI surface, one page).

Gate: `./maquinista dashboard start`, `curl 127.0.0.1:8900/dash/api/
healthz`, `./maquinista dashboard stop` — all green, end-to-end, in
an integration test.

### Phase 1 — Frontend scaffolding + shadcn + first real route

**Goal:** loading `/dash/` renders an empty, branded shell with a
bottom nav and an empty "Agents" page, shadcn theme applied, Vite
dev mode working, go:embed of the prod build working.

→ **Commit 1.1** `internal/dashboard/web/` — `npm create vite@latest
web -- --template react-ts`, `package.json` pinned. `.gitignore`
for `node_modules` and `dist`. `make dashboard-web-install`, `make
dashboard-web-build`, `make dashboard-web-dev` Makefile targets.

→ **Commit 1.2** Tailwind 4 + shadcn init (`npx shadcn@latest
init`). Add base components: `button`, `card`, `badge`, `dropdown-
menu`, `sheet`, `tabs`, `toast`. Commit the generated files (per
shadcn's "you own the code" model).

→ **Commit 1.3** `src/routes/` via TanStack Router (file-based);
`src/lib/query.ts` for TanStack Query; `src/app.tsx` with bottom
nav (Agents / Inbox / Conversations / Jobs) + sticky header.
Three placeholder routes.

→ **Commit 1.4** `internal/dashboard/web/web.go` — `//go:embed
dist` + `http.FS` wiring with a strip-prefix for `/dash/`. Add
`--dev` flag that proxies to `127.0.0.1:5173` via
`httputil.ReverseProxy` when set. Integration test (Go side only)
asserts `GET /dash/` returns 200 with a non-empty HTML shell.

→ **Commit 1.5** Playwright install + first E2E test:
`tests/e2e/shell.spec.ts` — launches the Go binary against a
`dbtest.PgContainer`, navigates to `/dash/`, asserts the page has
the header and the four bottom-nav items. `make
dashboard-e2e` target. CI job added.

Gate: opening `/dash/` on a phone shows the empty shell; Playwright
trace is clean; Lighthouse PWA-ready score ≥ 80 (perf can be
improved later once there's real content).

### Phase 2 — Read-only Agents view with live SSE

**Goal:** the Agents list renders real rows from Postgres, with
status dots, and updates within 1s when an agent's state changes.

**Agent card shape** (preserved from prior plan):

```
┌─────────────────────────────┐
│ ● maquinista     claude 4.6 │   status dot · name · runner+model
│   last seen 2s ago          │   relative time from agents.last_seen
│   "Fixing webhook dedup…"   │   latest outbox excerpt, 80 chars
│   #main  #planner  43¢ today│   badges: role, soul template, cost
└─────────────────────────────┘
```

**Status dot logic** (preserved):
- green — `status='running'` AND `last_seen < 30s`
- amber — `status='running'` AND `last_seen ≥ 30s`
- red   — `status='running'` AND (`stop_requested` OR missing tmux_window)
- gray  — `status IN ('stopped','archived')`

→ **Commit 2.1** `internal/dashboard/api.go` — `GET /dash/api/
agents` returns `[{id, runner, model, status, last_seen,
last_outbox_excerpt, role, soul_template, unread_inbox_count}]`.
Query joins `agents`, `agent_settings`, and the most recent
`agent_outbox` row. Tested via `httptest` + `dbtest.PgContainer`.

→ **Commit 2.2** `src/features/agents/` — `useAgents()` hook
(TanStack Query, 30s stale, 5m gc), `AgentCard` component,
`AgentsPage`. Vitest + RTL snapshot.

→ **Commit 2.3** `internal/dashboard/stream.go` — SSE endpoint;
subscribes to PG NOTIFY on `agent_inbox_new`, `agent_outbox_new`,
`agent_stop`; emits JSON events. Backpressure via per-client
buffered channel; drop-oldest policy with a `retry:` hint on
reconnect. `httptest` covers the happy path; a unit test covers
drop-oldest.

→ **Commit 2.4** `src/lib/sse.ts` — EventSource wrapper with auto-
reconnect and exponential backoff; on each event, call
`queryClient.invalidateQueries(['agents'])` scoped to the relevant
agent. Vitest covers reconnect semantics with a fake EventSource.

→ **Commit 2.5** Playwright test `tests/e2e/agents-live.spec.ts`:
seeds an agent row → opens the page → asserts the card renders →
inserts an `agent_outbox` row via `pgx` → asserts the excerpt on
the card updates within 2s. This is the first full user-journey
test and the template for every subsequent phase.

Gate: phone shows the agent list; sending a Telegram message to an
agent updates the card text within 2s without a page refresh; the
e2e test above is green locally and in CI.

### Phase 3 — Agent detail with tabs (inbox, outbox, conversation)

**Goal:** tapping an agent opens a detail page with tabs; inbox +
outbox rows render; conversation view shows threaded bubbles.

→ **Commit 3.1** `GET /dash/api/agents/{id}` — details; `GET /dash/
api/agents/{id}/inbox` and `.../outbox` with `?before=<cursor>` +
`?limit=50`. Shared pagination helper in `internal/dashboard/api/
paginate.go`. httptest coverage.

→ **Commit 3.2** `src/features/agents/AgentDetail.tsx` — shadcn
`Tabs`; three sub-routes under `/agents/$id`. Inbox/outbox use
`useInfiniteQuery`; intersection-observer triggers the next page.

→ **Commit 3.3** `GET /dash/api/conversations/{id}` — threaded
timeline merging inbox + outbox rows by `created_at` within a
`conversation_id`. Bubbles right-aligned for the agent, left for
counterpart.

→ **Commit 3.4** `src/features/conversations/ConversationView.tsx`
— chat-style layout; sticky composer-footer stub (disabled until
Phase 5). Mobile Safari `pb-[env(safe-area-inset-bottom)]`.

→ **Commit 3.5** SSE deltas wire into inbox/outbox queries —
inserting a row scrolls the feed or shows a "N new messages ↓"
pill if the operator has scrolled up.

→ **Commit 3.6** Playwright: `tests/e2e/agent-detail.spec.ts`
asserts tap-through, tab switches, infinite scroll, and live SSE
insertion of a new outbox row.

### Phase 4 — KPIs, costs, jobs, system health

**Goal:** the agents page gains a KPI strip; a Jobs page lists
scheduled jobs + webhook handlers; a System Health panel exposes
pool + tmux + disk stats.

New tables (migrations — small, separately committable):

- Commit 4.1: `internal/db/migrations/024_agent_turn_costs.sql` —
  per-turn cost capture (schema as prior plan).
- Commit 4.2: `internal/db/migrations/025_model_rates.sql` — seed
  rates so historical costs survive a price change.
- Commit 4.3: `internal/monitor/cost.go` captures `usage.*_tokens`
  from the claude runner's stdout on each turn and inserts a row.
  Unit tests over a stdout fixture.

Then the UI:

- Commit 4.4: `GET /dash/api/kpis` — aggregated today/yesterday.
- Commit 4.5: `<KpiStrip />` with 6 tiles and a Recharts donut for
  cost-by-model.
- Commit 4.6: `GET /dash/api/jobs` + `<JobsPage />` listing
  `scheduled_jobs` and `webhook_handlers`.
- Commit 4.7: `GET /dash/api/health` + `<SystemHealthCard />`:
  pool stats (via `pgxpool.Stat()`), tmux window count, PID uptime,
  bot connection, disk used by worktrees.
- Commit 4.8: Playwright — runs 20 fake turns via seeded rows,
  asserts the KPI strip matches the SQL sum; toggles a job off
  from the UI and verifies `enabled=FALSE`.

### Phase 5 — Action surface (write)

**Goal:** the operator can reply, interrupt, kill, respawn, rewind,
void, reroute, and pin memory from the dashboard.

Endpoints (form-encoded, CSRF via double-submit cookie):

```
POST /dash/api/agents/{id}/inbox        composer
POST /dash/api/agents/{id}/interrupt
POST /dash/api/agents/{id}/kill
POST /dash/api/agents/{id}/respawn
POST /dash/api/agents/{id}/rewind       {checkpoint_id, mode}
POST /dash/api/outbox/{id}/void
POST /dash/api/outbox/{id}/reroute      {target_agent}
POST /dash/api/memory/{id}/pin          {pinned: bool}
```

Commits 5.1–5.8 map 1:1 to those endpoints. Each commit adds:
- The Go handler + httptest unit tests.
- The React affordance (sticky composer, long-press menu, or toast-
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

- Commit 6.1: `migration 026_dashboard_auth.sql` — operator_credentials, dashboard_sessions, dashboard_audit, dashboard_rate_buckets.
- Commit 6.2: `internal/dashboard/auth.go` — middleware, session
  cookies, PBKDF2-SHA256 password path.
- Commit 6.3: Telegram magic-link flow — bot handler in
  `internal/bot/handlers.go` for `/dash`, dashboard endpoint
  `GET /dash/auth/magic?token=...` exchanges token for session.
- Commit 6.4: `<LoginPage />` + `<AuthGate />` in the SPA.
- Commit 6.5: Audit logging wrapper around every Phase 5 endpoint
  + `<AuditPage />` under `/dash/audit`.
- Commit 6.6: Per-operator + per-IP rate limits (60 writes/min).
- Commit 6.7: Playwright specs for each auth mode — none, password
  (good/bad password, lockout), telegram (mock the bot webhook).

## Testing strategy

| Layer | Tool | Lives in | Runs in CI |
|---|---|---|---|
| Go unit | `go test` | `internal/dashboard/*_test.go` | yes, `make test` |
| Go integration (real Postgres) | `dbtest.PgContainer` | `internal/dashboard/*_integration_test.go` | yes, under `make test-integration` (Docker-gated) |
| JS unit / component | Vitest + RTL | `web/src/**/*.test.tsx` | yes, `make dashboard-web-test` |
| E2E / user journey | **Playwright** | `web/tests/e2e/*.spec.ts` | yes, `make dashboard-e2e` |

**Playwright harness** (the spine of the plan):

```
// web/tests/e2e/support/maquinistad.ts
export async function startDashboard(t: TestInfo) {
  const pg = await startPostgresContainer();         // testcontainers
  await runMigrations(pg.url);
  const proc = spawn('./maquinista', ['dashboard', 'start',
                     '--listen', '127.0.0.1:0'],
                    { env: { DATABASE_URL: pg.url } });
  const listen = await readLineMatching(proc.stdout, /listen=(\S+)/);
  t.teardown(async () => { proc.kill(); await pg.stop(); });
  return { url: `http://${listen}`, pg };
}
```

Every E2E spec follows the same shape: `startDashboard(t)` →
`page.goto(url)` → seed DB via `pg.client` → assert → mutate →
assert delta.

Test-data fixtures live in `web/tests/e2e/fixtures/*.sql` and are
applied via raw SQL (no Go-side seeder — keeps the harness thin).

**Coverage floor:** every Phase-N "gate" item has a Playwright spec.
CI fails if a Phase-N commit lands without its spec. This is the
guardrail that makes "well tested" real.

## Files

New:

```
cmd/maquinista/cmd_dashboard.go           start/stop/status subcmds
cmd/maquinista/cmd_dashboard_test.go      PID-file + lifecycle tests
internal/dashboard/server.go              router + middleware
internal/dashboard/api.go                 JSON handlers
internal/dashboard/stream.go              SSE multiplexer
internal/dashboard/auth.go                Phase 6
internal/dashboard/audit.go               Phase 6
internal/dashboard/web/web.go             go:embed + dev proxy
internal/dashboard/web/package.json       (Vite 7)
internal/dashboard/web/tsconfig.json
internal/dashboard/web/vite.config.ts
internal/dashboard/web/tailwind.config.ts
internal/dashboard/web/playwright.config.ts
internal/dashboard/web/src/main.tsx
internal/dashboard/web/src/app.tsx
internal/dashboard/web/src/routes/...     TanStack Router file tree
internal/dashboard/web/src/features/...   agents, conversations, jobs
internal/dashboard/web/src/components/ui  shadcn components (owned)
internal/dashboard/web/src/lib/{query,sse,api,types}.ts
internal/dashboard/web/tests/e2e/*.spec.ts
internal/dashboard/web/tests/e2e/support/maquinistad.ts
internal/db/migrations/024_agent_turn_costs.sql   Phase 4
internal/db/migrations/025_model_rates.sql        Phase 4
internal/db/migrations/026_dashboard_auth.sql     Phase 6
internal/monitor/cost.go                           Phase 4
docs/dashboard.md                          operator docs
```

Modified:

```
cmd/maquinista/main.go         register dashboardCmd
internal/config/config.go      Dashboard{} section + env vars
Makefile                       dashboard-web-install/build/dev/test/e2e
.gitignore                     internal/dashboard/web/{node_modules,dist,playwright-report}
.github/workflows/*.yml        install Node + playwright browsers
README.md                      one-liner pointer to docs/dashboard.md
```

## Verification per phase

Matches the "Gate" lines at the end of each phase above. All gates
are Playwright specs; phase is not merged until its spec is green.

## Rejected alternatives

- **Next.js 16 (static export).** Pays Next's complexity tax for a
  subset of features we actually use. Chosen against in Decision 1.
- **TanStack Start SPA mode.** Promising but framework is SSR-first
  and still pre-1.0 in April 2026; re-evaluate in v2.
- **Refine.** Opinionated around CRUD; fights live-mailbox shape.
- **Astro + islands.** Wrong fit for a highly-interactive, globally
  live-updated dashboard.
- **Puppeteer.** Maintenance-mode by 2026; Playwright overtook it on
  downloads in 2024, and its auto-wait + WebKit coverage close flake
  classes Puppeteer cannot. See Decision 2.
- **Cypress.** Great interactive runner, but one-browser-at-a-time
  and its synthetic clock model fights our SSE streams.

## Open questions

1. **Tailwind 4 vs 3.** Tailwind 4 is the default for new shadcn
   projects in 2026 and ships a Lightning-CSS-based engine; decide
   at Phase 1 whether to pin to v4 or pin conservatively to v3.4 if
   CI tooling lags.
2. **TanStack Router vs plain react-router.** Router is TS-sharper
   but one more library to learn. Plan assumes Router; flipping to
   react-router v7 is a half-day refactor if the DX is worse.
3. **Charting.** Recharts covers donut + line + sparkline. If we
   need anything richer, `uplot` is ~40 KB and works on mobile.
4. **SSE vs WebSocket.** SSE for the read path (Phase 2–4) is clean.
   Phase 5 write endpoints are request/response; if they grow long-
   running responses ("rewind in progress… done"), reconsider WS.
5. **Hot-reload in dev mode.** Vite HMR handles the React side.
   Backend hot-reload via `air` or similar is a nice-to-have; punt
   unless iteration speed bites.
6. **Operator identity & SaaS.** `active/productization-saas.md`
   will need Phase 6's auth primitives. Keep `operator_id` in
   `dashboard_audit` opaque so the migration to tenants is rename-
   only.
7. **Export.** CSV export of costs + audit log is probably yes;
   conversations are no (privacy). Defer to Phase 4/6.

## Interaction with other active plans

- `active/multi-agent-registry.md` — agent list reads the same
  `agents + agent_settings` view; archive action mirrors the CLI
  once Phase 5 ships.
- `active/agent-soul-db-state.md` — soul tab renders `agent_souls`;
  Phase 5 soul-edit is a textarea-per-section form; injection
  scanner runs server-side.
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
  before sidecar lands (single-consumer path today is fine); Phase
  5 read-back ("your message was picked up") lights up once
  sidecar is real.
- `active/productization-saas.md` — depends on Phase 6 auth.

---

**Before I touch code I need sign-off on Decisions 1, 2, 3 above.**
Reply with the picks (e.g. "A / Playwright / Standalone") and I
start at Phase 0 Commit 0.1.
