# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
make build          # Go binary + Next.js dashboard (incremental)
make build-go       # Go binary only (skips Next.js — use for quick iteration)
make test           # go test ./...
make vet            # go vet ./...
make up / make down # Start/stop PostgreSQL via Docker Compose

# Run a single Go test
go test ./internal/relay/... -run TestProcessOne -v

# Dashboard (Next.js)
make dashboard-web-dev    # HMR dev server
make dashboard-web-test   # Vitest unit tests
make dashboard-e2e        # Playwright end-to-end (requires make dashboard-e2e-install first)
make dashboard-test       # Go-side dashboard tests (supervisor, config, CLI)
```

`SKIP_DASHBOARD=1 make build` skips the Next.js pipeline and uses the committed `standalone.tgz` tarball.

## First-run

```bash
make up               # start Postgres
make build
./maquinista migrate  # apply schema
./maquinista start    # start full stack
```

`./maquinista start` detaches both the orchestrator (Telegram bot) and the dashboard (`http://127.0.0.1:8900`). Stop with `./maquinista stop`. On subsequent runs only `./maquinista start` is needed.

## Architecture

> **Keep `arch/` in sync.** Whenever you make a structural change — new tables, new daemon goroutines, new data flows, changes to routing — update the relevant file(s) in `arch/`. Each file covers one concern and is meant to stay accurate as the system evolves. See `arch/README.md` for the index.

### Big picture

Maquinista runs three coordinated daemon processes against a single PostgreSQL instance:

- **Orchestrator** (`maquinista orchestrator start`) — Telegram bot + monitor + mailbox + relay + dispatcher + sidecar manager + scheduler
- **Dashboard** (`maquinista dashboard start`) — embedded Next.js app, served by a Go supervisor
- **Orchestrator engine** (optional `--orchestrate` flag) — autonomous task loop

All persistent state lives in Postgres. Daemons communicate via `pg_notify` channels with a 10 s poll fallback. No shared in-memory state between processes.

### Message flow (full path)

```
User (Telegram or dashboard)
        │
        ▼
   agent_inbox  ──[NOTIFY agent_inbox_new]──▶  sidecar → tmux PTY → agent
                                                                         │
                                                                   agent_outbox
                                                                    [NOTIFY agent_outbox_new]
                                                                         │
                                                                      relay
                                                               ┌────────┴────────┐
                                                     channel_deliveries     inbox_echoes
                                                          [telegram]     [inbox mirror]
                                                               │               │
                                                          dispatcher      inboxecho.RunDispatch
                                                               │               │
                                                         Telegram API    Telegram API
                                                                    (👤 sender: text)
```

Dashboard reads `agent_outbox` directly (no relay needed). Telegram reads via `channel_deliveries` → dispatcher.

`inbox_echoes` mirrors non-Telegram user messages (dashboard, future Slack/Discord) back to the agent's Telegram topic. Adding a new external channel means: a new leg in `inboxecho.fanout.go` + a new dispatcher — no structural changes.

### Key data paths

- **Routing** — four-tier ladder: `[@mention]` → owner binding lookup → tier-3 spawn (`t-<chatID>-<threadID>`) → picker. See `arch/routing.md`.
- **Agent identity** — souls stored in `agent_souls`, rendered via `maquinista soul render <id>`, injected at spawn via shell substitution. See `arch/soul-and-identity.md`.
- **Workspaces** — `scope=shared|agent|task`; agent/task scopes get git worktrees. See `arch/workspaces.md`.
- **Migrations** — `internal/db/migrations/NNN_*.sql`, applied by `./maquinista migrate`. Add a new file; never edit existing ones.

### Package map (non-obvious relationships)

| Package | Role |
|---------|------|
| `internal/relay` | `agent_outbox` → `channel_deliveries` fanout + A2A mention parsing |
| `internal/dispatcher` | `channel_deliveries` → Telegram Bot API |
| `internal/inboxecho` | `agent_inbox` (non-Telegram) → `inbox_echoes` → Telegram (echo mirror) |
| `internal/sidecar` | Per-agent goroutine: claims inbox rows, drives to tmux PTY |
| `internal/monitor` | Tails agent transcript JSONL, writes `agent_outbox` rows |
| `internal/routing` | Four-tier routing ladder, `writeOwnerBinding` |
| `internal/bot/topic_provisioner.go` | Creates Telegram forum topics for dashboard agents |
| `cmd/maquinista/spawn_topic_agent.go` | Tier-3 inline agent spawn + pre-bind race guard |
| `cmd/maquinista/reconcile_agents.go` | Restores tmux panes on daemon restart |

### NOTIFY channels

| Channel | Fired by | Consumed by |
|---------|----------|-------------|
| `agent_inbox_new` | DB trigger on `agent_inbox` INSERT | sidecar, `inboxecho` fanout |
| `agent_outbox_new` | DB trigger on `agent_outbox` INSERT | relay |
| `channel_delivery_new` | DB trigger on `channel_deliveries` INSERT | dispatcher |
| `inbox_echo_new` | DB trigger on `inbox_echoes` INSERT | `inboxecho` dispatch |
| `agent_status` | `status.go` explicit `pg_notify` | dashboard SSE |
| `task_events` | DB trigger | orchestrator engine |

### Configuration

All config via `.env` (loaded by `internal/config/Load()`). Key vars: `TELEGRAM_BOT_TOKEN`, `ALLOWED_USERS`, `ALLOWED_GROUPS`, `DATABASE_URL`, `MAQUINISTA_DIR`, `TMUX_SESSION_NAME`.
