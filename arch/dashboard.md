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

The dashboard uses SSE (`/api/stream`) backed by Postgres `LISTEN/NOTIFY`.
A single `EventSource` per browser tab listens to several pg channels and
re-dispatches them as DOM `CustomEvent`s so multiple React components can
subscribe without opening extra connections.

### Two distinct real-time patterns

#### 1. Persistent (outbox-backed)

Used for content that has value after the fact and must survive a page
reload or reconnect.

**Flow:**

```
Go sink writes row → agent_outbox (DB)
    → pg_notify("agent_outbox_new")
    → SSE → React Query invalidate
    → frontend re-fetches from DB
```

Data is durable in `agent_outbox`. The pg_notify is just a wake-up call;
the real payload is always fetched from the DB. Examples:

- Agent text responses
- Thinking blocks
- Completed tool call summaries (tool_use + tool_result paired)

#### 2. Ephemeral (direct pg_notify only)

Used for transient state that has no value once it's gone. **Nothing is
written to any DB table.**

**Flow:**

```
Go emitter calls pg_notify directly
    → SSE → DOM CustomEvent → React state
    → gone on disconnect / page reload
```

No DB read on the receive side. Examples:

- `tool_event` — live tool call progress (`tool_use` / `tool_result`)
  while a tool is actively running. After the session the paired summary
  is in `agent_outbox` via the persistent pattern.
- `agent_status` — terminal spinner text polled every second from the
  tmux pane (e.g. "Running Bash(git status)"). Shown as a pulsing
  "thinking" indicator; meaningless once the agent is idle.

**Rule of thumb:** if a user reloading the page should still see it, use
the outbox pattern. If it only matters right now, use direct pg_notify.

## Authentication

`auth_mode=none` (default) — no auth, bind to loopback only.
`auth_mode=password` — HTTP Basic via `MAQUINISTA_DASHBOARD_PASSWORD`.
`auth_mode=telegram` — Telegram Login Widget (plans/active/dashboard-telegram-auth.md).

## TODO

- [x] SSE / push updates instead of polling
- [ ] Telegram auth implementation (plans/active/dashboard-telegram-auth.md)
- [ ] Cost tracking panel (plans/active/dashboard-cost-sse.md)
- [ ] Rewind / action replay (plans/active/dashboard-rewind-actions.md)
- [ ] Mobile-optimized layout
