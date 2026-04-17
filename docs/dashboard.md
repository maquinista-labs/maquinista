# Maquinista Dashboard

A mobile-first, live-updated web UI for the maquinista fleet. Runs
as a Next.js child process supervised by the `maquinista` binary.

Design doc: [`plans/active/dashboard.md`](../plans/active/dashboard.md).
Deferred follow-ups:
[`plans/active/dashboard-rewind-actions.md`](../plans/active/dashboard-rewind-actions.md),
[`plans/active/dashboard-telegram-auth.md`](../plans/active/dashboard-telegram-auth.md),
[`plans/active/dashboard-cost-sse.md`](../plans/active/dashboard-cost-sse.md).

## Prerequisites

- **Go 1.25+** (for building `maquinista`)
- **Node 22+** on `$PATH` (operator's Claude Code install already
  provides this; override with `MAQUINISTA_DASHBOARD_NODE_BIN` if
  it lives outside PATH)
- **Docker** (running Postgres — the daemon and the dashboard share
  the same `DATABASE_URL`)
- **Playwright browsers** are only needed for E2E tests, not for
  running the dashboard

## First-time setup

```sh
# 1. Boot Postgres (same container the main daemon uses).
docker compose -f docker/docker-compose.yml up -d

# 2. Apply migrations.
make build
./maquinista migrate

# 3. Install the dashboard's npm deps + build the standalone bundle
#    the Go binary embeds (~50 MiB).
make dashboard-web-install
make dashboard-web-package    # npm build + tar → internal/dashboard/standalone.tgz

# 4. Rebuild the Go binary so //go:embed picks up the new tarball.
make build
```

After step 4 the binary contains the Next.js bundle. The Go
`dashboard start` command extracts it into
`~/.maquinista/dashboard/<sha>/` on first run (cached across
subsequent starts) and spawns `node server.js` against it.

If you skip step 3, `dashboard start` still works — it falls back
to a Node healthcheck stub and logs a loud "run `make
dashboard-web-package` for the real Next server" hint on stderr.
Useful for testing the CLI surface without building the frontend.

## Running the dashboard

```sh
# Default: listen on 127.0.0.1:8900.
./maquinista dashboard start

# Bind elsewhere.
./maquinista dashboard start --listen 0.0.0.0:9001

# Against a pre-built bundle in the working tree (dev/CI shortcut —
# skips the extract step).
./maquinista dashboard start --no-embed internal/dashboard/web/.next/standalone

# In another terminal:
./maquinista dashboard status        # running (PID N)
./maquinista dashboard logs --follow # tails ~/.maquinista/logs/dashboard.log
./maquinista dashboard stop          # SIGTERM → 10 s grace → SIGKILL
```

Open `http://127.0.0.1:8900/` and you'll land on `/agents`. With
no agents registered, the list shows an empty banner; start the
main daemon (`./maquinista start`) and agent cards appear within
a second via SSE.

## Dev loop (iterating on the frontend)

```sh
# Terminal 1 — the main daemon (optional; provides real agent data).
./maquinista start

# Terminal 2 — Vite-style hot-reload on the Next.js side.
cd internal/dashboard/web
DATABASE_URL=postgres://maquinista:maquinista@localhost:5434/maquinistadb?sslmode=disable npm run dev
```

`npm run dev` serves on `127.0.0.1:3000` with full HMR and the same
Postgres. Once you're happy, re-run `make dashboard-web-package &&
make build` to refresh the embedded release bundle.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `MAQUINISTA_DASHBOARD_LISTEN` | `127.0.0.1:8900` | Listen address |
| `MAQUINISTA_DASHBOARD_AUTH` | `none` | Auth mode (`none` / `password` / `telegram`) |
| `MAQUINISTA_DASHBOARD_THEME` | `system` | Default theme (`system` / `dark` / `light`) |
| `MAQUINISTA_DASHBOARD_NODE_BIN` | `node` | Node executable path (override for nvm/asdf shims) |
| `MAQUINISTA_DASHBOARD_DB_MAX` | `5` | Max pg pool connections on the Next side |
| `DATABASE_URL` | — | Shared with the main daemon |

## Auth modes

### `none` (default)

No auth. Fine for loopback-only deployments where the network layer
(e.g. Tailscale) provides the trust boundary. Bind to `127.0.0.1`
and don't expose externally.

### `password`

```sh
export MAQUINISTA_DASHBOARD_AUTH=password
./maquinista dashboard stop   # bounce so middleware re-reads env
./maquinista dashboard start
```

Seed an operator the first time (one-shot psql; later the
maquinista CLI grows an `operator add` subcommand):

```sh
# PBKDF2 hashing inline — a tiny Node one-liner is simpler than
# exposing CLI surface for Phase 6.
node -e '
  const crypto = require("crypto");
  const username = process.argv[1];
  const password = process.argv[2];
  const salt = crypto.randomBytes(16).toString("hex");
  const iter = 600000;
  const hash = crypto.pbkdf2Sync(password, salt, iter, 32, "sha256").toString("hex");
  console.log(`INSERT INTO operator_credentials(username, pbkdf2_hash, salt, iter) VALUES ('${username}', '${hash}', '${salt}', ${iter});`);
' otavio hunter42 | psql "$DATABASE_URL"
```

Visit `http://127.0.0.1:8900/` → redirected to `/auth` → sign in.
Account locks for 15 min after 5 failed attempts. Logout via
`POST /api/auth/logout` (UI button lands in a follow-up commit).

### `telegram`

Deferred. `/auth` currently renders a stub banner. See
[`plans/active/dashboard-telegram-auth.md`](../plans/active/dashboard-telegram-auth.md)
for the magic-link-via-bot implementation plan.

## Running tests

```sh
# Go side: supervisor + CLI + Postgres integration (requires Docker).
make dashboard-test

# Extra: real-Next-server integration test (builds the bundle if stale).
MAQUINISTA_DASHBOARD_NEXT_E2E=1 go test -run RealNext ./cmd/maquinista/ -v

# Next side: Vitest (pg pool, SQL helpers, SSE encoder, auth, rate limiter).
cd internal/dashboard/web && npm test

# Playwright E2E. First time on a host:
make dashboard-e2e-install    # Chromium + WebKit + OS deps (sudo)
make dashboard-e2e            # boots Postgres, dashboard, runs 72 specs
```

The test counts at the moment of this writing:
- 42 Go tests (under `-race`)
- 40 Vitest tests
- 72 Playwright tests (36 specs × chromium-desktop + mobile-safari)

Integration tests skip cleanly when Docker or Node is missing.

## Troubleshooting

- **`dashboard: already running (PID N)`** — another `dashboard
  start` is live. Run `maquinista dashboard stop` first, then retry.
- **`spawn failed: node: executable not found`** — install Node 22
  (`nvm install 22 && nvm use 22`) or point
  `MAQUINISTA_DASHBOARD_NODE_BIN` at the binary.
- **Port already bound** — pick another via `--listen 127.0.0.1:PORT`.
- **Dashboard crash-loops** — check `maquinista dashboard logs`; the
  supervisor gives up after 5 crashes in 60 s and exits with the
  budget error. The log file is appended across runs.
- **"embedded bundle is the NOT_BUILT placeholder"** on stderr —
  you forgot `make dashboard-web-package`. The fallback Node stub
  still serves `/api/healthz` so the lifecycle integration test
  stays green.
- **Playwright specs all skipped** — the Postgres container failed
  to come up or the orphan from a previous run is wedged. Clean
  up with `docker rm -f $(docker ps -aq --filter name=maquinista-e2e)`
  and retry.
- **`401 unauthenticated` from every API route** — you set
  `MAQUINISTA_DASHBOARD_AUTH=password` but have no cookie yet. Sign
  in at `/auth` first.
