# Dashboard Telegram magic-link auth

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres
> is the system of record**.

## Context

`plans/active/dashboard.md` Phase 6 shipped the "none" and
"password" auth modes. A third mode — `telegram` — was scoped out
because it requires a round-trip through the existing Telegram bot
and a new `agent_magic_links` table.

The value is real: the operator is usually on a phone, usually
logged into the bot already, and typically doesn't remember the
dashboard password. `/auth` currently renders a stub banner when
`MAQUINISTA_DASHBOARD_AUTH=telegram`. This plan wires the magic-
link flow end-to-end.

## Flow

```
Operator visits /auth (mode=telegram)
         │
         ▼
 /auth shows "Send me a link" button + short instructions
         │  tap
         ▼
 POST /api/auth/telegram/request
   → insert magic_links row {token_hash, expires_at=now+10m,
                              operator_username}
   → send Telegram DM via existing bot:
     "Your dashboard link: https://…/auth/magic?t=<token>"
         │
         ▼
 Operator taps link in Telegram (lands back on dashboard host)
         │
         ▼
 GET /auth/magic?t=<token>
   → look up magic_links by sha256(token),
     check expires_at > NOW() AND consumed_at IS NULL
   → create dashboard_sessions row, set cookie, delete magic link
   → redirect to /agents
```

## Scope

### Commit T.1 — Schema

Migration `028_dashboard_magic_links.sql`:

```sql
CREATE TABLE dashboard_magic_links (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id   UUID        NOT NULL REFERENCES operator_credentials(id) ON DELETE CASCADE,
    token_hash    TEXT        NOT NULL UNIQUE,
    telegram_user TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_at   TIMESTAMPTZ
);
CREATE INDEX idx_magic_links_expires
    ON dashboard_magic_links (expires_at)
    WHERE consumed_at IS NULL;
```

Also allow an operator row to not require a PBKDF2 hash (nullable
columns) so operators whose only identity is "my Telegram user_id"
can exist:

```sql
ALTER TABLE operator_credentials
    ALTER COLUMN pbkdf2_hash DROP NOT NULL,
    ALTER COLUMN salt DROP NOT NULL,
    ADD COLUMN telegram_user_id BIGINT UNIQUE;
```

### Commit T.2 — Request endpoint

`POST /api/auth/telegram/request` — body `{username: "otavio"}`
(operator tells us which identity they're claiming; subsequent
ownership is verified via Telegram's DM). Rate-limited 3/min/IP.

Writes a magic_links row, then invokes the bot via the existing
dispatcher:

```
channel_deliveries.insert({
  kind: 'telegram_dm',
  target: operator.telegram_user_id,
  content: { text: `Dashboard sign-in: ${url}/auth/magic?t=${token}` }
})
```

The dispatcher already handles Telegram sends; we piggyback on
its delivery pipeline (see migration 009) so the Node Route
Handler stays pure DB.

Audit: row with action='auth.magic-link.request', subject contains
the username but NOT the token.

### Commit T.3 — Consume endpoint

`GET /auth/magic?t=<token>` — Server Component that:

- Hashes the token (sha256).
- Looks up the magic_links row by hash, checks `expires_at > NOW()`
  and `consumed_at IS NULL`.
- On hit: marks the row consumed, creates a dashboard_sessions row,
  sets the SESSION_COOKIE, `redirect("/agents")`.
- On miss/expired: render an error page with a "Send me a new
  link" button.

Audit: `auth.magic-link.consume`, ok=true/false.

### Commit T.4 — UI

Replace the existing stub banner on `/auth` when mode=telegram
with a form:

```tsx
<form action="/api/auth/telegram/request" method="POST">
  <input name="username" required />
  <Button type="submit">Send me a link</Button>
</form>
```

The submit button shows a success toast ("Check your Telegram")
and the form disables for 30 s to discourage retry spam.

### Commit T.5 — Cleanup job

A once-a-minute cleanup inside the Next server (setInterval on
module init, bounded by process lifetime) deletes magic_links rows
where `expires_at < NOW() - INTERVAL '1 hour'`. Tiny; prevents
table bloat.

### Commit T.6 — Bot integration

Bot-side hook in `internal/bot/handlers.go`: accept a `/dash`
command that inserts an `operator_credentials` row for the sender
if missing, immediately triggers the `POST /api/auth/telegram/
request` flow over the internal network, and echoes the result.
Gives "start from Telegram" ergonomics without requiring the
operator to hit `/auth` first.

### Commit T.7 — Playwright specs

4 × 2 specs in `tests/e2e/auth-telegram.spec.ts`:

- Request happy path inserts a magic_links row + a
  channel_deliveries row (asserted via `withDb`).
- Consume happy path sets the session cookie + deletes (marks
  consumed) the magic_links row + session cookie passes middleware
  on `/agents`.
- Expired link renders the error page.
- Rate limit returns 429 after 3 requests from the same IP.

`MAQUINISTA_DASHBOARD_AUTH=telegram` is set via the global-setup
env propagation; a small dashboard-restart harness flips modes
between spec blocks (expensive — run in its own Playwright project
configured with `testMatch: /auth-telegram/`).

## Files

New:

```
internal/db/migrations/028_dashboard_magic_links.sql     T.1
internal/dashboard/web/src/app/api/auth/telegram/request/route.ts T.2
internal/dashboard/web/src/app/auth/magic/page.tsx        T.3
internal/dashboard/web/src/components/dash/magic-link-request.tsx T.4
internal/dashboard/web/src/lib/magic-links.ts             helpers
internal/dashboard/web/tests/e2e/auth-telegram.spec.ts    T.7
```

Modified:

```
internal/dashboard/web/src/app/auth/page.tsx              T.4 (replace stub)
internal/bot/handlers.go                                  T.6
internal/dashboard/web/src/middleware.ts                  allow /auth/magic
```

## Security notes

- The URL carries a 256-bit random token. DB stores only
  `sha256(token)`; a DB leak doesn't expose live links.
- `consumed_at` is single-use — a second fetch is rejected.
- 10-minute expiry caps the exposure window.
- Telegram itself is the out-of-band channel; the URL is only as
  secure as the operator's Telegram account. For v1 that's the
  right posture (operator already uses the bot for every
  maquinista action). Multi-operator deployments with hostile
  neighbours should stick to `password` mode.

## Verification per commit

Each commit's Playwright spec is its gate. The full set:

- Request without a Telegram identity → 400.
- Request with an identity → magic_links row + channel_deliveries
  row appear.
- Visit `/auth/magic?t=<token>` → session cookie set, redirect to
  `/agents`, magic_links row has `consumed_at != NULL`.
- Second visit with same token → error page.
- Expired token → error page.
- `/dash` command from the bot round-trips to a working magic link.

## Open questions

1. **Who allocates operator_credentials rows for Telegram
   identities?** T.6 assumes the first `/dash` from an allowed
   Telegram user auto-creates a row. Safer v1: require manual
   seeding by the operator (same one-shot psql trick as the
   password path), the bot `/dash` only finds-and-triggers.
2. **Scope gate for magic-link requests.** With
   `ALLOWED_USERS` already enforced on every bot command, the
   bot hook is naturally restricted. The Next-side `POST
   /api/auth/telegram/request` has no such gate; it should
   confirm the username corresponds to a live operator before
   inserting a link row (deny-list unknown usernames, but without
   leaking which usernames exist — return 200 either way and
   log to audit).
3. **Link via email alternative?** Out of scope for this plan;
   Telegram is the maquinista-native channel.
