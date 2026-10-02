# EX-07 Worker spawn wiring — plan

## Goal

Claim loop → real panes. The task-scheduler exists and is tested
(`internal/taskscheduler`), the pipeline-worker soul is seeded (migration
035), but nothing in `orchestrator start` runs the scheduler and its only
EnsureAgent is the row-only stub behind the standalone subcommand. A `ready`
task therefore sits unclaimed forever (observed live 02/10: pilot task
stayed `ready` with 0 agents across 6 one-minute polls).

## Scope

- `internal/pipeline/worker.go` (new): `WorkerSoulTemplate`,
  `WorkerRole`, `MintWorkerID`, `ResolveWorkerExec` — the frozen EX-02
  contracts surfaced for the cmd side without a cross-package dependency on
  unexported dispatch helpers.
- `cmd/maquinista/pipeline_worker.go` (new): `ensureTaskWorker` — the
  production `taskscheduler.EnsureAgentFn`. Worktree guard → mint →
  `ResolveWorkerExec` (soft-fail to defaults) → `agentspawn.SpawnFresh`
  (full chain: row, soul clone, memory seed, tmux window, sidecar inbox
  goroutine), mirroring `pipelineReviewerSpawner`.
- `cmd/maquinista/cmd_start.go`: start `taskscheduler.Run` in the
  tickets-enabled block, next to review dispatch.
- `arch/pipeline.md`: "Task scheduler (EX-07)" section.

## Non-goals

- No new migration (035 already seeds the soul; scheduler tables exist).
- No reaper for `claimed` tasks with dead agents (scheduler comment
  reserves it; `uq_agents_task_live` releases naturally when the agent row
  dies).
- The standalone `task-scheduler` subcommand keeps its stub.
- No unit test for `ensureTaskWorker` itself: it is a thin adapter whose
  every dependency (DispatchOne contract, SpawnFresh chain, mint shape,
  exec resolution) is already covered by package tests; the proof is the
  live pilot round (EX-06's C10 rides it).

## Risks

- ResolveWorkerExec soft-fail: a broken soul contract spawns workers on
  cfg.DefaultRunner instead of wedging tasks — deliberate asymmetry vs
  review dispatch (workers are replaceable, reviewers gate verdicts).
- EnsureAgent failure after the claim commit: DispatchOne reverts to
  `ready` (existing behavior); worktree guard turns the common case
  (missing path) into a readable log line.
