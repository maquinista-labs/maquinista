# EX-03: Review dispatch — reviewer spawn, verdict parsing, transitions

ADR-0005 "EX-03 Review dispatch (1 d): on `review`, spawn reviewer (zero-author
check), verdict parsing → transitions + Linear mirror + `review_rounds`."

## Summary

Pipeline tasks currently die at the worker: `maquinista-done` flips them to
`done` (generic path), nobody reviews the diff, and the board shows Done before
any human-quality gate. This task closes the loop's first half: a pipeline task
completes into `review` (not `done`), a dispatch loop spawns a fresh reviewer
agent (zero-author, `pipeline-reviewer` soul, runner/model resolved from the
frozen EX-02 extras contract), watches its outbox for the contract verdict
line, and transitions the task (`ready_to_merge` / `changes_requested` /
`pending_approval`). The Linear mirror stays purely derived — no
`pending_state` writes are needed once the new statuses map to canonical
columns.

## In scope

- done-path branch for pipeline tasks (`metadata->>'ticket_issue_id'` set)
- `RunDispatch` loop: dispatch pass (spawn reviewers) + verdict pass
  (parse + transition) + stall watchdog
- zero-author check (reviewer ≠ completing worker, structurally + explicitly)
- `review_rounds` accounting at each reviewer spawn
- new task statuses `changes_requested`, `ready_to_merge` + `DerivedState`
  mappings
- reviewer spawn via `agentspawn.SpawnFresh` (extended with role/task binding)
- `runner`/`model` resolution from template extras (`default_runner`,
  `reasoning_class`)
- wiring in `cmd start` next to bridge/sync (same enable gate)
- `arch/pipeline.md` Dispatch section

## Out of scope

- Fixer loop (re-claim of `changes_requested`) — EX-04; this task only lands
  the status + `review_rounds` it will consume. Round cap → Needs Human is
  EX-04.
- Merge execution (`gh pr merge`) — EX-05; `ready_to_merge` is the terminal
  status EX-03 produces.
- Arbiter adjudication of contested verdicts — EX-04+ (soul exists; no
  dispatch path yet).
- Telegram verdict summaries — EX-06.
- Any soul template edits — souls stay runner-agnostic; dispatch READS extras.
- Provider code — dispatch never talks to the ticket system; the sync loop
  already mirrors derived statuses.

## Decisions

1. **Done-path branch in SQL, not a post-hoc flip.** The completing tx gains
   `status = CASE WHEN metadata->>'ticket_issue_id' IS NOT NULL THEN 'review'
   ELSE 'done' END`. A post-hoc flip (loop notices `done` pipeline tasks)
   races the 10 s sync into pushing Done to the board. One atomic UPDATE, no
   race, non-pipeline tasks byte-identical behavior.

2. **Derived statuses, not `pending_state` writes.** arch/pipeline.md reserved
   explicit `pending_state` for "future pipeline code"; with proper task
   statuses (`changes_requested`, `ready_to_merge`) + `DerivedState` mappings
   the diff-based sync carries every transition with zero new writers. The
   explicit-wins mechanism stays untouched for operator overrides.

3. **Dispatch owns its spawn via `agentspawn.SpawnFresh`** (the
   scheduler-job pane path, proven), extended with `Role` + `TaskID`
   (defaults preserve current behavior). NOT `orchestrator.EnsureAgent` —
   that path's spawner is a stub in `cmd task-scheduler` and binds no soul
   template. `SpawnFresh` already does soul clone → tmux window → ready wait
   → status flip → sidecar inbox goroutine.

4. **Agent id mint inside pipeline** (small query, suffix-bumping like
   `mintAgentID`): `reviewer-<taskID>` / `reviewer-<taskID>-r2` on collision.
   No import of orchestrator; keeps pipeline self-contained. `claimed_by`
   is NOT set for reviewers (they don't hold the task claim; the task sits
   in `review`).

5. **Verdict = latest assistant outbox message containing a contract line.**
   Parse `^VERDICT: (approve|request_changes|needs_human)\s*$` (line-trimmed)
   in the newest assistant row for the reviewer agent. Soul contract (EX-02
   AC 8) says the session ENDS with exactly one such line. No-match ⇒ keep
   waiting (the watchdog covers permanent stalls). Re-parse is impossible:
   the reviewer flips to `dead` in the same tx as the transition, and both
   passes only consider live reviewers.

6. **Zero-author check, two layers.** Structural: dispatch always mints a
   FRESH reviewer id — never reuses a live agent (contrast EnsureAgent's
   reuse path). Explicit: reviewer id compared against the author recorded in
   the review-entry data (task_context `result` row written by the done
   path); on equality (pathology guard) → no spawn, task → `pending_approval`,
   loud log.

7. **Stall watchdog.** A live reviewer with no verdict whose `last_seen` is
   older than `MAQUINISTA_REVIEW_TIMEOUT` (default `2h`) → agent dead +
   task `pending_approval`. Prevents permanently-stuck `review` tasks; EX-04's
   fixer re-claim and a human unblock both start from `pending_approval`.

8. **Runner/model resolution is a pure function.**
   `ResolveExec(cfg, defaultRunner, reasoningClass)` → `(runner, model)`:
   runner from extras `default_runner` (fallback cfg.DefaultRunner); model =
   `MAQUINISTA_PI_MODEL_HIGH` for `high`, else `MAQUINISTA_PI_MODEL`
   (fallbacks: cfg default model). The runner override rides
   `FreshParams.RunnerType`; the model rides the spawn env. Keeps the EX-02
   literals (`pi` + `standard|high`) as the only interface.

## Interfaces (frozen upstream contracts honored)

- Verdict vocabulary: `VERDICT: approve|request_changes|needs_human`
  (role-souls plan AC 8) — parser implements exactly this.
- Extras keys `default_runner`, `reasoning_class` (role-souls AC 13) —
  read as `[]byte` (pgx v5 jsonb gotcha, role-souls AC 15).
- Template id literal `pipeline-reviewer` (migration 035) — dispatch reads,
  never writes templates.
- `DerivedState` signature unchanged; two new case arms.
- No new env vars beyond `MAQUINISTA_REVIEW_TIMEOUT` (optional, defaulted).

## Acceptance criteria

- AC 1: Completing a pipeline task (metadata `ticket_issue_id`) sets
  status `review`, stamps `done_at`, releases `claimed_by`; a non-pipeline
  task still lands `done` with identical fields.
- AC 2: `DerivedState("changes_requested")` → `Changes Requested`;
  `DerivedState("ready_to_merge")` → `Ready to Merge`; all previous
  mappings byte-identical; unknown still skipped.
- AC 3: Dispatch pass spawns a reviewer for a `review` pipeline task with no
  live reviewer: fresh agents row (task_id bound, role `reviewer`), soul
  cloned from template `pipeline-reviewer`, cwd = task worktree_path,
  runner from extras, pane open, sidecar inbox goroutine started.
- AC 4: Zero-author: reviewer id == author id ⇒ no spawn, task flips
  `pending_approval`, log line emitted; fresh-mint path never returns a
  pre-existing id.
- AC 5: Review prompt enqueued to the reviewer's inbox
  (`origin_channel='task'`, `external_msg_id='review:<task>:<round>'`,
  type `review`) with task id, worktree path, diff base, verdict reminder.
- AC 6: `tasks.review_rounds` increments exactly once per reviewer spawn.
- AC 7: Idempotent: a second dispatch tick over an already-reviewed task is
  a no-op (live-index guard + pre-check); crash between spawn and prompt
  heals on the next tick (prompt enqueue keyed by external_msg_id).
- AC 8: Verdict parser matches the contract line only at a line boundary,
  exact vocabulary; latest message wins; no match ⇒ no transition.
- AC 9: Transitions: approve → `ready_to_merge`; request_changes →
  `changes_requested`; needs_human → `pending_approval`; each writes a
  `task_context` row kind `verdict` (agent_id = reviewer) in the same tx.
- AC 10: After a parsed verdict the reviewer agents row is `dead` and its
  tmux window killed (best-effort); the task's live slot is free.
- AC 11: Board mirror: after each transition the sync loop (unchanged)
  pushes the canonical column — verified by `DerivedState` unit proofs +
  the explicit-wins precedence test staying green.
- AC 12: Wiring: `cmd start` runs `RunDispatch` only when the pipeline is
  enabled (`FromEnv().Enabled()`), beside bridge/sync; orchestrator without
  pipeline config is byte-identical.
- AC 13: `ResolveExec` maps: extras pi+standard → (pi, standard-model);
  pi+high → (pi, high-model); unknown class → standard; empty extras →
  cfg defaults. No provider/linear vocabulary anywhere in dispatch.
- AC 14: Neutrality check: under `internal/pipeline/`, `linear` appears only
  in `linear.go`/`linear_test.go`; dispatch files mention neither linear
  nor provider types.
- AC 15: Docs: `arch/pipeline.md` gains the Dispatch section (trigger, spawn,
  verdict, transitions, watchdog, env); CLAUDE.md package map gains the
  dispatch line; status comment drift in 001 noted in the spec (migrations
  immutable).
- AC 16: Watchdog: reviewer `last_seen` older than timeout ⇒ agent dead,
  task `pending_approval`; reviewer inside timeout ⇒ untouched.

## Checks (C1–C16 → AC 1–16)

See checks.md. House rule: every proof shows its PASS lines, not exit codes.

## Risks

- Reviewer pane readiness flakiness (known pi/claude first-run modals) —
  SpawnFresh already warns-only; the watchdog catches a never-ready pane.
- Outbox latency: monitor writes assistant rows as they stream; a verdict
  seen mid-stream would transition early. Contract says final line; accepted
  residual risk, same trust EX-01's verifier used.
- Double dispatch across two orchestrator processes: guarded by the partial
  unique-live index (migration 011) + live pre-check; second process errors
  and no-ops.

## Assumptions to falsify at pilot (EX-07)

- The reviewer can actually review a worktree diff with origin/main as base
  in the maquinista repo layout (git fetch happens in-prompt, agent-side).
- 2h default is generous enough for a high-reasoning review on pi.
