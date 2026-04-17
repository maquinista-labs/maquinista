# Maquinista Dashboard

A mobile-first, live-updated web UI for the maquinista fleet. Runs
as a Next.js child process supervised by the `maquinista` binary. See
[`plans/active/dashboard.md`](../plans/active/dashboard.md) for the
full design doc.

## Prerequisites

- **Go 1.25+** (for building `maquinista`)
- **Node 22+** on `$PATH` (the operator's Claude Code install already
  provides this)
- **Docker** (only for the Postgres fixture in integration/E2E tests)

## CLI

```sh
maquinista dashboard start   [--listen host:port]
maquinista dashboard stop
maquinista dashboard status
maquinista dashboard logs    [--follow]
```

### `start`

Spawns the dashboard's Node child, writes `~/.maquinista/dashboard.pid`,
appends the child's stdout/stderr to `~/.maquinista/logs/dashboard.log`,
supervises and restarts on crash (bounded at 5 restarts per 60 s).

Listen address resolution: `--listen` flag > `MAQUINISTA_DASHBOARD_LISTEN`
env > default `127.0.0.1:8900`.

Exits with the restart-budget error if the child crash-loops past the
budget; otherwise blocks on SIGTERM/SIGINT from the operator.

Refuses to start if the PID file exists and the recorded process is
alive. A stale PID file (from an ungraceful shutdown) is cleaned up
automatically.

### `stop`

Reads the PID, sends SIGTERM, waits up to 10 s for clean exit, then
SIGKILLs. Tolerates missing/stale PID files with a clear message.

### `status`

Prints `dashboard: running (PID N)` or `dashboard: not running`. Exits
non-zero when not running so shell scripts can branch on it.

### `logs [--follow]`

Dumps `~/.maquinista/logs/dashboard.log`. With `--follow`, tails the
file (polling at 100 ms) until Ctrl+C. If the file doesn't exist and
`--follow` is set, waits for it to appear.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `MAQUINISTA_DASHBOARD_LISTEN` | `127.0.0.1:8900` | Listen address |
| `MAQUINISTA_DASHBOARD_AUTH` | `none` | Auth mode (`none`/`password`/`telegram`) — Phase 6 |
| `MAQUINISTA_DASHBOARD_THEME` | `system` | Default theme (`system`/`dark`/`light`) |
| `MAQUINISTA_DASHBOARD_NODE_BIN` | `node` | Node executable path (override for nvm/asdf shims) |
| `DATABASE_URL` | — | Shared with the main daemon |

## Architecture (current state)

Phase 0 is complete: a Go CLI + `dashboard.Supervisor` that spawns a
Node healthcheck stub and supervises it. Phase 1 replaces the stub
with the real Next.js scaffold and adds the first Playwright E2E.

```
maquinista dashboard start
   │
   ├─ Go CLI (cmd/maquinista/cmd_dashboard.go)
   │    writes PID file, signals SIGTERM on exit
   │
   └─ internal/dashboard.Supervisor
        extracts embedded Next bundle (Phase 1.5)
        spawns `node server.js` (Phase 1.6; stub in Phase 0.3)
        appends child stdio to dashboard.log
        restarts on crash (bounded)
```

See `plans/active/dashboard.md` for the full phase plan.

## Running tests

```sh
# Fast Go-side suite (supervisor + CLI + integration).
make dashboard-test

# Playwright E2E (Phase 1 onwards):
make dashboard-e2e
```

The integration tests skip cleanly when Docker or Node is missing.

## Troubleshooting

- **`dashboard: already running (PID N)`** — another `dashboard start`
  is live. Run `maquinista dashboard stop` first.
- **`spawn failed: node: executable not found`** — install Node 22
  (`nvm install 22 && nvm use 22`) or point
  `MAQUINISTA_DASHBOARD_NODE_BIN` at the binary.
- **Port already bound** — pick another via `--listen 127.0.0.1:PORT`.
- **Dashboard crash-loops** — check `maquinista dashboard logs`; the
  supervisor gives up after 5 crashes in 60 s and exits with the
  budget error.
