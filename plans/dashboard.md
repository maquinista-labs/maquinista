# Dashboard (mobile-first)

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

Today the only way to see what maquinista is doing is:

- `tmux attach` to the session (good for one agent, useless on mobile)
- `psql` the mailbox tables (bad ergonomics, not observable over cell)
- Telegram bot admin commands (`/show`, `/inbox`, …) — discoverable but
  tied to chat scroll, no visuals, no cost view, no checkpoint surface

The operator — usually me, on a phone, during a deploy or a school run
— needs a glance-view of the fleet: who's running, who's stuck, what
each agent last said, how much it's cost, and one-tap actions
(approve / interrupt / rewind). This plan ports the *feature set* of
the hermes and openclaw dashboards to maquinista's domain model,
shaped for mobile first.

### Reference implementations worth studying

- **hermes-webui (sanchomuzax)** — closest shape for a clean SPA:
  KPI cards, live session feed, FTS over history, skills browser,
  WebSocket live updates, dark/light theme.
- **Hermes HUD** — 13 tabs incl. cost-per-model, keyboard palette.
- **openclaw-dashboard (mudrii)** — zero-deps Go-style layout with
  live top metrics bar, cost donut, cron table, AI chat, 6 themes.
- **openclaw-dashboard (tugcantopaloglu)** — production-grade auth
  (TOTP + PBKDF2), Docker admin, config editor.
- **ClawMetry** — observability / flow-graph Grafana-style.

Maquinista's dashboard should land closer to the **mudrii** shape
(simple, server-rendered, tiny footprint, embedded) than the SPA
shape, because (a) it must ship as part of the single Go binary and
(b) it must be mobile-first by design, not retrofit.

### What's different about maquinista's domain

Hermes and openclaw are single-agent-centric. Maquinista is multi-
agent by default (one tmux window per agent, per-agent soul / memory
/ checkpoint / worktree). The dashboard's primary axis is therefore
**agent**, not session — with inbox/outbox/conversations as cross-
cutting feeds.

## Scope

Four phases. Phase 1 delivers a useful read-only dashboard; 2 adds
metrics and scheduled-job visibility; 3 opens the action surface
(reply, rewind, spawn); 4 hardens auth for external exposure.

### Phase 1 — Read-only mobile-first dashboard

**Stack** (intentionally boring, matches maquinista's "one binary,
no npm" vibe):

- Server-rendered **Go html/template** partials.
- **HTMX** (~14 KB min.gz) for partial reloads and SSE, no SPA build.
- **Server-Sent Events** for live updates (single GET `/dash/stream`
  multiplexes all topics; HTTP/2 scales fine; mobile Safari / Chrome
  both support SSE reliably — WebSocket's duplex isn't needed).
- **Plain CSS** with CSS custom properties for theming; no tailwind.
  ~2 KB after gzip for the whole stylesheet.
- **PWA**: `manifest.webmanifest` + tiny service worker for install-
  to-home-screen + offline shell (header + "reconnecting…" state).

All static assets live under `internal/dashboard/static/` and are
embedded via `//go:embed`. Zero filesystem reads at runtime.

**Routes** — all under `/dash`:

```
GET  /dash/                        → root, redirect to /dash/agents
GET  /dash/agents                  → agent list (default landing)
GET  /dash/agents/{id}             → agent detail (tabs below)
GET  /dash/agents/{id}/inbox       → inbox rows
GET  /dash/agents/{id}/outbox      → outbox rows
GET  /dash/agents/{id}/soul        → soul (from agent_souls, Phase 3 of soul plan)
GET  /dash/agents/{id}/memory      → blocks + archival memory (memory plan)
GET  /dash/agents/{id}/checkpoints → git checkpoint timeline (checkpoint plan)
GET  /dash/conversations           → threaded view across all agents
GET  /dash/conversations/{id}
GET  /dash/jobs                    → scheduled_jobs + webhook_handlers tables
GET  /dash/stream                  → SSE multiplexed live feed
```

Each route returns full HTML on first load and returns *just the
partial* (header stripped) when HTMX's `HX-Request: true` header is
present. This is server-side navigation with zero client state.

**Mobile-first layout** — bottom nav (four icons: agents, inbox,
conversations, jobs), sticky top header with agent/page title, swipe-
able tabs on detail pages. Breakpoints: 320 px baseline, 768 px
switches to split pane (list on left, detail on right), 1200 px adds
a third column (live feed rail). No table HTML on mobile — tables
collapse to stacked cards via `@media (max-width: 640px)`.

**Agent list** — the primary view. Each agent is one card:

```
┌─────────────────────────────┐
│ ● maquinista     claude 4.6 │   ← status dot (green/amber/red/gray),
│   last seen 2s ago          │     name, runner+model, last-seen
│   "Fixing webhook dedup…"   │   ← latest outbox excerpt, 80 chars
│   #main  #planner  43¢ today│   ← tags: role, soul template, cost
└─────────────────────────────┘
```

Status dot logic:
- green — `status='running'` AND `last_seen < 30 s ago`
- amber — `status='running'` AND `last_seen ≥ 30 s`
- red   — `status='running'` AND `stop_requested` OR `tmux_window` missing
- gray  — `status IN ('stopped','archived')`

Unread inbox count badge in the top-right of the card if
`COUNT(*) WHERE status IN ('pending','processing')` > 0.

**Conversation view** — chat bubbles, right-aligned for the agent,
left-aligned for the counterpart (user / peer agent / webhook
source). Shows `from_kind` as a small icon above each bubble
(human / agent / scheduled / webhook). Mobile Safari's `::-webkit-
scrollbar-hide` + sticky composer footer, same shape as iMessage.

**Live updates via SSE** — `/dash/stream` subscribes to the existing
Postgres triggers (migration 009 already ships `agent_inbox_new`,
`agent_outbox_new`, `channel_delivery_new`, `agent_stop`) and emits:

```
event: agent.status
data: {"agent_id":"maquinista","status":"running","last_seen":"…"}

event: inbox.new
data: {"agent_id":"maquinista","id":123,"from_kind":"user","excerpt":"…"}

event: outbox.new
data: {"agent_id":"maquinista","id":456,"excerpt":"…"}
```

HTMX swap rules: `hx-swap-oob="beforeend:#agent-<id>-feed"` for
each event type. No page reload, no client-side state management.

**Theming** — three themes (system, dark, light). System follows
`prefers-color-scheme`. Dark palette: Catppuccin Mocha (validated
as popular per reference list). Light palette: GitHub default. One
CSS custom-property block per theme, swap by setting `:root` class.

### Phase 2 — Metrics, cost, jobs, system health

**KPI strip** at the top of the agent list (horizontal scroll on
mobile, static row ≥768 px):

- Active agents (running / total)
- Inbox in-flight (pending + processing)
- Outbox pending
- Tokens today (input + output, sum across agents)
- Cost today (USD, running)
- Cost projected month (linear extrapolation)

Cost sourcing: claude runner already streams `usage.input_tokens` /
`usage.output_tokens` per turn into its stdout. Capture in
`internal/monitor/cost.go` (already partially exists for outbox
scraping), write to a new `agent_turn_costs` table:

```sql
-- migration 020_agent_turn_costs.sql
CREATE TABLE agent_turn_costs (
  id               BIGSERIAL PRIMARY KEY,
  agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  inbox_id         UUID REFERENCES agent_inbox(id),
  model            TEXT NOT NULL,
  input_tokens     INTEGER NOT NULL DEFAULT 0,
  output_tokens    INTEGER NOT NULL DEFAULT 0,
  cache_read       INTEGER NOT NULL DEFAULT 0,
  cache_write      INTEGER NOT NULL DEFAULT 0,
  input_usd_cents  INTEGER NOT NULL DEFAULT 0,   -- computed at insert from a per-model rate table
  output_usd_cents INTEGER NOT NULL DEFAULT 0,
  started_at       TIMESTAMPTZ NOT NULL,
  finished_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX agent_turn_costs_agent_finished_idx
  ON agent_turn_costs (agent_id, finished_at DESC);
```

Rates in a seeded lookup (`model_rates(model, input_per_mtok,
output_per_mtok, cache_read_per_mtok, cache_write_per_mtok,
effective_from)`) so historical costs stay accurate when pricing
changes.

Donut chart for cost-by-model: inline SVG, 16 ms to render, no
charting library.

**Jobs view** — rows from `scheduled_jobs` and `webhook_handlers`
(migration 010 already has both). Columns on desktop, stacked on
mobile: `name`, `schedule or path`, `agent`, `last_run_at`,
`next_run_at`, `enabled`. A row tap opens a detail drawer with the
prompt template + last N inbox rows the job produced.

**System health panel** — a single card on the agent list page:

- Postgres: pool in-use / idle / waiting
- tmux session: attached / detached windows
- Maquinista PID + uptime
- Bot connection: Telegram / Discord (per configured channel)
- Disk used by worktrees (`du -sh .maquinista/worktrees`)

Refreshed every 5 s via SSE `event: system.health`.

**Alerts banner** — inline above the KPI strip when any of:

- Any agent stuck in `status='running'` with `last_seen > 5 min`
- Any inbox row with `attempts >= max_attempts` (dead)
- Today's cost > configured daily cap
- Any webhook handler with > N failures in last hour

Dismissable per-session; persists across dismissal via
`dashboard_alerts(id, kind, subject, created_at, dismissed_at,
operator)` if Phase 4 auth is on.

### Phase 3 — Action surface (write)

Read-only is useful but the operator ends up switching to Telegram
for actions. Close the loop:

**Composer** — sticky bottom bar on the agent detail and conversation
views. Enqueues an `agent_inbox` row with
`from_kind='user', origin_channel='dashboard',
origin_user_id=<operator>, content.text=<body>`. Uses the existing
inbox consumer — zero new plumbing.

**Per-agent actions** — long-press (mobile) or right-click (desktop)
an agent card:

- **Interrupt** — send an inbox row with content marker
  `content.control='interrupt'`; the runner wrapper sees it and
  issues Ctrl+C into the tmux window.
- **Kill** — `UPDATE agents SET stop_requested=TRUE`, relies on
  `multi-agent-registry.md` Phase 1 reconcile skipping the row.
- **Respawn** — clear `tmux_window`, next reconcile spawns fresh.
- **Rewind to checkpoint** — opens a checkpoint picker
  (`checkpoint-rollback.md` Phase 3), confirms, invokes
  `POST /dash/api/agents/{id}/rewind`.
- **Pin memory** — from a memory row, one-tap sets `pinned=TRUE`
  (agent-memory-db.md Phase 3).

**Per-message actions** — tap a single outbox row:

- **Copy link** — URL with `#outbox-<id>` anchor.
- **Reroute** — send the same content to a different agent via
  `@mention` fanout (agent-to-agent-communication.md Phase 1).
- **Void** — mark `status='voided'`, stops any pending
  `channel_deliveries` for it.

**Quick-reply presets** — configured in `agent_settings.roster ->>
'quickReplies'` as an array of `{label, body}`. Rendered as chips
above the composer (`👍 ship it`, `🔁 try again`, `⛔ stop`). One-tap
enqueues the inbox row with the preset body. Immediate mobile win —
no typing on the phone keyboard.

All action endpoints:

```
POST /dash/api/agents/{id}/inbox          → new inbox row (composer)
POST /dash/api/agents/{id}/interrupt
POST /dash/api/agents/{id}/kill
POST /dash/api/agents/{id}/respawn
POST /dash/api/agents/{id}/rewind         {checkpoint_id, mode}
POST /dash/api/outbox/{id}/void
POST /dash/api/outbox/{id}/reroute        {target_agent}
POST /dash/api/memory/{id}/pin            {pinned: bool}
```

CSRF via double-submit cookie (both PWA and desktop-safe); no JSON
body on inert HTMX forms, just form-encoded.

### Phase 4 — Auth, audit, external exposure

Phase 1–3 assume **local-only** binding (`dashboard.listen =
127.0.0.1:8900` default). Phase 4 makes it safe to expose via
Tailscale / WireGuard / Cloudflare-Tunnel.

**Auth tiers** (pick per deployment):

1. **None** — bind to loopback, trust the network layer (default).
2. **Password** — PBKDF2-SHA256 (openclaw tugcantopaloglu pattern),
   stored in `operator_credentials(id, username, pbkdf2_hash,
   salt, iter, created_at)`. Cookie session in
   `dashboard_sessions(id, operator_id, token_hash, ua, ip, created_at,
   expires_at)`.
3. **Password + TOTP** — same + RFC 6238 TOTP, 30 s window,
   `operator_credentials.totp_secret`. QR code served at
   `/dash/setup/totp` once, then locked.
4. **Telegram-gated** — reuse the existing bot: operator messages the
   bot `/dash`, receives a one-tap magic link with a 10-minute token.
   Zero new credentials, perfect for "I'm on my phone and don't
   remember the password". This is the maquinista-native option —
   steal it over the other three.

Default recommendation: **Telegram-gated**, falling back to password
if the bot is down.

**Audit log**:

```sql
CREATE TABLE dashboard_audit (
  id           BIGSERIAL PRIMARY KEY,
  operator_id  TEXT,                     -- NULL for unauthed local
  action       TEXT NOT NULL,             -- 'inbox.post', 'agent.kill', …
  subject      JSONB NOT NULL,            -- {agent_id, outbox_id, …}
  ua           TEXT,
  ip           INET,
  ok           BOOLEAN NOT NULL,
  error        TEXT,
  at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX dashboard_audit_operator_at_idx
  ON dashboard_audit (operator_id, at DESC);
CREATE INDEX dashboard_audit_action_at_idx
  ON dashboard_audit (action, at DESC);
```

Every write endpoint in Phase 3 records one row. Surfaced on the
dashboard itself under `/dash/audit` (operator-scoped view).

**Rate limits** — per-operator + per-IP, sliding-window counters in
a small in-memory ring (or Postgres `dashboard_rate_buckets` if we
need durability). 60 writes / minute default.

## Mobile-specific decisions

- **No virtualization libraries.** Pagination via `?before=<cursor>`
  cursor + HTMX `hx-trigger="revealed"` on the last row.
- **Tap targets ≥ 44 px.** Verified via `min-height: 44px` on every
  interactive element.
- **Haptics.** `navigator.vibrate(10)` on destructive action
  confirmations (kill / void). iOS Safari ignores it gracefully.
- **Input types.** `inputmode="decimal"` for cost-cap fields,
  `enterkeyhint="send"` on composer, `autocomplete="off"` on search.
- **Viewport.** `viewport-fit=cover` for notched phones; respect
  `env(safe-area-inset-*)` on bottom nav and composer.
- **Offline shell.** Service worker precaches `/dash/shell.html` (a
  skeleton with header + reconnecting indicator). Data is always
  fresh from the server when online; offline shows the last cached
  agent list + a "stale" banner.
- **Install prompt.** Custom "Add to home screen" nudge after the
  operator visits three distinct sessions, dismissable for 30 d.
- **Pull-to-refresh.** Native browser behaviour retained; SSE makes
  it rarely needed, but operators expect the gesture.

## Files

New:

- `internal/dashboard/server.go` — handlers, router, SSE pump.
- `internal/dashboard/templates/*.html` — layout + per-page partials.
- `internal/dashboard/static/app.css`
- `internal/dashboard/static/app.js` — tiny (~3 KB): HTMX, SSE reconnect, theme toggle, haptics.
- `internal/dashboard/static/manifest.webmanifest`
- `internal/dashboard/static/sw.js` — service worker (shell cache + offline banner).
- `internal/dashboard/static/icon-{192,512,maskable}.png`
- `internal/dashboard/stream.go` — SSE multiplexer subscribing to
  existing `LISTEN agent_inbox_new` / `agent_outbox_new` /
  `channel_delivery_new` / `agent_stop` channels.
- `internal/dashboard/auth.go` — Phase 4 (none / password / TOTP /
  Telegram-magic-link).
- `internal/dashboard/audit.go` — Phase 4.
- `internal/db/migrations/020_agent_turn_costs.sql` (Phase 2)
- `internal/db/migrations/021_model_rates.sql` (Phase 2)
- `internal/db/migrations/022_dashboard_auth.sql` (Phase 4)
- `cmd/maquinista/cmd_dashboard.go` — optional standalone binary
  subcommand (`maquinista dashboard --listen :8900`) that shares the
  same DB pool as the main process.

Modified:

- `cmd/maquinista/cmd_start.go` — optionally start dashboard goroutine
  alongside the bot (gated on `cfg.Dashboard.Enabled`).
- `internal/config/config.go` — `Dashboard` section (listen addr,
  auth mode, theme default, telegram-magic-link TTL).
- `internal/bot/handlers.go` — Phase 4 Telegram `/dash` command
  returning a magic-link URL.

## Verification per phase

- **Phase 1** — open `http://127.0.0.1:8900/dash/agents` on phone,
  see agent cards. Send a Telegram message to the agent; within 1 s
  the outbox excerpt on the card updates without a refresh. Switch
  to the conversation view; new message appears as a bubble.
  Lighthouse mobile score ≥ 90 (perf + accessibility + PWA).
- **Phase 2** — run the agent for 20 turns; `agent_turn_costs` has
  20 rows; the KPI strip shows a non-zero "cost today" that matches
  `SELECT SUM(input_usd_cents + output_usd_cents)/100.0 FROM
  agent_turn_costs WHERE finished_at::date = CURRENT_DATE`.
  Toggle a scheduled job off from `/dash/jobs` → `enabled=FALSE`.
- **Phase 3** — from the phone's lock screen, open the PWA; long-
  press the agent card; choose "Interrupt". `tmux capture-pane -t
  maquinista:<agent>` shows Ctrl+C. Type "ship it" in the composer
  → new inbox row, agent picks it up within 1 s, replies.
- **Phase 4** — enable `auth=telegram-magic-link`, expose via
  Tailscale, message the bot `/dash` → receive a URL; tap, authed
  for 10 min. Any write action logs a `dashboard_audit` row. Second
  device without auth gets redirected to `/dash/auth`.

## Interaction with other plans

- **`multi-agent-registry.md`** — the agent list reads
  `agents + agent_settings` with the same reconcile predicate.
  `maquinista agent archive` from the CLI is mirrored by a dashboard
  action once Phase 3 lands.
- **`agent-soul-db-state.md`** — soul tab renders `agent_souls` row;
  `soul edit` flows through the web in Phase 3 (textarea per
  section, submit re-renders). Injection scanner runs server-side.
- **`agent-memory-db.md`** — memory tab has two sub-tabs: blocks
  (editable textarea per block, enforces `char_limit`) and
  archival (search + pin). Phase 0 of the memory plan is a
  prerequisite for the blocks UI.
- **`agent-to-agent-communication.md`** — conversation view naturally
  handles `from_kind='agent'` rows with an "agent" icon; `kind='a2a'`
  threads show both participants. Broadcast (`@everyone`) threads
  render with a participant list collapsible.
- **`checkpoint-rollback.md`** — checkpoint tab is a vertical
  timeline; rewind is a confirm-then-POST flow. Conflict prompt from
  the plan renders as a modal.
- **`json-state-migration.md`** — irrelevant once that lands; the
  dashboard just reads Postgres.

## What we deliberately skip (vs reference dashboards)

- **Flow graph / decision transcripts** (ClawMetry). Nice-to-have,
  big build. Conversation view + checkpoint timeline together
  already give 80 % of the observability without a canvas renderer.
- **Docker / container admin** (tugcantopaloglu). Out of scope —
  maquinista is a single binary.
- **AI chat over the dashboard** (mudrii). Talking to the dashboard
  itself is redundant when every agent already accepts messages from
  the composer.
- **UFW / fail2ban panels**. Host-level security is the host's job.
- **6 themes**. Ship 3 (system, dark, light). Theming is CSS custom
  props; more can be added by operators in userspace.
- **Keyboard palette with 13 tabs**. Mobile-first means touch-first.
  Desktop gets `?` to open a shortcut cheatsheet in Phase 2.

## Open questions

1. **Go templates vs Templ vs htmx-only.** Raw `html/template` works
   but escapes painfully. [`a-h/templ`](https://templ.guide) would
   be nicer but adds a code-generation step. Start raw, migrate if
   template size passes ~20 partials.
2. **SSE vs WebSocket.** SSE is simpler and mobile-friendly but
   one-way. If Phase 3 write-path needs server-push responses (e.g.
   "rewind in progress… done"), WebSocket becomes tempting. Defer —
   use polling on the specific long-running call until pain shows.
3. **Embedding vs separate binary.** Ship inside the main binary (a
   goroutine) by default so operators get it for free; `maquinista
   dashboard --listen …` subcommand is available for split
   deployments (dashboard on a public host, main on a private one).
   Same binary, shared `go:embed` assets.
4. **Charting.** Inline SVG covers donut + sparkline. If we need
   anything richer (cumulative cost line, response-time histogram),
   `uplot` is ~40 KB and works on mobile. Decide when the need hits.
5. **Operator identity.** Phase 4 treats operators as a flat list.
   Multi-operator / role separation (`viewer` vs `admin`) is
   deferred; not needed for the single-operator case.
6. **Export.** Should the dashboard offer JSON/CSV export of
   conversations, costs, audit? Probably yes for audit + cost, no
   for conversations (privacy). Ship CSV in Phase 2.
