# EX-07 pilot round — plan

## Goal

Run ONE real task end-to-end through the ADR-0005 pipeline (dogfood):
Linear ticket → task claim → worker spawn in a worktree → spec artifacts →
PR → review loop. First falsification of the loop-convergence assumption.

## Scope

- Exactly ONE new deliverable file: `docs/run-logs/2026-10-02-pilot-ex07.md`
  — a short run log (date, task, one line per pipeline stage exercised).
- This spec pair (`.specs/features/pilot-ex07/plan.md` + `checks.md`),
  required by the pipeline-worker soul contract, written by hand.
- Git/PR mechanics on branch `pilot-ex07`: commit → push → `gh pr create`
  (base `main`) → `tasks set-pr` → `maquinista-done` → `mark-review`.

## Environment deviations (noted per pilot instructions)

- This box does NOT have the tlc-spec-lean validators (`validate_plan.py`,
  `validate_checks.py`, `check_commit.py`). They are skipped for this round;
  `checks.md` uses a single command-exit proof instead.
- `maquinista` CLI: not on `PATH`, no `context` subcommand — absolute binary
  path used; observation rows go to `task_context` via `psql` directly.

## Non-goals

- Touch nothing else: no source changes, no migrations, no config.
- No infra operations (systemctl / orchestrator / tmux / database restarts)
  — the pane runs under the orchestrator.

## Risks

- The deployed bash `scripts/maquinista-done` sets `status='done'`
  unconditionally; the ticket-aware `db.MarkDone` → `'review'` branch
  (documented in `internal/pipeline/dispatch.go`) is not wired to a CLI.
  Mitigation: `tasks mark-review` (idempotent safety net) runs after
  `maquinista-done` so the reviewer dispatch finds `status='review'`.
