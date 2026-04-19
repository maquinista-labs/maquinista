# Dashboard

## Architecture

The dashboard is a Next.js app embedded inside the maquinista binary and
served by a Go HTTP server. It connects to the same Postgres DB as the
bot — there is no separate backend API layer beyond Next.js route handlers.

```
operator browser
      │  HTTP
      ▼
Go HTTP server (maquinista dashboard start)
      │  reverse proxy to Next.js standalone server
      ▼
Next.js (node, standalone bundle extracted from binary)
      │  pg (node-postgres) direct connection
      ▼
Postgres
```

The Go server handles:
- Static asset serving / reverse proxy to Node.
- Health check endpoint.
- Authentication middleware (none / password / Telegram — phase 6).

Next.js route handlers (`src/app/api/`) connect to Postgres directly via
`node-postgres`. There is no Go API for the dashboard to call — it talks
to the DB itself.

## Embedding

The built Next.js standalone bundle is compressed into
`internal/dashboard/standalone.tgz` and embedded in the Go binary via
`go:embed`. On `dashboard start`, it is extracted to a temp directory
(or operator-specified `--embed-dir`) and served from there.

For development, `--dashboard-no-embed <path>` skips extraction and
points directly at a pre-built `.next/standalone` directory.

## Key API routes

| Method | Route | What it does |
|--------|-------|--------------|
| GET | `/api/agents` | List agents with status |
| POST | `/api/agents` | Spawn new agent (calls `spawnAgentFromDashboard`) |
| GET | `/api/agents/:id` | Single agent detail |
| POST | `/api/agents/:id/inbox` | Send message to agent |
| GET | `/api/agents/:id/inbox` | List inbox rows (paginated) |
| GET | `/api/agents/:id/outbox` | List outbox rows (paginated) |
| GET | `/api/agents/:id/timeline` | Merged inbox+outbox conversation view |
| POST | `/api/agents/:id/interrupt` | Send interrupt control message |
| POST | `/api/agents/:id/kill` | Set `stop_requested=TRUE` |
| POST | `/api/agents/:id/respawn` | Clear `tmux_window`, trigger reconcile |
| POST | `/api/agents/:id/rename` | Set `agents.handle` |
| POST | `/api/agents/:id/archive` | Set `status='archived'` |
| DELETE | `/api/agents/:id/delete` | Hard delete agent row |
| GET/POST | `/api/agents/:id/workspaces` | List / create workspaces |

## Tunnel

`/dashboard` Telegram command starts a Cloudflare Quick Tunnel to the
local dashboard. This makes the dashboard reachable from mobile without
port forwarding. The tunnel has a configurable TTL (default 15 min).
`tunnel.Manager` (`internal/tunnel/`) supervises the `cloudflared` process.

## Real-time updates

Currently polling-based (dashboard UI polls `/api/agents/:id/outbox`
on an interval). SSE infrastructure (`internal/dashboard/web/src/lib/sse.ts`)
exists for push-based updates.

## Authentication

`auth_mode=none` (default) — no auth, bind to loopback only.
`auth_mode=password` — HTTP Basic via `MAQUINISTA_DASHBOARD_PASSWORD`.
`auth_mode=telegram` — Telegram Login Widget (plans/active/dashboard-telegram-auth.md).

## TODO

- [ ] SSE / push updates instead of polling
- [ ] Telegram auth implementation (plans/active/dashboard-telegram-auth.md)
- [ ] Cost tracking panel (plans/active/dashboard-cost-sse.md)
- [ ] Rewind / action replay (plans/active/dashboard-rewind-actions.md)
- [ ] Mobile-optimized layout
