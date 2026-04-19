# Dashboard Login + Persistent Boot Tunnel

**Goal:** Two independent deliverables shipped together.

1. **Login page** — password auth for the dashboard, users created only via
   Telegram bot command. The Next.js login page already exists; the missing
   piece is the Go-side Telegram command that writes to `operator_credentials`.

2. **Persistent cloudflared tunnel** — starts automatically when
   `maquinista start` runs, URL is logged to stdout and sent as a Telegram
   message with an [Open] button. The tunnel stays alive for the entire
   process lifetime (no TTL). `/dashboard` returns the already-running URL
   (existing `IsRunning()` path already handles this).

---

## What already exists (do not rebuild)

| Asset | Path |
|---|---|
| Login page + form | `web/src/app/auth/page.tsx` + `web/src/components/dash/login-form.tsx` |
| Login / logout routes | `web/src/app/api/auth/login/route.ts`, `.../logout/route.ts` |
| Edge middleware (auth gate) | `web/src/middleware.ts` |
| Auth lib (PBKDF2, sessions) | `web/src/lib/auth.ts` |
| DB schema | `internal/db/migrations/026_dashboard_auth.sql` |
| Tunnel manager | `internal/tunnel/manager.go` |
| `/dashboard` + `/dashboard_stop` commands | `internal/bot/dashboard_commands.go` |
| Tunnel wiring in bot | `internal/bot/bot.go` (field `tunnel *tunnel.Manager`) |

---

## Scope

### Part A — Telegram user-management command

New command: `/dashboard_user add <username> <password>`

The bot writes directly to Postgres using Go's `golang.org/x/crypto/pbkdf2`
package. The hash format must match what `auth.ts` produces:

```
pbkdf2:sha256:<iter>:<salt_hex>:<key_hex>
```

Where:
- `iter = 600_000`
- `salt` = 16 random bytes, hex-encoded
- `key` = `pbkdf2.Key([]byte(password), salt_bytes, iter, 32, sha256.New)`, hex-encoded

#### Files to create

**`internal/bot/dashboard_user_commands.go`**

```go
// handleDashboardUser routes /dashboard_user subcommands.
// Usage: /dashboard_user add <username> <password>
func (b *Bot) handleDashboardUser(msg *tgbotapi.Message) { ... }

// createOperatorCredential inserts a row into operator_credentials.
// Returns the new operator UUID.
func createOperatorCredential(ctx context.Context, pool *pgxpool.Pool, username, password string) (string, error) { ... }

// hashPasswordGo produces the same format as auth.ts hashPassword().
func hashPasswordGo(password string) (hash, salt string, err error) { ... }
```

#### Files to modify

**`internal/bot/commands.go`** — add case:

```go
case "dashboard_user":
    b.handleDashboardUser(msg)
```

**`internal/bot/bot.go`** — add to `registerCommands()`:

```go
tgbotapi.BotCommand{Command: "dashboard_user", Description: "Manage dashboard users: add <username> <password>"},
```

**`go.mod` / `go.sum`** — `golang.org/x/crypto` is almost certainly already
a transitive dependency; confirm with `go list -m golang.org/x/crypto`. If
absent, `go get golang.org/x/crypto`.

---

### Part B — Persistent boot tunnel

#### Config change

Add one field to `DashboardConfig` in `internal/config/config.go`:

```go
// AutoTunnel starts a cloudflared Quick Tunnel at orchestrator boot with
// no TTL. Set MAQUINISTA_DASHBOARD_AUTO_TUNNEL=1 to enable.
AutoTunnel bool
```

Load it in `loadDashboardConfig()`:

```go
cfg.AutoTunnel = parseBoolEnv(os.Getenv("MAQUINISTA_DASHBOARD_AUTO_TUNNEL"))
```

#### Boot sequence change

In `internal/bot/bot.go`, expose a method:

```go
// StartPersistentTunnel starts a no-TTL cloudflared tunnel to the dashboard
// and logs the URL. Idempotent if already running. Called by the supervisor
// after bot init when AutoTunnel is enabled.
func (b *Bot) StartPersistentTunnel(ctx context.Context) (string, error) { ... }
```

Implementation:
1. Call `b.ensureDashboardRunning()` — same helper used by `/dashboard`
2. Call `b.tunnel.Start(ctx, listenAddr, 0)` — `dur=0` means no expiry
3. `log.Printf("dashboard: tunnel ready → %s", url)`
4. Send Telegram message with [Open] button to every configured `AllowedUser`
   (first user in the list if there is no dedicated notify chat) — reuse
   `replyWithURLButton` or send directly via `b.api.Send`

In `cmd/maquinista/cmd_start.go`, inside `runOrchestratorSupervised()`, after
`bot.New(cfg)` returns and before `b.Run(ctx)`:

```go
if cfg.Dashboard.AutoTunnel {
    go func() {
        // Small delay so the dashboard process has time to bind its port.
        time.Sleep(3 * time.Second)
        if url, err := b.StartPersistentTunnel(ctx); err != nil {
            log.Printf("auto-tunnel: %v", err)
        } else {
            log.Printf("auto-tunnel: %s", url)
        }
    }()
}
```

#### Auth guard

When `AutoTunnel=true` and `AuthMode=none`, the dashboard is publicly
accessible with no login. Emit a loud warning at boot:

```
WARNING: MAQUINISTA_DASHBOARD_AUTO_TUNNEL=1 but MAQUINISTA_DASHBOARD_AUTH=none.
The dashboard is publicly accessible without authentication.
Set MAQUINISTA_DASHBOARD_AUTH=password to require login.
```

Do not force-set `AuthMode` automatically — respect operator intent, just warn.

#### `/dashboard` command is unchanged

`handleDashboard()` already calls `b.tunnel.IsRunning()` first and returns the
existing URL + remaining time. With `dur=0` it prints "Tunnel running (no
expiry)." — correct behaviour with no code change.

---

## Execution order

```
Step 1  Confirm golang.org/x/crypto is already in go.mod
Step 2  Write internal/bot/dashboard_user_commands.go
Step 3  Wire command in commands.go + registerCommands()
Step 4  Add AutoTunnel to config + loadDashboardConfig()
Step 5  Add Bot.StartPersistentTunnel() method
Step 6  Wire auto-start in runOrchestratorSupervised()
Step 7  Build + smoke-test:
          maquinista start  (with AUTO_TUNNEL=1)
          /dashboard_user add otavio supersecret
          open tunnel URL → should redirect to /auth
          login with otavio / supersecret
          verify /dashboard bot command shows the existing URL
```

---

## Operator setup (README / env vars)

```env
# .env additions
MAQUINISTA_DASHBOARD_AUTH=password          # required for login gate
MAQUINISTA_DASHBOARD_AUTO_TUNNEL=1          # start tunnel at boot
MAQUINISTA_DASHBOARD_LISTEN=127.0.0.1:8900  # default, usually no need to set
```

Create first user:
```
/dashboard_user add myuser mypassword
```

---

## Out of scope for this plan

- TOTP / 2FA
- `/dashboard_user list` / `/dashboard_user remove`
- Telegram magic-link auth
- Named cloudflared tunnels (persistent subdomain) — Quick Tunnels only
- Operator management UI in the dashboard itself
