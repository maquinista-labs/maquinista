# Run log — F0 pilot cohort rehearsal

- **Date:** 2026-10-15 (pre-flight; cohort round pending)
- **Task:** [MAQ-53] Pilot cohort rehearsal (execution): 2–3 external users ship real PRs end-to-end
- **Task ID:** `79c3b0b1-5df3-42c2-9c69-8e535488f09f` (Linear MAQ-53)
- **Runbook:** `docs/pilot-onboarding.md` (MAQ-49, PR #51) — audience: the operator
- **Purpose:** execute MAQ-49's DoD in the wild — 3 users × ≥1 merged PR,
  ledger numbers, and the breakage list that seeds the F1 tenant ADR.

## Pre-flight — gate audit at rehearsal time (runbook §0)

Performed 2026-10-15 ~02:20 by the implementor agent on barceloneta
(orchestrator + dashboard up since 02:07, Postgres 17 on :5434 — the
"box dark 10/10" park condition is lifted). Re-run mechanically anytime:
`scripts/rehearsal-gates.sh [runtime-.env]`.

| Gate | Prerequisite | Landed by | Status at audit | Evidence |
|---|---|---|---|---|
| P1 | Per-user repo binding | MAQ-45 (#52) | **RED — code landed, config inert** | `USER_REPOS` absent from runtime `.env`; `config.MayAccessRepo` treats unlisted users as unrestricted, so the guard does nothing until the operator adds `<tg-id>:<repo-root>` pairs and restarts (§2 step 3). |
| P2 | Per-user session cap | MAQ-46 (#54) | GREEN (default active) | `MAQUINISTA_MAX_AGENTS_PER_USER` defaults to 2 (`internal/config/config.go`); unset is valid. |
| P3 | Cost-per-task ledger | MAQ-47 (#53) | GREEN | `maquinista ledger --by user` runs clean against the live DB. |
| P4 | This runbook | MAQ-49 (#51) | GREEN | `docs/pilot-onboarding.md` present. |

**Cohort status at audit:** not started. `agent_inbox` shows exactly one
`origin_user_id` ever (817242359 = operator); the second allowlisted TG id
(6760585507) has never messaged the box.

**Action before first invite (operator):** set P1 — add to the runtime
`.env`, then `./maquinista stop && ./maquinista start`:

```
USER_REPOS=<tg-id-a>:/home/barceloneta/code/<repo-a>,<tg-id-b>:/home/barceloneta/code/<repo-b>
```

One user, one repo during F0 (runbook §2 step 3). `scripts/rehearsal-gates.sh`
must exit 0 before any user is invited.

## 1. Merged-PR table (runbook §5.1 — fill during the cohort)

DoD bar: **3 users × ≥1 merged PR each**.

| user | repo | PR | merged at | cost (ledger) |
|---|---|---|---|---|
| *(cohort pending)* | | | | |

## 2. Cost-per-task ledger snapshot (runbook §5.2)

Capture after the last cohort merge (F2 pricing input, ±20%):

```
maquinista ledger --by user --since 720h
maquinista ledger --by task --since 720h
maquinista ledger --by session --since 720h
```

| grain | paste of rollup output |
|---|---|
| *(cohort pending)* | |

## 3. What-broke list (runbook §5.3 — the F1 ADR input)

Format: `symptom → single-user assumption hit (§4 row or new) → workaround
used → F1 question it raises`. Completeness over severity.

Seeded from pre-flight observations:

- **Single gh identity confirmed** — box-wide `gh` login is
  `otaviocarvalho` (`gh auth status`); every cohort PR will be authored by
  it → §4 row 1 → none yet → does F1 need per-user git provenance or is
  PR-comment attribution enough?
- **Repo binding is config, not state** — P1 lives in `.env` + daemon
  restart; onboarding a user mid-cohort bounces the whole box (all agents'
  tmux panes die with the restart?) → §4 row 3 (flat allowlist) adjacent →
  workaround TBD during cohort → should USER_REPOS move to a DB table with
  hot reload (F1 tenant-seam question)?
- *(fill cohort incidents here, however small)*

## Offboarding (runbook §6)

After the cohort: remove cohort IDs from `ALLOWED_USERS`, kill their
agents (`maquinista kill <agent-id>`), archive workspaces, keep ledger
rows and this log — they are the F0 deliverable.
