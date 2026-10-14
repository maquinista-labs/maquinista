# F0 Pilot Onboarding Runbook — external users on the existing box

> Task: [MAQ-49](https://linear.app/brisaai/issue/MAQ-49/pilot-cohort-rehearsal-2-3-external-users-ship-real-prs-on-the)
> (ADR-0009 F0). Audience: the operator (Otavio) onboarding 2–3 hand-invited
> users onto **barceloneta**, the existing box. No sandbox: the trust boundary
> is the allowlist (known people), not gVisor — isolation is F1 and
> explicitly parked.

## 0. Prerequisite status board — do not onboard until green

Every row must be verifiably landed *before* the first external user sends a
message. Onboarding without them re-creates the single-user assumptions F0
exists to surface.

| # | Prerequisite | Guards | Landed by | Verify |
|---|--------------|--------|-----------|--------|
| P1 | **Per-user repo binding** — a user's chats may only spawn agents/worktrees for their own repo | cross-repo access | MAQ-45 | send a message referencing another user's repo from a bound user → routing rejection, no spawn |
| P2 | **Per-user session cap** — bounded live agents per user (default N=2) | runaway spawn on shared silicon | MAQ-46 | open N+1 topics / agents as one user → N+1th spawn rejected with a readable message |
| P3 | **Cost-per-task ledger** — rollup of `agent_turn_costs` + turn-end events per user/task | F2 pricing calibration input | MAQ-47 | ledger query returns non-zero rows after a smoke task |
| P4 | **This runbook** — onboarding steps + rehearsal DoD tracker | execution drift | MAQ-49 (this file) | operator has executed §2 once end-to-end |

P1–P3 are in flight as separate tasks; re-check this table at rehearsal time.
If any is unlanded, **fix that first** — do not onboard around a missing
guard.

## 1. What the box gives a pilot user (mechanism inventory)

| Capability | Mechanism | Where configured |
|---|---|---|
| Telegram gate | `ALLOWED_USERS` / `ALLOWED_GROUPS` (comma lists of numeric IDs), enforced by `bot.isAuthorized` before any routing | `.env`, reloaded on daemon restart |
| Agent identity | souls (`agent_souls`, templates incl. `default`) rendered at spawn | `maquinista soul template …` |
| Topic → agent | tier-3 spawn: first message in a fresh forum topic creates agent `t-<chatID>-<threadID>` + owner binding | automatic (`arch/routing.md`) |
| Dashboard path | dashboard-spawned agent, then `/agent_default @handle` binds it to a topic | `routing.SetUserDefault` |
| Runner class | flash-class via `MAQUINISTA_DEFAULT_RUNNER=pi` + `MAQUINISTA_PI_MODEL=zai/glm-5.3-flash` | `.env` (box-wide) |
| Workspaces | `scope=agent` worktrees `.worktrees/<agent-id>/` keep parallel agents off each other's branches | `maquinista agent ws …` (`arch/workspaces.md`) |
| Cost capture | monitor writes `agent_turn_costs` at model rates (`model_rates`) per turn | automatic (`internal/monitor/cost.go`) |
| PR egress | box-wide `gh` login + shared runner creds | host-level (see §4) |

## 2. Per-user onboarding checklist

Repeat for each invited user. Steps marked **[operator]** touch the daemon or
host; steps marked **[verify]** are the acceptance check for that step.

1. **[operator] Collect the Telegram user ID.** Have the user message the bot;
   the unauthorized reply / bot log yields their numeric ID.
   (`ALLOWED_GROUPS` stays untouched — pilot users work in their own DM /
   private topics, not shared groups.)
2. **[operator] Allowlist the user.** Append the ID to `ALLOWED_USERS` in
   `.env`, then restart so config reloads:
   `./maquinista stop && ./maquinista start`.
   **[verify]** user DMs the bot and is no longer rejected by the gate.
3. **[operator] Bind the user to exactly one repo** (P1, MAQ-45 mechanism).
   Record the mapping in the onboarding log (§5). Do not hand out second
   repos during F0 — one user, one repo keeps the rehearsal legible.
   **[verify]** from the user's topic, an agent asked to touch repo B is
   rejected by the routing seam, not silently allowed.
4. **[operator] Provision the first topic + agent.** Simplest: create a
   private forum topic per user; their first message tier-3-spawns
   `t-<chatID>-<threadID>` with the default soul and the flash-class runner.
   If identity matters, spawn from the dashboard first, set a handle, and
   bind with `/agent_default @handle`.
5. **[verify] Session cap (P2).** Spawn until rejection: the (N+1)th live
   agent for this user must bounce with a readable message. Kill the extra
   panes afterwards (`maquinista kill <agent-id>`).
6. **[verify] Smoke task through the full path.** User asks for one small
   change in *their* repo; expect: agent row (`status=running`, runner
   `pi`), worktree under the user's repo, a PR opened against that repo, and
   a non-zero `agent_turn_costs` row within a turn of the reply.
7. **[verify] Ledger (P3).** Run the MAQ-47 rollup verb for this user/task;
   record the numbers in the rehearsal log (§5).
8. **[operator] Record onboarding** in the rehearsal log: user label, TG id,
   repo, topic id, agent id, date.

Onboarding is complete for a user when steps 2–7 are all green. The user is
now shipping real PRs as F0 requires.

## 3. Runner class and what it costs

F0 runs every pilot agent on the flash-class runner
(`pi` + `zai/glm-5.3-flash`). That choice is load-bearing:

- COGS target ≈ $1–2/user (ADR-0009 Rev 4); the ledger (P3) replaces the
  estimate with measured numbers.
- Model keys are **box-wide** (`ZAI_API_KEY`): per-user spend is computed
  token-side from `agent_turn_costs` × `model_rates`, not at the provider.
- Per-user model overrides / BYOK are F2 — do not promise them to pilot
  users.

## 4. Known single-user assumptions to watch (seed of the F1 breakage list)

The rehearsal's DoD includes "a written list of what broke". These are the
assumptions we already know are single-user-shaped — confirm or refute each
in the wild and log the incident, however small:

- **One GitHub identity.** Every PR from every user is authored by the box's
  `gh` login (`otaviocarvalho`). Git provenance is lost for the cohort; PR
  review commentary must carry the pilot user's name manually.
- **Shared runner credential namespace.** Runners share `~/.claude` and one
  transcript root — the 30/09–01/10 incident (two panes, one transcript) is
  the same failure class; watch for cross-user transcript bleed.
- **Flat allowlist.** `ALLOWED_USERS` grants the Telegram gate only; before
  P1 lands it implicitly grants *everything*. Repo binding is the only
  fence, and it is routing-level, not filesystem-level.
- **Same-UID filesystem.** All agents run as one OS user: a pilot user's
  agent can read another user's worktree under `.worktrees/`. Acceptable for
  known people (F0 premise), but log every case where it *mattered*.
- **Operator-global surfaces.** Dashboard sessions, `/plan`, audit log,
  souls and the task board are shared. A pilot user with dashboard access
  sees the operator's world.
- **One queue, no fairness.** The task scheduler and merge queue are
  box-global; one user's burst can starve the others (no per-user
  concurrency beyond P2's cap).
- **Absolute `worktree_path` / single-box assumption.** Task rows point at
  barceloneta paths; nothing about F0 generalizes to a second box (that is
  ADR-0003/0004 territory).

## 5. Rehearsal DoD tracker (fill during the cohort)

Write one run log per cohort round in `docs/run-logs/<date>-f0-rehearsal.md`
(house style: see `2026-10-02-pilot-ex07.md`), covering the three DoD items:

1. **Merged-PR table** — one row per DoD unit:
   `| user | repo | PR | merged at | cost (ledger) |`
   DoD bar: **3 users × ≥1 merged PR each**.
2. **Cost-per-task ledger snapshot** — paste the MAQ-47 rollup output per
   user/task/session (wall-clock seconds + token cost at current rates).
   This is the F2 pricing-calibration input (±20%).
3. **What-broke list** — every deviation, however small, in the format:
   `symptom → single-user assumption hit (§4 row or new) → workaround used →
   F1 question it raises`. This list is the *input* to the F1 tenant-seam
   ADR; completeness matters more than severity.

## 6. Offboarding

1. Remove the ID from `ALLOWED_USERS` (`.env`) and restart the daemon.
2. `maquinista kill <agent-id>` for the user's live agents; owner bindings
   for their topics can then be deleted without respawn races.
3. Archive workspaces / worktrees for the user's agents
   (`maquinista agent ws archive`); delete the worktree dir when the PRs
   have merged.
4. Keep the ledger rows and the run log — they are the F0 deliverable.

## Non-goals (explicitly parked)

- gVisor / sandbox isolation — F1 tenant-seam ADR; F0's boundary is the
  allowlist plus P1/P2 guards.
- Quotas, billing, per-user pricing — F2 pricing ADR (needs this rehearsal's
  ledger numbers).
- BYOK model keys, per-user gh identities, second box — all post-F1.
