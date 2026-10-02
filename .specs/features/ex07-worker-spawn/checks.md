# EX-07 Worker spawn wiring — checks

Run: `make vet && go build ./... && go test ./internal/taskscheduler/
./internal/pipeline/` (db-backed tests need `make up`).

## Wiring

- [ ] `orchestrator start` runs `taskscheduler.Run` with
      `EnsureAgent: ensureTaskWorker(pool, cfg, sidecarMgr)` in the
      tickets-enabled block, and logs `task-scheduler: started`.
- [ ] `ensureTaskWorker` returns `errNoSidecars` when the sidecar manager
      is nil (pane without inbox goroutine can never receive the prompt).
- [ ] Worktree guard: nil/empty `tasks.worktree_path` → error naming the
      task; unstat-able path → error wrapping the stat failure; DispatchOne
      reverts the task to `ready` either way.

## Frozen contracts surfaced

- [ ] `pipeline.WorkerSoulTemplate == "pipeline-worker"` (migration 035
      seed id) and `pipeline.WorkerRole == "implementor"` (DispatchOne
      default).
- [ ] `MintWorkerID` mints `<role>-<taskID>[-rN]`, skipping ids present in
      `agents` (same shape as the reviewer mint).
- [ ] `ResolveWorkerExec` resolves the pipeline-worker extras
      (`default_runner` → RunnerType, reasoning_class → ModelOverride);
      empty keys resolve to empty strings (SpawnFresh falls back to
      `cfg.DefaultRunner`).

## Spawn chain

- [ ] SpawnFresh receives Role + TaskID (role-scoped live-index semantics,
      EX-04 pitfall f) and the pipeline-worker SoulTemplateID.
- [ ] Agent row lands with `status='running'`, bound `task_id`, and a
      sidecar inbox goroutine is started (step 8 of SpawnFresh).

## Scheduler behavior (existing, regression-guarded)

- [ ] `go test ./internal/taskscheduler/` green — claim/flip/enqueue/heal
      contract unchanged by the wiring.
- [ ] `go vet ./...` clean; `gofmt -l` clean on files authored in this PR.

## Live pilot (manual, rides the deployed stack)

- [ ] C7: a `ready` pilot task is claimed within one poll interval (30 s):
      status `ready` → `claimed`, agent `<role>-<taskID>` row
      `running`, tmux window open in the task worktree, `/work-on-task`
      inbox row enqueued, `tasks.claimed_by` set.
- [ ] C8: worker output reaches the task's Telegram topic via the relay.
- [ ] C10 (EX-06 follow-up): pipeline notification (verdict/merge outcome)
      lands in the Pipeline topic.
