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
| P1 | Per-user repo binding | MAQ-45 (#52) | **GREEN — wired 19/10** | `USER_REPOS=817242359:/home/barceloneta/code/maquinista` in runtime `.env` (prior config backed up to `.env.bak-20261019-p1`); daemon restarted 18:40 UTC; `scripts/rehearsal-gates.sh` exit 0 (P1–P4 all PASS). Unlisted users stay unrestricted by design (`config.MayAccessRepo` fail-open) — cohort users 2–3 get their pair at onboarding (§2 step 3). |
| P2 | Per-user session cap | MAQ-46 (#54) | GREEN (default active) | `MAQUINISTA_MAX_AGENTS_PER_USER` defaults to 2 (`internal/config/config.go`); unset is valid. |
| P3 | Cost-per-task ledger | MAQ-47 (#53) | GREEN | `maquinista ledger --by user` runs clean against the live DB. |
| P4 | This runbook | MAQ-49 (#51) | GREEN | `docs/pilot-onboarding.md` present. |

**Cohort status at audit:** not started. `agent_inbox` shows exactly one
`origin_user_id` ever (817242359 = operator); the second allowlisted TG id
(6760585507) has never messaged the box.

**P1 wired 2026-10-19 (operator):** cohort user 1 = operator
(817242359 → `maquinista` repo; one user, one repo per §2 step 3). The same
restart folded in the #56 deploy (`eca415c` → `0f6230b`, MAQ-54 liveness
belt). Gates green post-restart. Post-heal
(same day, see what-broke below): zombie respawns killed, user 1 = 1 live
agent (dashboard) — cap 1/2, ready for §2 steps 5–6.

**DONE 2026-10-19 (see P1 wired above).** Original instruction — set P1:
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
- **CONFIRMED 19/10: a daemon restart culls idle agent panes** — the P1
  wiring restart took 9 live panes down to 1 (4 pi + 4 claude panes gone, 1
  respawned fresh by reconcile, 8 stale window bindings cleared, "Recovery:
  0 live bindings"); zero tasks running/claimed at the moment, so no
  in-flight work was lost → §4 row 3 (flat allowlist) adjacent, plus the
  "repo binding is config, not state" seed above → workaround: restart only
  when no task is in flight (what this restart did) → should USER_REPOS move
  to a DB table with hot reload (F1 tenant-seam question), and should
  reconcile respawn idle panes or is the cull correct?
- **Boot reconcile respawns stale `running` topic rows → operator capped out
  at 8/2 before the cohort started** (CONFIRMED 19/10, post-P1-restart): the
  18:40 cull took 9 panes down, but boot reconcile respawned all 7 `t--*`
  topic agents (idle since 30/09–02/10) plus the dashboard agent within
  ~2.5 min. `CountLiveAgentsForUser` counts every live-status row, so user 1
  sat at 8 live vs cap 2 — the §2 smoke-task spawn in a fresh topic would
  have bounced on a cap measuring zombies. → §4 row 3 adjacent; **answers
  this log's own seeded question** ("should reconcile respawn idle panes or
  is the cull correct?"): with cap counting live, respawn of stale rows is
  actively harmful. → workaround: operator state heal — `maquinista kill` ×7
  zombie topic agents (verified none owned tasks first), cap back to 1/2.
  → F1 question: freshness floor on boot-respawn (only rows with recent
  `last_seen`), and/or cap counting on heartbeat (`last_seen` within N min)
  instead of raw status?
- **Boot reconcile double-spawned one topic → orphan pane sharing the window
  name** (19/10 18:42): `t--1003896184686-11213` got window @8 from the
  reconcile and a second spawn created @9 18 s later; the agents row pointed
  at @9, leaving @8 an orphan `pi` process indistinguishable by name.
  `maquinista kill` cleaned the row but BOTH panes survived it — window-by-
  name resolution cannot disambiguate a duplicate — manual `tmux
  kill-window` ×2 finished the job. → §4 row 2 exactly (shared namespace →
  "two panes, one name", the 30/09–01/10 incident class). → workaround:
  manual tmux surgery. → F1 question: enforce unique window names (collision
  suffix), and `kill` should fall back to pane-id targeting when a name
  resolves to >1 window.
- *(fill cohort incidents here, however small)*

## Offboarding (runbook §6)

After the cohort: remove cohort IDs from `ALLOWED_USERS`, kill their
agents (`maquinista kill <agent-id>`), archive workspaces, keep ledger
rows and this log — they are the F0 deliverable.