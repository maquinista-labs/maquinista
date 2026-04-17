# Productization: Maquinista as a SaaS

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres
> is the system of record.** Multi-tenancy is a Postgres tenancy
> problem first, not an infra problem.

## Context

Today maquinista is a single-operator tool: one Postgres, one tmux
session, one Telegram bot, one set of API keys in a `.env` file. The
[dashboard plan](dashboard.md) assumes `127.0.0.1:8900` and one
operator. To sell it, we need to turn the single-operator shape into
a multi-tenant product without losing the thing that makes it
interesting on mobile: **Telegram is the control plane**.

This plan answers three questions:

1. What's the smallest set of features we can charge for?
2. Who's the competition and where do we actually win?
3. What does the system need so a stranger can land on a URL, sign
   up, connect Telegram, and be running an agent in 5 minutes?

The [dashboard](dashboard.md) is a prerequisite — at minimum
Phase 1 (read-only) and Phase 4 (auth). This plan assumes it's
shipped.

## Positioning

**What maquinista is, rewritten for a landing page:**

> Your AI engineering team, dispatched from Telegram. Spawn a fleet
> of Claude / OpenCode agents, hand them GitHub issues, and watch
> them ship PRs from your phone. One-tap approve. Worktree-isolated.
> Cost-capped. No IDE required.

**Who it's for:**

- Solo founders and indie hackers who want to ship while away from
  the laptop (school run, airport, bed).
- Small dev teams (2–10) who want a shared agent fleet with
  per-developer Telegram access.
- Agencies running parallel client projects who need many
  worktree-isolated agents at once.

**Who it's not for (yet):**

- Enterprises with SOC2/HIPAA requirements (Phase 4 hardening
  needed; see §Compliance).
- Non-technical users (maquinista spawns CLIs, not chat-only agents).

## Competitive landscape

### Direct (multi-agent / orchestration)

| Product | Shape | Price | Maquinista vs. |
|---|---|---|---|
| **Cognition Devin** | Hosted autonomous engineer, Slack-first | $500 / mo "Team", ACU-metered | We're 10-25× cheaper, BYOK, Telegram-native, you own the agent |
| **Factory.ai Droids** | Hosted "droid" fleet, web UI | $25–99 / seat | We're self-hostable, multi-runner (not locked to one model), Telegram |
| **OpenHands Cloud** (All Hands AI) | Hosted OSS agent, web IDE | $20 / mo + credits | We're orchestration over *many* agents, not one browser session |
| **Cursor Background Agents** | Agents inside the IDE | Bundled in $20–40 Pro/Biz | We're IDE-free, mobile-first, multi-project parallel |
| **Conductor** (melty) | Multi-agent desktop app | Free / open beta | We're Telegram-first and cloud-hosted, not a Mac app |
| **CodeBuff** | CLI coding agent | $20 / mo | Single-agent; we orchestrate fleets |

### Adjacent (single-agent coding)

| Product | Shape | Price |
|---|---|---|
| **Claude Code** (Anthropic) | CLI, local | Pay-per-token via API |
| **Aider** | OSS CLI | Free + BYOK |
| **Cline / Roo Code** | VS Code extension | Free + BYOK |
| **Warp.dev Agent** | AI terminal | $18 / mo Pro |
| **GitHub Copilot Workspace** | Web, issue-driven | Bundled $10–39 |
| **Zed Agents** | IDE-native | Free / BYOK |
| **Replit Agent** | Browser IDE | $25 / mo Core |

### Observability / dashboards only

| Product | Shape |
|---|---|
| **ClawMetry** | Grafana-style flow graphs for OSS agents |
| **hermes-webui**, **openclaw-dashboard** forks | SPAs around one agent |
| **LangSmith / Langfuse** | Tracing for agent frameworks |

### Where maquinista wins (honestly)

1. **Telegram as the operator interface.** Every competitor assumes
   a laptop. Nobody else nails "approve the PR from the supermarket
   queue". This is unique and defensible.
2. **Fleet, not a single agent.** Devin-tier fleet semantics at
   indie-hacker prices. Parallel worktree isolation is non-trivial
   and already works.
3. **BYOK + multi-runner.** Claude Code, OpenCode, custom binaries.
   Users aren't locked to a model or a vendor's token margin.
4. **Postgres substrate.** Every agent action is inspectable SQL.
   Competitors are black boxes.
5. **Self-hostable escape hatch.** Same binary runs on a laptop,
   a VPS, or our cloud. Enterprise conversations go easier.

### Where we lose (honestly)

1. **No IDE integration.** Cursor and Zed users won't switch.
   Counter: we complement, don't replace — they run agents *in* the
   editor, we run agents *while you're away from* the editor.
2. **No web code view (Phase 1).** Devin and OpenHands let you see
   the agent type. We defer to the dashboard's read-only log. Live
   code view is [dashboard.md Phase 3+](dashboard.md).
3. **Learning curve.** Telegram topics, tmux windows, task specs —
   this isn't "paste a prompt and go". Onboarding (§7) must flatten
   this.

## MVP feature gap — what's missing to sell it

Current state (as of the dashboard plan + §0 migration):

- ✅ Multi-agent orchestration, per-topic mapping, task queue.
- ✅ Pluggable runners (claude, opencode, custom).
- ✅ Agent soul + memory (shipped in recent commits).
- ✅ A2A messaging Phase 1.
- ✅ Postgres-as-system-of-record.
- 🚧 Dashboard (read-only in progress; auth Phase 4 pending).
- ❌ Everything below.

### Must-have for paid MVP

| # | Feature | Why |
|---|---|---|
| M1 | **Sign-up / sign-in** | Self-serve onboarding; the #1 gate |
| M2 | **Workspace = tenant isolation** | One paying customer = one workspace; agents can't see other tenants' data |
| M3 | **Telegram account linking** | User's bot token or our shared bot; map Telegram user → workspace |
| M4 | **BYOK secrets vault** | Users paste their Anthropic / OpenAI / OpenRouter keys; we encrypt-at-rest |
| M5 | **Hosted agent runtime** | Managed tmux / container per workspace; user doesn't SSH anywhere |
| M6 | **GitHub OAuth app** | Read issues, open PRs; one-click repo connect |
| M7 | **Usage metering + cost caps** | Per-workspace token counter, hard cap, soft-cap alert to Telegram |
| M8 | **Billing (Stripe)** | Subscription + metered overage |
| M9 | **Dashboard auth (SaaS mode)** | Dashboard plan Phase 4, but session-cookie with workspace scoping |
| M10 | **Onboarding flow** | Landing → sign up → link Telegram → connect repo → first task → first PR, < 10 min |
| M11 | **Terms / Privacy / DPA template** | Legal minimum to take money |
| M12 | **Support channel** | Intercom or self-hosted Plausible-style chat |

### Should-have for launch month

| # | Feature | Why |
|---|---|---|
| S1 | **Team seats** | Add teammates; shared fleet; per-seat billing |
| S2 | **Agent templates library** | "Frontend bug-fixer", "Test writer", "PR reviewer" presets to shortcut M10 |
| S3 | **Approval quorum** | `requires_approval: true` tasks need N operator 👍 before merge |
| S4 | **Slack / Discord bridges** | Not everyone lives on Telegram; keep Telegram as the hero |
| S5 | **Webhook + cron surface** | Exposed from [dashboard](dashboard.md) Phase 2; productize it as "Triggers" |
| S6 | **Audit log (tenant-scoped)** | Shipped as part of dashboard Phase 4; tenant-filter it |
| S7 | **Status page** | `status.maquinista.io` driven by health probes |

### Nice-to-have (post-MVP, quarter 2)

- Live code view (streamed tmux-capture-pane over SSE, sanitized).
- Voice input on mobile (Telegram voice → Whisper → inbox).
- Self-hosted tier (license key gate on the same binary).
- Marketplace for agent templates / skills.
- SSO (Google, Microsoft, Okta for team plans).
- SOC 2 Type I.

## Architecture changes

The dev-shape → SaaS-shape delta is mostly one word: **tenant**.

### Tenancy model

Every row that today has `agent_id TEXT` or `project TEXT` gains a
`workspace_id UUID NOT NULL`. Workspaces are the billing boundary.

```sql
-- migration 030_workspaces.sql
CREATE TABLE workspaces (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slug         TEXT UNIQUE NOT NULL,      -- URL-safe, e.g. "acme"
  display_name TEXT NOT NULL,
  plan         TEXT NOT NULL DEFAULT 'trial',  -- trial|solo|team|agency|self-hosted
  status       TEXT NOT NULL DEFAULT 'active', -- active|past_due|suspended|deleted
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  stripe_customer_id TEXT,
  deleted_at   TIMESTAMPTZ
);

CREATE TABLE users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email         CITEXT UNIQUE NOT NULL,
  password_hash TEXT,                      -- nullable if SSO-only
  email_verified_at TIMESTAMPTZ,
  telegram_user_id BIGINT UNIQUE,          -- after /link flow
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at TIMESTAMPTZ
);

CREATE TABLE workspace_members (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role         TEXT NOT NULL,              -- owner|admin|operator|viewer
  invited_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  joined_at    TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, user_id)
);

CREATE TABLE sessions (
  id           TEXT PRIMARY KEY,           -- opaque, 32 bytes base64
  user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id UUID REFERENCES workspaces(id),  -- active workspace
  ua           TEXT, ip INET,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE workspace_secrets (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,              -- anthropic_api_key|openai_api_key|github_token|telegram_bot_token
  ciphertext   BYTEA NOT NULL,             -- AES-GCM, DEK wrapped by KMS-KEK
  kek_id       TEXT NOT NULL,
  nonce        BYTEA NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  rotated_at   TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, kind)
);

ALTER TABLE agents           ADD COLUMN workspace_id UUID NOT NULL REFERENCES workspaces(id);
ALTER TABLE agent_inbox      ADD COLUMN workspace_id UUID NOT NULL REFERENCES workspaces(id);
ALTER TABLE agent_outbox     ADD COLUMN workspace_id UUID NOT NULL REFERENCES workspaces(id);
ALTER TABLE agent_souls      ADD COLUMN workspace_id UUID NOT NULL REFERENCES workspaces(id);
ALTER TABLE agent_memory     ADD COLUMN workspace_id UUID NOT NULL REFERENCES workspaces(id);
ALTER TABLE scheduled_jobs   ADD COLUMN workspace_id UUID NOT NULL REFERENCES workspaces(id);
ALTER TABLE webhook_handlers ADD COLUMN workspace_id UUID NOT NULL REFERENCES workspaces(id);
-- … every table with tenant data.
```

**Isolation strategy:** Postgres Row-Level Security (RLS) with a
`current_workspace_id` session variable set at the start of every
request / worker job.

```sql
ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
CREATE POLICY agents_tenant ON agents
  USING (workspace_id = current_setting('app.workspace_id')::uuid);
```

Repeat for every tenant-owned table. The Go query layer (currently in
`internal/db/`) gets a thin wrapper that runs `SET LOCAL app.workspace_id`
per transaction. The operator never writes raw tenant-free queries.

### Compute isolation

The single `tmux` session becomes one session *per workspace*, run
inside a sandbox:

- **Option A (MVP): shared host, namespaced tmux + Linux user.**
  One systemd unit per workspace, dropping to `maquinista-ws-<slug>`
  with resource limits (`CPUQuota=100%`, `MemoryMax=4G`,
  `TasksMax=256`). Cheap, easy, no Kubernetes.
- **Option B (launch month): Firecracker microVMs per workspace.**
  Stronger isolation, same API surface. Switch once we have N>50
  paying workspaces.
- **Option C (enterprise): BYO-VPC runtime.** Enterprise pays to run
  the runtime in their cloud; we host only the control plane. This
  is the same self-host binary with a `--control-plane=<url>`
  registration flag. Defer.

### API keys & secrets

Never store plaintext. KMS options:

- **AWS KMS** — envelope encryption; the DEK (data encryption key)
  lives in Postgres wrapped by KMS KEK. Rotation = re-wrap.
- **age (filippo.io/age)** — cheaper if hosting on Hetzner / Fly;
  identities in 1Password for the operator bootstrap.
- **Vault (HashiCorp)** — overkill at MVP; revisit at team plan.

Ship **AWS KMS** (or equivalent on Fly.io / Cloud KMS if we host
there) — saves us writing key-rotation logic.

### Dashboard → tenant-aware

The existing `/dash/*` routes gain a workspace prefix and a scoped
query context:

```
Before: /dash/agents/{id}
After:  /ws/{workspace_slug}/agents/{id}
```

Session cookie stores `user_id`; an active-workspace cookie (or
URL segment) selects `workspace_id`. Every handler:

```go
func (s *Server) withTenant(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        u := sessionUser(r)
        ws := resolveWorkspace(r, u)
        if !hasMembership(u, ws) { http.Error(w, "", 403); return }
        ctx := context.WithValue(r.Context(), ctxWorkspace, ws.ID)
        next(w, r.WithContext(ctx))
    }
}
```

The SSE stream (`/dash/stream`) filters events by `workspace_id`
before emitting. Listener goroutines check the channel payload's
workspace column and drop mismatches.

### Telegram: one bot or many?

Two viable shapes:

1. **Shared "@maquinista_bot"** — one bot for everyone. Pros: no
   BotFather dance during onboarding. Cons: users can't customize
   the bot name / avatar, and Telegram rate-limits one bot across
   all tenants.
2. **BYOB (Bring Your Own Bot)** — user creates a bot via @BotFather
   (60 s) and pastes the token. Pros: tenant-owned brand, separate
   rate limits. Cons: one more onboarding step.

Ship **both**. Default to shared for fastest onboarding; offer BYOB
as a "graduation" feature unlocked at Team plan. The existing bot
code runs N instances (one per workspace with a custom token) on the
same process via goroutines + webhook routing.

```
-- migration 031_telegram_bots.sql
CREATE TABLE telegram_bots (
  workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  mode         TEXT NOT NULL,          -- 'shared' | 'byob'
  bot_token_secret_id UUID,            -- nullable in shared mode
  bot_username TEXT NOT NULL,
  webhook_secret TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Billing plumbing

Stripe subscriptions, metered for overages. Webhook at
`/webhooks/stripe` updates `workspaces.plan` and `workspaces.status`:

```sql
CREATE TABLE billing_events (
  id           BIGSERIAL PRIMARY KEY,
  workspace_id UUID REFERENCES workspaces(id),
  stripe_event_id TEXT UNIQUE,
  kind         TEXT NOT NULL,           -- subscription.created, invoice.paid, …
  payload      JSONB NOT NULL,
  processed_at TIMESTAMPTZ
);

CREATE TABLE usage_counters (
  workspace_id UUID NOT NULL REFERENCES workspaces(id),
  period_start DATE NOT NULL,           -- billing period, month-aligned
  agents_active_peak INTEGER NOT NULL DEFAULT 0,
  tasks_shipped INTEGER NOT NULL DEFAULT 0,
  agent_hours  NUMERIC(10,2) NOT NULL DEFAULT 0,
  tokens_in    BIGINT NOT NULL DEFAULT 0,
  tokens_out   BIGINT NOT NULL DEFAULT 0,
  usd_cents_spent INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (workspace_id, period_start)
);
```

We **do not mark up token cost** on BYOK — users pay their LLM
provider directly. We charge for the orchestration substrate (agent
hours, seats, concurrent agents, hosted compute).

## Pricing

Pricing is a belief, not a spreadsheet. Below is a belief with
numbers attached.

### Free tier — "Hobby"

- 1 user, 1 workspace
- 1 concurrent agent
- 10 agent-hours / month
- BYOK required (no managed credits)
- Shared Telegram bot only
- Community support (GitHub Discussions)
- Public repos only

**Why:** Lets people try without a credit card; caps the abuse
surface; public-only removes the biggest compliance risk.

### Solo — **$19 / mo** (or $180/yr)

- 1 user, 1 workspace
- 3 concurrent agents
- 100 agent-hours / mo (≈ 3.3 h/day)
- Private repos
- Shared or BYOB Telegram bot
- Email support, 48 h SLA
- $0.25 / agent-hour overage

**Target:** indie hackers, solo consultants.

### Team — **$49 / seat / mo** (min 2, or $480/yr/seat)

- Per-seat billing
- 10 concurrent agents per workspace
- 500 agent-hours / mo (pooled)
- All channels: Telegram + Slack + Discord + Web
- Approval quorum, role-based access
- Priority support, 24 h SLA
- $0.20 / agent-hour overage

**Target:** 2–10 dev teams, agencies on one engagement.

### Agency — **$299 / mo flat**

- Up to 10 workspaces (one per client)
- Unlimited seats within a workspace
- 25 concurrent agents per workspace
- 2000 agent-hours / mo pooled across workspaces
- White-label: custom Telegram bot name, custom dashboard domain
- Bring Your Own Worker (connect a self-hosted worker for sensitive
  clients)
- Shared Slack channel with us for support

**Target:** dev agencies running 3+ concurrent client projects.

### Self-hosted — **$0 (open core)** or **$199 / mo**

- The binary is free to run locally or on your own box (MIT-ish
  core).
- Paid **Self-Hosted Business** unlocks: dashboard auth modes
  (TOTP / SSO), multi-workspace, audit log retention > 30 days,
  priority support, license-key-gated builds.
- Feature parity with Team, infra under the customer's control.

**Target:** regulated industries, DIY operators with a spare VPS,
enterprise pilots.

### Enterprise — **"Talk to us"**

- BYO-VPC / air-gapped deployment
- SAML SSO, SCIM provisioning
- SOC 2 Type II, DPA, custom MSA
- Dedicated support, named account engineer
- Volume token commits via Anthropic / OpenAI direct contracts
- Starts at $2,500 / mo

**Target:** companies that can't put secrets into someone else's
cloud.

### Pricing rationale

- **Solo at $19** undercuts Cursor ($20), Warp ($18), CodeBuff ($20),
  OpenHands ($20) by a buck while offering fleet semantics they don't.
  Anchoring on the cheapest-useful-AI-dev tier is deliberate.
- **Team at $49/seat** sits where Cursor Business ($40) and Factory
  ($99) bracket. We price in the middle because fleet + Telegram is
  a legitimate upgrade over IDE-only; we don't price at Devin levels
  ($500) because BYOK means we don't have their infra cost.
- **Agency at $299 flat** converts shops that resent per-seat math.
  Ten workspaces × $19 = $190 Solo-equivalent, so we capture upside
  for multi-client shops without nickel-and-diming.
- **Self-hosted at $199** is **more expensive** than Solo on
  purpose — managed is the default path; self-host is for people
  who need it, and they'll pay.
- **Overage not per-token.** Agent-hour makes sense because that's
  what users feel (they ran an agent for 3 hours). Token cost is
  their LLM bill.

### What we will not do

- **Free unlimited.** Breaks unit economics; attracts crypto miners.
- **Pay-per-PR.** Sounds clever, incentivizes bad PRs. Agent-hour
  aligns incentives better.
- **Enterprise-only.** We start with prosumer and move up. Starting
  top-down means a 12-month sales cycle with zero validation.

## Onboarding flow (the 5-minute demo)

The new-user journey, designed to fit on a 6-inch phone:

```
1. Land on maquinista.io, hit "Start free"
2. Enter email + password (or "Continue with GitHub")
3. Verify email (magic link)
4. Pick workspace slug → workspace created
5. "Connect Telegram" → tap link → Telegram opens →
   /start → bot replies "Linked to workspace `acme`. Welcome."
6. "Paste your Anthropic API key" (tested via a noop /ping to
   /v1/messages; green check on success)
7. "Connect GitHub" → OAuth → pick a repo
8. "Start your first agent" → one-click spawns an agent named
   `hello` bound to the repo, sends inbox row "What can you do?"
9. Dashboard opens; watch the outbox stream reply in real time
10. "Now ask it to do something": prompt text field with placeholder
    "Find and fix a typo in the README"
```

Instrument every step as a funnel event; the drop-off between
step 4 and step 5 is the biggest onboarding risk (Telegram out-of-
app bounce). Mitigation: deep-link with a prefilled bot token arg
so @-tagging the right bot is a single tap.

Time budget:

| Step | Target | Blocker if > |
|---|---|---|
| 1–4 | 60 s | password form sluggish |
| 5 | 60 s | Telegram deep link broken |
| 6 | 45 s | API key paste / verify slow |
| 7 | 45 s | GitHub OAuth consent |
| 8–10 | 90 s | agent spawn latency > 10 s |

**Target total: < 5 min.** Anything over 7 min loses half the users.

## Security & compliance (the minimum)

To take money without embarrassing ourselves:

- **Data at rest:** Postgres disk encryption (RDS / Fly Volumes /
  whatever we land on) + column-level encryption for API keys.
- **Data in transit:** TLS 1.3 everywhere; HSTS on the dashboard.
- **Auth:** bcrypt/argon2id for passwords; session rotation on
  privilege change; CSRF double-submit cookies for POSTs.
- **Secret scanning:** run `trufflehog` on every pushed PR before
  the agent opens it, to avoid agents leaking customer secrets in
  commits.
- **Rate limits:** per-user (sign-in, sign-up), per-workspace
  (inbox enqueue, API calls), per-IP.
- **Abuse:** sign-up requires email verification + optional Turnstile
  captcha on suspicious IPs.
- **Backups:** daily Postgres snapshots, 30-day retention; tested
  restore once a quarter.
- **Legal pages:** Terms, Privacy, Acceptable Use, DPA (template).
  Use a founder-friendly template (Stripe Atlas, Clerky) and have a
  lawyer review before any enterprise contract.
- **Subprocessors list:** Stripe, Anthropic, OpenAI, AWS, Cloudflare,
  Telegram. Published on `/legal/subprocessors`.
- **Incident response:** simple email-first runbook; PagerDuty once
  we have paying customers in > 2 timezones.

**What we defer** until a customer asks (and pays enough to fund
the audit):

- SOC 2 Type I / II (budget: ~$30k / ~$60k)
- HIPAA BAA
- FedRAMP
- ISO 27001

## Observability (for us, not the user)

- **App metrics:** Prometheus + Grafana. Agent spawn latency,
  inbox consumer lag, token cost per workspace.
- **Logs:** structured JSON to Loki / Grafana Cloud.
- **Error tracking:** Sentry for the Go process + the dashboard
  frontend.
- **Analytics:** Plausible (privacy-respecting) on the marketing
  site; PostHog for product usage inside the app.
- **Uptime:** Better Stack / Upptime on `status.maquinista.io`.

## Go-to-market

Low-budget, founder-led, in the order we should do them:

1. **Dogfood** — use maquinista to build maquinista. We are already
   doing this; publish the agent conversation logs (sanitized) as
   marketing content.
2. **Launch on Hacker News** with a 90-second screen recording of
   "fix a bug from bed" (phone-only demo).
3. **Product Hunt** the same week as HN.
4. **Dev Twitter / BlueSky** threads showing the fleet UI on mobile;
   these screenshots are visually distinct from every other coding
   tool.
5. **YouTube demo** — 3-minute "you already use Telegram; now use it
   to ship code" walkthrough.
6. **Agency outbound** — 50 hand-picked agencies, personalized
   Loom each, offering a free month.
7. **Indie Hackers** post-mortem of the build.
8. **Conferences:** GopherCon lightning talk on "Telegram as an
   operator UI", FOSDEM room on multi-agent orchestration.

Content moats: "How we built X with N parallel agents" posts, each
one effectively a feature launch + a dogfooding case study.

## Phases

### Phase 1 — Foundation (weeks 1–4)

- Migrations 030 (workspaces/users/sessions), 031 (telegram_bots).
- RLS policies on every tenant table.
- `internal/db/tenant.go` wrapper that sets `app.workspace_id`
  per transaction.
- `internal/auth/` package: sign-up, verify email, sign-in, session
  cookie, CSRF.
- Dashboard routes re-prefixed under `/ws/{slug}/...`.
- Marketing site skeleton at `maquinista.io` (static Hugo site).

**Exit:** two users can sign up on a staging URL, create separate
workspaces, spawn agents, and not see each other's anything.

### Phase 2 — Billing + keys + onboarding (weeks 5–8)

- Stripe integration (`/webhooks/stripe`, `/billing/*` routes).
- Secrets vault (`internal/secrets/` + KMS envelope).
- Onboarding wizard: email-verify → slug → Telegram link →
  API key → GitHub → first agent.
- Agent-hours metering (`internal/billing/meter.go` tails
  `agent_turn_costs` and rolls into `usage_counters`).
- Usage caps enforced at inbox-enqueue time.
- Public pricing page.

**Exit:** a stranger can hit the landing page, pay us $19, and have
a working agent within 10 min.

### Phase 3 — Team plan (weeks 9–12)

- `workspace_members`, invitations, roles (owner/admin/operator/viewer).
- Slack + Discord bridges re-using the existing channel-deliveries
  plumbing.
- Agent template library (seeded `agent_souls` blueprints).
- Approval quorum on tasks with `requires_approval=true`.
- Audit log (tenant-scoped view from [dashboard.md Phase 4](dashboard.md)).

**Exit:** a 5-person team pays us $245 / mo (5 × $49) and has a
shared fleet.

### Phase 4 — Self-hosted Business + Agency (weeks 13–16)

- License key gating in the binary (`maquinista license verify`).
- Multi-workspace mode for self-hosted.
- White-label config (custom bot name, CNAME for dashboard).
- Self-hosted update channel (`maquinista upgrade`).
- Support playbook + Intercom / Help Scout.

**Exit:** first agency paying $299 / mo; first self-hosted business
license.

### Phase 5 — Enterprise runway (quarter 2)

- SAML SSO (`internal/auth/saml.go`).
- SCIM provisioning.
- BYO-VPC worker registration protocol.
- SOC 2 Type I engagement kickoff.
- Sales motion: outbound to 20 hand-picked prospects.

**Exit:** one enterprise pilot signed (even if it's a design
partner at a discount).

## Open questions

1. **Marketing domain.** `maquinista.io` (available? check), or
   a new brand? "Maquinista" is evocative but hard to spell for
   non-Portuguese speakers; consider brand-front / product-back.
2. **Hosting region.** Start in `fra` (EU) for GDPR friendliness,
   or `iad` (US) for latency to most LLM endpoints? Probably both
   from day one, at the cost of a multi-region Postgres read
   replica setup. Defer to single-region MVP.
3. **Do we run the LLM calls or just proxy?** Proxying lets us
   cache + observe across tenants but creates a compliance surface
   (we see prompts). BYOK-direct means we never see the bytes.
   Default to BYOK-direct; revisit when we need cost optimization.
4. **Telegram as control plane is the differentiator — what if
   Telegram dies / gets banned in a key market?** Slack + Discord
   bridges ship in Phase 3 as a safety net. Design the mailbox so
   channels are pluggable (already true via `channel_deliveries`).
5. **Open-source licensing.** MIT for the engine, commercial
   license for dashboard pro features? BSL (Business Source
   License) for everything with a 4-year Apache conversion? Pick
   one before any first release — changing later erodes trust.
   Strong recommendation: **BSL 1.1 with additional-use grant for
   self-hosted < 50 seats**, converting to Apache-2.0 after 4
   years. Matches Sentry / CockroachDB precedent.
6. **Fair-source / fair-code movement.** The above license stance
   aligns; commit to publishing all code in public even before
   launch, to build pre-launch trust.
7. **Will we take VC money?** Not to ship MVP. The unit economics
   work at < 1000 paying customers without outside capital given
   BYOK removes the biggest cost item. Revisit at $50k MRR.

## What we deliberately cut from MVP

- **Web-based code editor** (see Replit, Devin). Huge build;
  conversation + screenshot is 80 % of the value on mobile.
- **Custom model fine-tuning per workspace.** Deep rabbit hole.
- **On-prem GitHub Enterprise support.** Hold for Enterprise phase.
- **Multi-language marketing (PT / ES).** We speak Portuguese and
  it's tempting for a LATAM launch, but translating a pricing page
  isn't the bottleneck to product-market fit.
- **Mobile native apps.** PWA (already in dashboard plan) covers
  90 % of the mobile case at < 5 % of the build effort.

## Success metrics

Month 1 post-launch:
- 50 sign-ups, 10 activate (land a first PR), 3 convert to paid.

Month 3:
- 300 sign-ups, 60 active, 30 paid → ~$700 MRR.

Month 6:
- 1000 sign-ups, 200 active, 120 paid, 2 Agency, 1 Enterprise pilot
  → ~$5k MRR.

Month 12:
- 5000 sign-ups, ~500 paid customers, ~$25k MRR. Sustainable
  one-founder business or a credible seed-round pitch.

None of these numbers matter until step 10 of the onboarding flow
takes < 5 min on a phone with flaky wifi. Ship that first.

## Interaction with other plans

- **`dashboard.md`** — hard prerequisite. Phase 1 of this plan
  needs dashboard auth Phase 4 already in place. The workspace
  prefix (`/ws/{slug}/...`) is a superset of what the dashboard
  plan proposes.
- **`multi-agent-registry.md`** — fleet semantics + reconcile loop
  are what this plan *sells*. Phase 2–3 of that plan must be
  shipped before Team plan makes sense.
- **`agent-soul-db-state.md`** + **`agent-memory-db.md`** — become
  the "agent template library" on the Team plan; each workspace
  clones templates into its own souls/memory rows.
- **`agent-to-agent-communication.md`** — A2A within a workspace is
  fine; across workspaces is explicitly forbidden by RLS.
- **`checkpoint-rollback.md`** — "Rewind" is a premium feature;
  could gate it to Team+ as a pricing lever (or keep on all tiers
  as a trust builder; probably the latter).
- **`opencode-integration.md`** — multi-runner is a selling point
  on the pricing page; finish OC-01..04 before launch so the
  "pluggable runners" claim isn't marketing-only.
