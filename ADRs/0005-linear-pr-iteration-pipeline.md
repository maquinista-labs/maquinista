# ADR-0005: Linear-Driven PR Iteration Pipeline — tlc-spec-lean Execution via maquinista Agent Sessions

- **Status:** Aceito (2026-10-09, Otavio — de facto live: 43+ issues shipped through the pipeline since late Sep 2026)
- **Date:** 2026-10-01
- **Deciders:** Otavio
- **Scope:** Linear ↔ maquinista bridge (intake + board mirror), tlc-spec-lean execution contract for task agents, reviewer/arbiter/fixer/merger agent roles, PR review-loop state machine, merge path (rebase + GH PR merge)
- **Depends on:** existing task engine (migrations 001/003/011), merge queue (002), workspaces (028), A2A (018), Telegram relay; tlc-spec-lean skill contract (Tech Leads Club 1.0.0)
- **Bootstrap:** this pipeline's first customer is its own build — the EX tasks below are filed as the first Linear tickets and executed through the pipeline itself once EX-01/EX-02 land

## Context

Otavio wants agents that iterate on PRs autonomously: a Linear task gets picked
up, a spec is written in a worktree, a worker implements it, a PR is opened,
review agents judge it, changes-requested rounds loop until approved, and a
merger rebases and merges. Today that loop is hand-driven.

maquinista already owns most of the machinery — verified in-repo 2026-10-01:

- **Task state machine**: `tasks.status` runs `pending → ready → claimed →
  review → done | failed`, plus `pending_approval` (human approval gate with
  dependency cascade) — migration 001 + 003 (`internal/db/migrations/003_levante.sql:37-59`);
  `worktree_path` / `pr_url` / `pr_state` columns exist since migration 011
  (`011_task_pipeline.sql:5-28`). `internal/prompt/prompt.go:50` enforces the
  `maquinista-done` completion protocol (worktree mode auto-commits).
- **Merge queue** (migration 002, `cmd/maquinista/cmd_merge.go:81-118`):
  `ClaimMergeEntry` → `git.MergeNoFF` → on `ConflictError` abort + record
  conflict files + task observation → `CompleteMerge` → worktree removal.
  Local-only today: no rebase on `origin/main`, no GitHub PR merge, no CI wait.
- **Workspaces**: task-scoped git worktrees (`task/<task-id>` branches, lazy
  `git.WorktreeAdd`, restart-safe) — `arch/workspaces.md`; cleanup-after-merge
  already implemented in the merge path (`cmd_merge.go:113`).
- **Orchestrator engine** (`arch/orchestration.md`): polls `pending` tasks,
  claims, spawns a subtask agent, injects the task body as first inbox message.
- **A2A** (`internal/a2a/`): synchronous agent↔agent request/response +
  subagent spawn with delegation permission and depth caps — the dispatch
  seam for reviewer/verifier roles.
- **Human-in-the-loop**: `agent_outbox` → relay → Telegram topics; replies
  land back in `agent_inbox` → sidecar → pane (`arch/messaging.md`).

tlc-spec-lean defines the execution contract each task agent will follow:
`PLAN → CHECKS → BUILD → VERIFY` with artifacts in
`.specs/features/<feature>/{plan,checks,verification}.md`, validators
(`validate_plan/checks/verification.py`), and an independent verifier that is
never the author (rule 5). Precedent in this repo: `.specs/features/pi-integration/`.

Linear ground truth, verified via GraphQL API 2026-10-01: the API key in
circulation is **Adriana's account** (viewer = Adriana Höher Dorneles) and the
workspace holds exactly **one team, BRI (BRISAAI)** — the Brisa product board.
The pipeline needs its own team + custom workflow states so board automation
never touches the product board. Linear's state *types* are only six
(triage/backlog/unstarted/started/completed/canceled); the mid-flow columns
are named custom states of type `started` — supported, but they must be created.

Gap on the box: the barceloneta checkout's origin is HTTPS with **no stored
git credentials, and `gh` is not installed** (verified by ssh 2026-10-01) —
`git push` from the box fails today. PR creation and merge therefore need a
one-time provisioning step (EX-00) before any worker can open a PR.

## Frozen obligations (EARS — the pipeline contract)

These freeze *what must be true*; the per-task *how* stays with the
tlc-spec-lean plans derived from them (one `.specs/features/` plan per EX task).

1. WHEN a Linear issue enters the MAQ team's Todo state THEN the poller SHALL
   create a maquinista task row and move the issue to In Progress within 60 s.
2. WHEN a worker claims a pipeline task THEN it SHALL work in the task-scoped
   worktree on branch `task/<linear-id>` and its first artifact SHALL be
   `.specs/features/<feature>/plan.md` passing `validate_plan.py`.
3. WHEN the plan passes and the user approved checks derivation THEN the worker
   SHALL write `checks.md` passing `validate_checks.py` and implement until
   every proof is green before signaling completion.
4. WHEN the worker completes THEN the system SHALL push the branch, open a PR
   titled with the Linear identifier, set `tasks.status='review'`, and move the
   issue to In Review.
5. WHEN a PR is in review THEN the system SHALL dispatch a reviewer session
   whose author has zero commits on that branch (tlc rule 5), which SHALL
   review the Linear task, plan.md, checks.md, verification.md and the full
   diff, and return a verdict of `approve`, `request_changes`, or `needs_human`.
6. WHEN the verdict is `request_changes` THEN the issue SHALL move to Changes
   Requested and a fresh fixer session SHALL resume the SAME worktree and PR.
7. WHEN a task exceeds 3 review rounds THEN the system SHALL park it in Needs
   Human (`pending_approval`) with a Telegram summary of both positions.
8. WHEN the verdict is `approve` THEN the task SHALL enter the merge queue
   (Linear → Ready to Merge); the merger SHALL rebase onto `origin/main` and
   squash-merge the PR once CI is green.
9. WHEN the rebase conflicts THEN the merger SHALL either resolve via a merger
   agent session or park the task in Needs Human with the conflict file list
   (the queue already records conflicts — `cmd_merge.go:88-97`).
10. WHEN `PIPELINE_AUTO_MERGE=0` (the v1 default) THEN the merger SHALL post a
    merge proposal to Telegram and wait for `maquinista approve <task>` before
    merging.
11. WHEN the merge completes THEN the issue SHALL move to Done, the worktree
    SHALL be removed, and a merge note SHALL post to the pipeline Telegram topic.

## Options Considered

| | A. Native maquinista pipeline (reuse task engine + new Linear bridge + review souls) | B. GitHub Actions-centric (Linear webhook → GH workflows → agents on GH runners) | C. Standalone pipeline daemon (Python + tmux, outside maquinista) | D. Status quo + helper scripts (no review agents) |
|---|---|---|---|---|
| Reuses existing state machine | full (tasks/merge_queue/pending_approval) | none — rebuilds in GH | partial | full |
| Board mirror (Linear) | new bridge, one seam | native-ish (GH Projects instead) | custom | manual |
| Worktree isolation | existing (`task/<id>`) | GH runner checkout | reimplemented | manual |
| Human-in-the-loop | Telegram relay (exists) | GH comments only | custom bot | manual |
| Autonomous review loop | reviewer/arbiter/fixer souls | custom actions, model auth in GH secrets | custom | none |
| Effort | ≈ 6–7 dev days | ≈ 8–10 days + secrets migration | ≈ 10+ days, second system of record | 1–2 days, doesn't meet the ask |

Decision driver: the ask is "maquinista as the trigger to its agent sessions" —
the trigger (orchestrator engine), the sessions (tmux runners + monitor), the
isolation (worktrees), the loop guard (pending_approval) and the queue (merge_queue)
all exist. Option A adds one bridge and three roles; everything else re-creates
machinery maquinista already runs in production.

## Decision

**Option A.** Linear is the human-facing board; Postgres remains the system of
record; tlc-spec-lean is the execution contract inside every worker session.

### Architecture

```
Linear MAQ team                maquinista (barceloneta)                 GitHub
Todo ──poll──▶ linear-bridge ─▶ tasks row (pending) ─▶ orchestrator
In Progress ◀─mirror─┐          │ claim → spawn worker pane
                     │          │   task-scoped worktree, tlc-spec-lean
                     │          │   plan → checks → build → verify(a2a)
In Review ◀──────────┤          ▼ PR opened → status='review'
                     │        reviewer session (fresh) → arbiter verdict
Changes Requested ◀──┤          ├─ request_changes → fixer session (same
                     │          │   worktree/PR) → back to review
Needs Human ◀────────┤          ├─ needs_human → pending_approval + TG
Ready to Merge ◀─────┤          └─ approve → merge queue
Done ◀───────────────┘            merger: rebase origin/main → gh pr merge
                                  (--squash, CI green) → worktree rm
```

- **linear-bridge** (new `internal/pipeline/`): poller (60 s) claims Todo
  issues (label `pipeline`) by inserting the task row first — the row is the
  single-owner claim — and a `linearSync` goroutine mirrors every Postgres
  state transition back to Linear with retry. Agents never call Linear
  directly; the determinism boundary is the state machine.
- **Roles as soul templates** (seed migration in the 028 style):
  `pipeline-worker` (tlc-spec-lean contract, default runner pi),
  `pipeline-reviewer` + `pipeline-arbiter` (high-reasoning runner; reviewer is
  dispatched fresh per round, never the author), `pipeline-fixer`,
  `pipeline-merger`. Dispatch via the orchestrator engine and A2A.
- **Round accounting**: `review_rounds INT DEFAULT 0` (new migration) —
  incremented on every request_changes; ≥ 3 → `pending_approval` (Needs Human).
  `pending_approval` already cascades to dependents (migration 003) and has an
  approve verb (`maquinista approve`, `cmd_approve.go`) — the human unblock path.
- **Merge path**: extend `processMerge` with a GH mode (`PIPELINE_MERGE_MODE=gh`):
  `git fetch origin` → rebase branch on `origin/main` in the worktree →
  `gh pr merge --squash` gated on `gh pr checks` green. The existing local
  `MergeNoFF` mode stays for repos without PR flows. Conflict on rebase →
  spawn merger agent; unresolvable → Needs Human with conflict files.
- **Spec visibility**: plan.md ships inside the PR, so the tlc human-stop moves
  to PR review; tasks labeled `plan-gate` instead stop at `pending_approval`
  right after the plan for Otavio to read before any build.
- **Verification**: `verification.md` written by an independent verifier session
  (A2A subagent) is a hard precondition for the PR-open signal; default tlc
  profile `light`, label `profile:standard` escalates.

### Implementation tasks

- **EX-00 Provisioning (0.5 d)**: create Linear team MAQ ("Maquinista") +
  custom states (In Progress, In Review, Changes Requested, Needs Human,
  Ready to Merge) + labels (`pipeline`, `plan-gate`, `profile:standard`);
  install `gh` for service user `barceloneta`, auth via `GH_TOKEN` from the
  box `.env`, verify `git push` + `gh auth status` from the box.
- **EX-01 linear-bridge (1–1.5 d)**: poller, claim, `linearSync` mirror, migrations.
- **EX-02 Role souls (1 d)**: four templates + prompt contract (validators,
  `maquinista-done`, verdict vocabulary), worker spec-first enforcement.
- **EX-03 Review dispatch (1 d)**: on `review`, spawn reviewer (zero-author
  check), verdict parsing → transitions + Linear mirror + `review_rounds`.
- **EX-04 Fixer loop (1 d)**: re-claim Changes-Requested tasks into the same
  worktree/PR; round cap → Needs Human.
- **EX-05 GH merge mode (1 d)**: rebase + `gh pr merge --squash` + CI gate +
  auto-merge flag + conflict→agent path.
- **EX-06 Telegram plumbing (0.5 d)**: pipeline topic, verdict summaries,
  merge proposals, needs-human questions (relay exists; wire content).
- **EX-07 Pilot (1 d)**: one real task end-to-end through the loop — filed as a
  Linear ticket and executed BY the pipeline (dogfood; first falsification of
  the loop-convergence assumption).

**Effort estimate: ≈ 6–7 dev days total.** Load-bearing assumption to falsify
first (EX-00 + EX-07): the review-agent loop converges — bad specs get caught
in round 1–2 instead of burning rounds, and the box can push PRs at all.

### Consequences

- **Positive:** autonomous task→PR→merge with board truth in Linear and system
  truth in Postgres; review discipline (independent verifier, zero-author
  reviewer) rides tlc-spec-lean's existing gates; human-in-the-loop reuses the
  Telegram relay and `pending_approval`; merge queue gains rebase/CI awareness
  without abandoning its conflict ledger; the pipeline builds itself (bootstrap).
- **Negative:** review rounds are token-expensive (high-reasoning arbiter per
  round — the round cap is also a cost cap); polling adds ≤ 60 s intake latency
  (Linear webhooks would need a public endpoint the box doesn't have); merge
  authority delegated to agents even behind the flag — a bad merge is possible
  until proven otherwise; one more seed migration of souls to maintain.
- **Neutral:** v1 targets the maquinista repo only; other target repos are a
  config mapping added later. ADR-0002/0003/0004 are orthogonal — the pipeline
  rides the default tmux executor and moves to Substrate sandboxes with it.

### Revisit Triggers

- A bad merge ships → flip default profile to `standard`, lower the round cap,
  keep `PIPELINE_AUTO_MERGE=0` until two clean weeks pass.
- Linear webhook availability or a tunnel the team accepts → replace polling.
- Second target repo onboards → generalize repo mapping beyond maquinista.
- Runner/harness churn (pi/dsh) → souls are runner-agnostic; re-bind per role.

## References

- Linear GraphQL, verified 2026-10-01: viewer = Adriana Höher Dorneles; teams = [BRI] only (list-teams) — motivates the dedicated MAQ team
- `internal/db/migrations/001_initial.sql` (tasks), `002_worktrees_and_merge_queue.sql` (merge_queue), `003_levante.sql:37-59` (pending_approval cascade), `011_task_pipeline.sql:5-28` (pr columns + review state), `028_seed_default_agents.sql` (soul seeds)
- `cmd/maquinista/cmd_merge.go:81-118` (MergeNoFF + ConflictError ledger), `cmd_approve.go` (pending_approval → ready)
- `arch/orchestration.md`, `arch/workspaces.md`, `arch/messaging.md` (read 2026-10-01)
- `internal/prompt/prompt.go:50` (`maquinista-done` completion protocol)
- tlc-spec-lean skill v1.0.0 (Tech Leads Club) — PLAN/CHECKS/BUILD/VERIFY contract, validator gates, independent-verifier rule
- Box probe 2026-10-01 (ssh): `gh` not installed, `gh auth` absent, origin HTTPS `github.com/maquinista-labs/maquinista.git` — motivates EX-00
- `.specs/features/pi-integration/` — in-repo precedent for the artifact layout
