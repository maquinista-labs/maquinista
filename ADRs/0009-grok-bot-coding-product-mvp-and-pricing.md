# ADR-0009: A Grok-Bot-Style Coding Agent Product on Top of Maquinista — Landscape, Gaps, MVP and Pricing

- **Status:** Aceito (2026-10-09 — Otavio ratified the direction in chat; see Rev 2. Remaining unanswered items moved to §Still open and gate the fase ADRs, not this one)
- **Date:** 2026-10-09
- **Amended:** 2026-10-09 — **Rev 2** (Otavio, chat): (1) landscape comparison refocused **Telegram-native**; (2) product surface = **our own IDE/app** — the existing dashboard splits out of the maquinista binary into a standalone web app, composed with the Telegram leg on top of maquinista execution; (3) agent execution isolated in **gVisor/microVM** sandboxes (ADR-0004's sandbox knob gets its concrete first value)
- **Amended:** 2026-10-09 — **Rev 3** (Otavio, chat): pricing model decided — **BYOK only from day 1** (user's own OpenRouter/z.ai/Claude keys or runner-CLI subscription auth); we charge a **fixed platform subscription (ARR-style) on top of measured infrastructure cost** — never tokens. Predictable for us as we scale, predictable for the user (model spend stays on their own provider plans). Rev 1 pricing ladder marked superseded.
- **Amended:** 2026-10-09 — **Rev 4** (Otavio, chat): product is **B2C only** — individual developers, no team/enterprise tiers, ever; concrete **price estimates added** (Free / $9.90 / $24.90) derived from the infra-cost formula, pre-F0 calibration. "ARR-style" reads as MRR under B2C.
- **Deciders:** Otavio
- **Scope:** Product exploration — "what would a Grok-Bot analog for coding look like built on maquinista". Landscape snapshot (market-analysis type, re-runnable), capability have/miss inventory, priority-rated gap backlog, MVP fases, pricing ladders. NOT a build authorization; each fase that touches the repo state machine gets its own MAQ ticket + spec.
- **Type:** 9 (market analysis snapshot) + 8 (product/GTM) hybrid; the landscape tables carry their own re-run procedure.

## Context

### The product thesis

Grok Bot (x.ai, launched Sep 2026) proved the UX: you message a teammate-shaped bot in chat, it goes away, does computer work, and comes back "when your approval is needed" (verified: x.ai/bot, x.ai/news/team-bots, fetched 2026-10-09). The coding analog — **message a bot in your pocket, it opens a session on your repo, ships a PR through review, and asks you only when a gate needs a human** — is exactly what maquinista already does internally for its own repo (ADR-0005, Aceito: 43+ issues shipped through the pipeline; ADR-0008 turn-end contract fully live as of MAQ-43/44, deployed `6afae8c` 2026-10-09).

The question this ADR answers: what exists in that category, what maquinista has and lacks for a hosted multi-user version, what an MVP looks like, and what to charge.

### Verified landscape (fetched 2026-10-09)

**Commercial chat-native coding agents — Slack is the battleground; no commercial player is Telegram-native:**

| Product | Channel | Depth | Pricing (verified, source) |
|---|---|---|---|
| Claude Code in Slack / Claude Tag (Anthropic) | Slack | Full: mention → cloud session on connected repos → Create PR (1 PR/session) | Pro/Max plans; org identity via Claude Tag — code.claude.com/docs/en/slack.md |
| @claude GitHub Action (Anthropic) | GitHub mentions | Full: implement + push on issue/PR mention; separate auto-review product | Pro/Max — code.claude.com/docs/en/claude-code/github-actions.md |
| Codex in Slack (OpenAI) | Slack | Full: mention → Codex Cloud task; admin-enabled delegation, RBAC, per-user connect | ChatGPT Plus $20/mo (help.openai.com, verified); Pro no 5-h limit; Enterprise credit-metered — developers.openai.com/codex/pricing |
| Devin (Cognition) | Slack, Teams, Linear, GitHub | Full: @Devin → "from triage to a pull request"; event automations | Free $0 / Pro $20 / Max $200 / Teams $80 + $40 per add-on seat — devin.ai/pricing (verified). ACU unit price: UNVERIFIED today (re-check devin.ai/pricing) |
| Factory Droid | Slack, Linear, Jira, Teams | Full delegations flow (private preview), DMs use personal identity | Enterprise, n/a on public page (verified absence) |
| OpenHands Cloud (All Hands) | Slack + web | Full cloud sessions, OAuth app | Pricing page payload shows $10/$19 anchors + "Free, local" + "providers at-cost" — SINGLE-SOURCE, AMBIGUOUS (openhands.dev/pricing); treat UNVERIFIED |
| Grok Bot / Team Bots (x.ai) | Grok apps + Slack handles (Team Bots); NOT X mentions | Generalist computer-use teammate — NOT repo/PR-native; GitHub only a plugin | $20 Pro; SuperGrok $30; Teams $40/seat — x.ai/bot, x.ai/news/team-bots (verified) |
| Grok Build (x.ai) | Terminal CLI | Coding agent, plan mode "every edit blocked until you approve" | Bundled — x.ai/build |

### Rev 2 — the comparison, focused on Telegram-native

| Player | Telegram-native | Hosted multi-user | Full PR pipeline w/ review gates | Hard session isolation | Entry price (verified 2026-10-09) |
|---|---|---|---|---|---|
| Grok Bot (x.ai) | ❌ (Grok apps + Slack handles) | ✅ | ❌ computer-use, not repo-native | "own computer" model | $20 |
| Claude Code in Slack | ❌ Slack | ✅ | ⚠️ 1 PR/session, no review rounds | cloud sessions | Pro/Max (UNVERIFIED today) |
| Codex in Slack | ❌ Slack | ✅ | ⚠️ task-centric | cloud RBAC | $20 (Plus) |
| Devin | ❌ Slack/Teams/Linear | ✅ | ✅ triage→PR, event automations | cloud sandboxes | $20 (Pro) |
| claude-code-telegram (OSS) | ✅ | ❌ single-user self-host | ❌ no PR/review | dir sandboxing + audit log | self-host |
| openclaw / nanoclaw (OSS) | ✅ (20+ channels) | ❌ personal gateway | ❌ assistant-first | pairing approval + container guide | self-host |
| **The product (this ADR)** | ✅ **wedge** | ✅ | ✅ **already built** | ✅ **gVisor/microVM (Rev 2)** | §Pricing |

Read: on Telegram specifically, nobody commercial exists at all; the only Telegram-native entries are single-user self-host bridges with no pipeline. We bring the pipeline + hosted multi-user + gVisor/microVM isolation to the empty quadrant.

**Consumer anchors:** GitHub Copilot Free / Pro $10 (base 1,000 AI credits) / Pro+ $39 (3,900) / Business $19 per seat (1,900 credits/user) — docs.github.com/en/copilot/get-started/plans.md (verified 2026-10-09). ChatGPT Plus $20/mo. Claude Pro/Max, Cursor, Amp Sourcegraph, Google Jules tiers: **UNVERIFIED today** (JS-walled after the house 2-attempt time-box; re-check anthropic.com/pricing, cursor.com/pricing, ampcode.com, jules.google).

**OSS Telegram-side (the actual frontier — all single-user, none a product):**

| Repo | ★ | What it is | Missing vs. a product |
|---|---|---|---|
| openclaw/openclaw | 391.5k | Personal-agent gateway, 20+ chat channels, harness plugins, pairing approval | Assistant-first, no repo→PR pipeline |
| nanocoai/nanoclaw | 30.9k | Container-sandboxed chat agents | No gated merge pipeline |
| chenhg5/cc-connect | 15.9k | Bridges Claude Code/Cursor/Codex/Gemini CLI to Telegram/Feishu/Slack | Remote control of YOUR local agents |
| overwirehq/claude-code-telegram | 2.8k | Telegram ↔ Claude Code with auth, dir sandboxing, audit log | Self-host single-user; no PR/review rounds |
| PleasePrompto/ductor | 458 | Telegram/Matrix control of coding CLIs, Docker sandbox, cron | Single-user console driver |
| sweepai/sweep | 7.7k | Former chat→PR player | Pivoted to JetBrains; domain dead — the category slot opened |

All OSS stars fetched via api.github.com 2026-10-09.

**The verified gap:** *no hosted, Telegram-native, multi-user product combines GitHub App repo onboarding → mention-triggered agent sessions → in-chat review rounds → gated merge pipeline with per-user identity and audit.* Every commercial player is Slack-first; every Telegram player is a self-host single-user bridge. Grok Bot has the UX but is computer-use generalist, not repo-native, and not on Telegram.

### What maquinista already has (live-code evidence, 2026-10-09, `6afae8c`)

| Capability | Evidence | Product relevance |
|---|---|---|
| Messaging-native intake + delivery | Telegram bot, 4-tier routing ladder, forum topics per agent (arch/routing.md; internal/dispatcher/telegram.go) | The core Grok-Bot UX, already built |
| Full PR pipeline with independent review | dispatch.go: reviewer/fixer spawn, verdict parsing, round caps; ADR-0005 | Nobody on Telegram has this |
| Gated merge queue | mergegate.go (env-independent gates), merge.go (rebase + CI gate + squash) | Trust layer — the differentiator vs "Yolo bots" |
| Self-healing ops | Freeze watchdog + turn-end contract + nudge ledger (ADR-0008, live), orphan sweep + episode claims (MAQ-41, deployed today), reconcile-on-restart | Reliability without babysitters — unmanaged-user prerequisite |
| Human-gate escapes | pending_approval parks + fan-out notifications to PR AND Linear (MAQ-34), requeue CLI (MAQ-40) | The "comes back when your approval is needed" loop, already notification-plumbed |
| Multi-runner LLM backend | claude/pi/codex commands (config.go:166,205); runs glm-5.3-flash today | Cost dial per tier — flash-class for cheap plans, premium models metered |
| Human-rendered notifications | MAQ-37: [MAQ-n] headlines, plain-language status, links on every note | Consumer-grade UX already enforced by pipeline review |
| Dashboard | Embedded Next.js + Go supervisor, SSE (port 8900) | Web surface exists; needs auth hardening before public |
| Ticket bridge | Linear intake + board mirror; ADR-0006 provider seam (Proposto) | Task intake generalizes to user stories |
| Operational proof | 77.8k LOC Go, 47 migrations, 43+ issues shipped, deploy script + health checks | It is not a demo; it runs daily |

### What maquinista is missing (priority-rated; greps stamped 2026-10-09)

**P0 — product-blocking:**
1. **Multi-tenancy** — zero tenant/org concept in migrations (`rg -in 'tenant|organi[sz]ation' internal/db/migrations/` = empty). Need: tenant-scoped tables, per-user workspaces + credentials, per-tenant agent souls. ADR-0003 (substrate scale-out) becomes load-bearing, not optional.
2. **Self-serve onboarding** — auth is `ALLOWED_USERS`/`ALLOWED_GROUPS` env allowlist (config.go). Need: GitHub App install → repo picker → Telegram identity link. No OAuth anywhere today.
3. **Per-user secrets + isolation** — agents run as the box user with broad access; untrusted users need a hard sandbox per session — **tech decided Rev 2: gVisor (runsc) first, microVM scale-out** — plus per-user tokens + audit logging.
4. **Billing/metering** — zero billing code (`rg stripe|billing|invoice internal/ cmd/` = empty). Need usage ledger, quotas, checkout.

**P1 — needed for chargeable quality:**
5. Multi-repo per user + repo management UX (today: one `MAQUINISTA_DIR`).
6. Ticket-provider seam executed (ADR-0006) — GitHub-only today (`internal/gh`); GitLab later.
7. Per-user observability — OTel tracing + per-task cost surfaced in-chat (ADR-0007 adoption items).
8. Reviewer quality guards — temporal verdict guards + benchmark harness (ADR-0007); consumer trust dies on one bad merge story.
9. Channel leg #2 (Discord/WhatsApp) — architecture ready (new inboxecho leg + dispatcher, arch/messaging.md), zero product pressure until retention data.

**P2 — later:**
10. Enterprise (SSO, RBAC, SOC2), voice notes, public dashboard auth.

## Options considered

| | A. Hosted Telegram SaaS on maquinista core | B. OSS self-host distro | C. Slack-first B2B | D. Grok-Bot generalist pivot | E. Status quo (internal tool) |
|---|---|---|---|---|---|
| Fits existing code | ✅ pipeline is the product | ✅ | ⚠️ new channel leg | ❌ discards gates/review crown jewels | ✅ |
| Competition | Empty niche (verified above) | 391k★ openclaw shadow | Devin + Anthropic + OpenAI own it | x.ai with distribution we lack | — |
| Revenue path | Credits/seats from day 1 | Support/hosting, weak | Enterprise ACU war | Consumer sub | none |
| Work to MVP | P0 list (tenancy, onboarding, billing) | Packaging only | Channel + enterprise sales | Ground-up | — |

**Recommendation: A, with B as the free moat** (self-host single-tenant edition OSS — it feeds the hosted product and matches the openclaw-era distribution reality; hosted multi-tenant core stays closed). C and D rejected: we lose every differentiation we already own.

### Product shape (Rev 2 — decided by Otavio, 2026-10-09)

1. **Telegram-native wedge** — the comparison and GTM focus stay on Telegram; other channels remain cheap later legs, not the launch story.
2. **Our own IDE/app** — the existing dashboard splits OUT of the maquinista binary into a standalone web app: repo/session browser, diffs, review verdicts, approvals, cost per task. The Telegram leg and the web app are two faces of the same core (both read `agent_outbox`/Postgres + notify fabric today; a real API seam lands with F1). The embedded dashboard stays as fallback while the split is in flight.
3. **Execution on gVisor/microVM** — every agent session runs sandboxed: **gVisor (runsc) first** (per-session user, network policy, zero ambient credentials — consistent with mergegate.go's no-ambient-gateEnv rule), **Firecracker-class microVMs as the scale-out path** on the ADR-0003 substrate (gVisor composes with k3s as a RuntimeClass; microVM is the later knob). ADR-0004's amendment made sandbox tech a config knob — Rev 2 picks its first value.

## MVP (fases; each = MAQ ticket + spec, state-machine-first)

- **F0 — Invite pilot (weeks, zero new infra).** 5–10 hand-invited users on the existing box: allowlist entries + one repo each, flash-class runner, BYO repo via deploy-key. Proves demand + measures real cost/task + breaks the single-user assumptions in the wild. *DoD: 3 external users ship ≥1 merged PR each; cost-per-task ledger (manual) exists.*
- **F1 — Tenant seam + onboarding.** Tenant column + scoping (state machine untouched), gVisor sandbox per session (Rev 2 tech; microVM later), GitHub App install + Telegram identity link, per-user audit log, dashboard split into the standalone web app (Rev 2 product shape), **per-user credentials vault — BYOK is a day-1 requirement (Rev 3)**: OpenRouter/z.ai/Claude keys or CLI subscription auth, injected only into the owning user's sandbox. *DoD: a stranger goes from repo URL to first PR without Otavio touching the box.*
- **F2 — Metering + billing.** Fixed platform subscription (Stripe) — **no token billing (Rev 3)**; usage ledger repurposed for infra-cost attribution (session wall-clock, storage, bandwidth) + quotas. *DoD: first $ collected; infra cost per active user measured within ±20% and the tier formula calibrated.*
- **F3 — Public launch.** Pricing ladder live, OTel cost tracing per task (ADR-0007 item), verdict guards + benchmark gate on reviewer quality, second runner tier GA. *DoD: public signup; weekly cohort retention reported.*

**MVP = end of F2** (chargeable); public at F3.

## Pricing

### Rev 3 — pricing model DECIDED (Otavio, 2026-10-09): BYOK + fixed platform subscription

- **BYOK only from the beginning.** Every session runs on the user's own model access: OpenRouter / z.ai / Anthropic keys — or their existing **subscriptions via runner CLI auth** (the runner seam already spawns `claude`/`codex`/`pi` CLIs, which authenticate natively against Claude Max / ChatGPT / z.ai coding plans). Per-user credentials live in a vault (F1) injected only into that user's sandbox.
- **We never bill tokens.** No credits, no pass-through, no margin on model spend.
- **We charge a fixed platform subscription (ARR-style tiers) over measured infrastructure cost.** Our COGS = session wall-clock on gVisor/microVM + storage (worktrees, Postgres) + bandwidth — fixed-ish and forecastable. That is what makes the price predictable for us as we scale AND for the user: their model bill arrives from their own provider, unchanged by us.
- **Tier numbers are a formula, calibrated by F0:** `tier price ≈ measured infra cost per active user × margin factor × concurrency allowance`. The F0 cost-ledger DoD is the calibration input; exact tiers land in the F2 pricing ADR. Free tier: BYOK with a small concurrent-session footprint cap.
- **Value framing:** the user pays for the platform — pipeline, review gates, self-healing, hard isolation, the app — never for tokens.

### Rev 4 — B2C only + price estimates (Otavio, 2026-10-09)

- **B2C only.** Individual developers on their own repos and their own model plans. No Teams/Enterprise tiers, no per-seat billing, no admin consoles — this product never sells to teams. The Teams rung of every earlier ladder (Rev 1's $40/seat, Copilot-Business-style anchors) is dead; Q1 below narrows accordingly.
- **Estimated COGS per active BYOK user** (pre-F0, order-of-magnitude): one Hetzner-class dedicated box ≈ $50/mo carrying 30–50 bursty gVisor sessions → ~$1.0–1.7/user in session infra, + ~$0.5–1.0 storage (worktrees, Postgres), + ~$0.5 bandwidth ≈ **$2–3.5 per active user/mo** at launch density, falling as box fill rises. F0's cost-ledger DoD replaces this estimate within ±20%.
- **Estimated tiers** — monthly, BYOK in all of them; tiers differ ONLY by concurrency, wall-clock quota and priority, never by model access:
  - **Free — $0** — 1 concurrent session, flash-class runner, ~20 sessions/mo, public repos. COGS ≈ $1–2/user, absorbed as CAC.
  - **Base — $9.90/mo (≈ R$49)** — 2 concurrent sessions, all runners, fair-use wall clock. ≈ 3–4× COGS at launch density. Anchor: Copilot Pro $10 (verified).
  - **Pro — $24.90/mo (≈ R$119)** — 3 concurrent, priority queue, long sessions, higher quota. Anchors: ChatGPT Plus $20 / Cursor Pro $20 — priced above them for concurrency + priority, below Devin-tier because the models are the user's bill, not ours.
  - Annual = 10× monthly on paid tiers.
- **Rule for the F2 pricing ADR:** price moves with measured wall-clock and concurrency, not model class. If a tier goes underwater, cut quota or price concurrency — never bundle tokens.
- Currency + rails (USD Stripe vs BRL Pix/Stripe-BR) decided at the F2 ADR; BRL figures above are psychological anchor points (~R$5/USD), not FX quotes.

### Superseded Rev 1–2 pricing analysis (bundled-token scenario — kept for the record)

**Cost per task arithmetic** (superseded Rev 3 for pricing; kept as the bundled-models scenario record — assumptions: ~80 agent+review turns, ~30k tokens/turn blended, 3:1 in:out → ~1.8M in + 0.6M out; unit prices from LiteLLM aggregator table fetched 2026-10-09 — cross-check against vendor pages before launch):

- Flash-class (GLM-4.6 proxy $0.60/$2.20 per M; our glm-5.3-flash likely cheaper — UNVERIFIED): ~$2.4/task upper bound, plausibly **$0.3–1.0** with flash pricing + prompt caching.
- Sonnet-class ($3/$15): 1.8×3 + 0.6×15 = **~$14.4/task** → a flat $20/mo unlimited-Sonnet plan is underwater at ~2 tasks. This is why Devin metered ACUs.
- Opus-class ($5/$25): **~$24/task** — premium runners must be metered or BYOK, never bundled flat.

**The margin law this implies:** *(superseded Rev 3 — applied to the bundled-models scenario only; BYOK removes the model-spend margin problem entirely, our bill is infrastructure.)* *bundle the cheap runner, meter the premium one.* Any flat plan must be priced on flash-class COGS with premium tasks burning credits or BYOK.

**Candidate ladder (feedback targets Q2/Q6 below):**

- **Free** — 3 tasks/mo, flash runner, public repos only. (COGS ≈ $1–3/user/mo.)
- **Pro $20/mo** — 30 flash-class tasks + BYOK unlimited (their keys, our orchestration); extra tasks $1 each. Anchor: Copilot Pro $10, ChatGPT Plus $20, Devin Pro $20 — all verified above.
- **Builder $49/mo** — 100 tasks + 5 sonnet-class tasks included, then credits; private repos, multi-repo.
- **Teams $40/seat/mo** — shared inbox topic, per-seat quotas, admin gates (anchor: Copilot Business $19, Devin Teams $80).

**Re-run procedure for the landscape tables (this ADR as snapshot):** refetch on official pricing pages (docs.github.com .md endpoints work; devin.ai + x.ai via r.jina.ai; anthropic/cursor/amp currently JS-walled — 2 attempts, then UNVERIFIED), quarterly or before any pricing decision, or after: a Devin/Anthropic/OpenAI Telegram launch, an openclaw hosted offering, or Copilot agent credit repricing. Feeds the F2 pricing ADR.

## Consequences

- Positive: maquinista's hardest-won machinery (gates, review, self-healing) becomes the moat exactly when chat-native agents are commoditizing; empty verified niche; owned-silicon deploy story (ADR-0004) keeps COGS defensible. **Rev 3:** BYOK removes model-COGS risk and vendor margin squeeze entirely — our cost base is infrastructure only, so a fixed price holds as we scale.
- Negative: multi-tenancy cuts across the state machine's assumptions (claimed_by identities, workspaces, souls) — F1 is the riskiest fase and needs its own ADR amendment; sandbox hardening is security work with real downside if rushed. **Rev 3:** BYOK adds onboarding friction (users need provider keys or CLI-authable subscriptions) and excludes plan-only users unless the runner CLIs' native subscription OAuth covers them.
- Neutral: Slack/etc. legs stay cheap to add later (architecture already fans out); OSS single-user edition needs a license decision (Q4).

## Still open (gates the fase ADRs — original 8 with Rev 2 status)

1. Target user first: prosumer devs ($20) vs teams ($40–80/seat)? — **partially answered Rev 4**: **B2C only** — individual developers; teams/enterprise excluded permanently. Remaining: persona + market (BR-first vs global), which gates currency/rails
2. Model bundling — **ANSWERED Rev 3**: BYOK only from day 1 (OpenRouter/z.ai/Claude keys or runner-CLI subscription auth); no bundled tokens, ever
3. MVP channel scope — **ANSWERED Rev 2**: Telegram-native wedge + our own web IDE/app (dashboard split); both legs from F1
4. License split (single-tenant OSS vs hosted closed) — **open**
5. Infra timing: barceloneta vs Hetzner substrate — **partially answered**: session isolation = gVisor now / microVM on ADR-0003 substrate; box/scale-out timing still open
6. Pricing shape — **ANSWERED Rev 3**: fixed platform subscription (MRR, B2C) over measured infra cost; no token/credit billing. **Rev 4 adds estimates**: Free / $9.90 / $24.90 (≈ R$49 / R$119) on COGS $2–3.5/user/mo, pre-F0. Exact tiers land in the F2 pricing ADR, calibrated by the F0 cost ledger
7. Product name — **open**
8. ToS/privacy floor before first charge — **open**

## References

- Landscape + pricing fetches: 2026-10-09 — code.claude.com/docs (llms.txt + slack.md + github-actions.md), developers.openai.com/codex/pricing, help.openai.com (Plus $20), docs.github.com/en/copilot/get-started/plans.md, devin.ai/pricing + docs.devin.ai (via r.jina.ai), x.ai /bot /news/team-bots /build (via r.jina.ai), docs.factory.com/delegations/slack.md, openhands.dev/pricing (RSC payload, ambiguous), api.github.com repo metadata + searches (~45 requests, ~20 domains; per-claim URLs inline above).
- Token unit prices: LiteLLM aggregator (raw.githubusercontent.com/BerriAI/litellm model_prices_and_context_window.json), fetched 2026-10-09; flagged aggregator-sourced, not vendor-primary.
- Internal evidence: greps stamped 2026-10-09 at `6afae8c` (tenant/billing absences, ALLOWED_USERS, runner config); ADR-0003/0004/0005/0006/0007/0008 as cited.
- UNVERIFIED carries-forward: Claude consumer tiers, Cursor, Amp, Jules, Devin ACU unit price, glm-5.3-flash exact pricing — each with its official URL above; verify at F2 pricing ADR time.
