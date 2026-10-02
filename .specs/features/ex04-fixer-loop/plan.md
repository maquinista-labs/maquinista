# EX-04: Fixer loop — changes-requested re-claim, round cap

ADR-0005 "EX-04 Fixer loop (1 d): re-claim Changes-Requested tasks into the
same worktree/PR; round cap → Needs Human."

## Summary

EX-03 leaves pipeline tasks dead-ended at `changes_requested`: the verdict
lands, the board mirrors Changes Requested, and nothing ever consumes it. This
task closes the loop's return half: a dispatch pass spawns a fresh fixer agent
(`pipeline-fixer` soul, same task worktree) fed with the reviewer's findings,
the fixer resolves findings and signals completion via `maquinista-done`
(which re-enters `review` through EX-03's done-path branch, bumping the round
on the next reviewer spawn), and a round cap parks tasks that keep failing
review into `pending_approval` instead of cycling forever. The fixer watchdog
gets the same stall backstop reviewers have.

## In scope

- fixer spawn pass in `RunDispatch` (after verdict, before watchdog):
  candidates = pipeline task in `changes_requested` with a `request_changes`
  verdict row, a worktree, and no live agent
- fix prompt enqueue: findings text (reviewer's final outbox message),
  round-tagged, dedup'd `fix:<task>:<round>`
- prompt-heal arm for a live fixer missing its round's prompt
- round cap on `request_changes`: `review_rounds >= cap` →
  `pending_approval` (atomic in the verdict tx), env
  `MAQUINISTA_REVIEW_ROUNDS_MAX` (default 3)
- watchdog arm for stalled fixers (same timeout, same parking)
- spawner seam generalization: `ReviewSpawnParams` gains role + soul-template
  passthrough (reviewer defaults preserved)
- `resolveTemplateExec` generalized to a template-id parameter
- `arch/pipeline.md` Fixer loop section + env row

## Out of scope

- Arbiter role (contested/repeated request_changes adjudication) — the cap is
  the v1 arbiter; the `pipeline-arbiter` soul stays unused until a later
  exercise.
- Merge execution (`ready_to_merge` consumption) — EX-05.
- Telegram notifications on cap/verdicts — EX-06.
- Board writes — the mirror stays purely derived; `changes_requested` already
  maps to Changes Requested (EX-03 AC 2).

## Decisions

1. **The fixer is dispatched, not scheduled.** Symmetric with the reviewer:
   `RunDispatch` owns pipeline-agent lifecycles; the task-scheduler path
   (status `ready` → EnsureAgent) is worker-only. A `changes_requested` task
   never becomes `ready`, so no scheduler change is needed or wanted.

2. **Episode key = `review_rounds`.** While a task sits in `changes_requested`
   the round counter is frozen (it only bumps at the next reviewer spawn), so
   `(task, review_rounds)` names one findings episode exactly. The fix-spawn
   tx records a `task_context` row kind `fix` with that round; the candidate
   query excludes tasks that already have the row. The inbox dedup
   (`fix:<task>:<round>`) is the second guard; `uq_agents_task_live`
   (migration 011) is the cross-process third.

3. **Findings ride the prompt, not a new table.** The reviewer's final
   assistant message (the same newest outbox row `latestVerdict` parses) is
   the findings list per the soul contract; the fix prompt embeds its tail
   (last ~6000 chars). No schema change, no migration.

4. **The cap lives inside the verdict UPDATE.** `UPDATE tasks SET status =
   CASE WHEN review_rounds >= $cap THEN 'pending_approval' ELSE $1 END WHERE
   id = $2 AND status = 'review' RETURNING status` — one atomic statement, no
   read-then-write window, and the landed status tells the verdict row what
   to record. Cap semantics: the request_changes that lands when the task has
   already burned `cap` review rounds parks it; with default 3, round 3's
   request_changes is terminal. ADR-0005 AC 7 wording ("exceeds 3 review
   rounds") is honored with rounds counted per reviewer spawn, EX-03's
   existing accounting.

5. **No zero-author guard for fixers — by design.** The guard protects
   reviewer independence. A fixer continuing the previous fixer's work is the
   point of the loop; the reviewer at round N+1 is always a fresh mint.

6. **Re-entry is EX-03's code, unmodified.** The fixer ends with
   `maquinista-done` → `db.MarkDone` → pipeline task → `review` (done-path
   branch). The next spawn pass mints `reviewer-<task>[-rN]` fresh and bumps
   the round. The loop closes with zero new transition code — only a
   regression proof that the branch treats a fixer-completed task exactly
   like a worker-completed one.

7. **Spawner seam grows two passthrough fields, not a second interface.**
   `ReviewSpawnParams.Role` + `.SoulTemplateID` (empty = reviewer defaults,
   applied dispatch-side); the cmd adapter forwards them with the same
   fallbacks. One seam, EX-03 call sites byte-identical.

## Interfaces (frozen upstream contracts honored)

- Verdict vocabulary unchanged (`approve|request_changes|needs_human`).
- `pipeline-fixer` template (migration 035): goal "Resolve the reviewer
  numbered findings in the SAME worktree and PR", extras
  `{"default_runner": "pi", "reasoning_class": "standard"}` — dispatch reads,
  never writes.
- `ResolveExec` pure signature unchanged; the fixer rides the same function.
- `EnqueueInbox` dedup contract: `(origin_channel, external_msg_id)`.
- `uq_agents_task_live`: at most one live agent per task — the fixer spawn
  pass requires no live agent, any role.
- One new env var: `MAQUINISTA_REVIEW_ROUNDS_MAX` (default 3).

## Acceptance criteria

- AC 1: A pipeline task in `changes_requested` with a `request_changes`
  verdict row, a worktree, and no live agent gets a fresh fixer: agents row
  role `fixer`, id `fixer-<task>[-rN]` (never a reused id), soul cloned from
  `pipeline-fixer`, cwd = worktree, runner/model from the fixer extras.
- AC 2: Spawn is followed by exactly one fix prompt (`origin_channel='task'`,
  `external_msg_id='fix:<task>:<round>'`, content type `fix`) embedding the
  findings text and the round; a `task_context` row kind `fix` records the
  episode in the same flow.
- AC 3: A live fixer whose round's prompt row is missing gets exactly one
  re-enqueue (heal), never a duplicate.
- AC 4: An episode already consumed (fix row for the current round exists)
  spawns nothing on later ticks — no second fixer for the same findings.
- AC 5: A `request_changes` verdict when `review_rounds >= cap` lands the
  task `pending_approval` atomically, and the verdict row says so; the
  reviewer still retires and the slot frees.
- AC 6: Below the cap, `request_changes` still lands `changes_requested`
  (EX-03 regression); approve/needs_human paths are byte-identical.
- AC 7: A fixer completing the task (done path) sends it to `review` with
  `done_at` stamped and `claimed_by` released; the next reviewer spawn mints
  a fresh id and bumps the round — the loop closes.
- AC 8: A live fixer with no outbox activity past the stall timeout parks the
  task `pending_approval` with a watchdog verdict row and retires the pane; a
  fixer inside the timeout is untouched.
- AC 9: Exec resolution reads the named template's frozen extras via the
  generalized resolver: fixer (`standard`) → std model path; reviewer
  (`high`) unchanged; unknown class → standard; empty → cfg defaults.
- AC 10: Neutrality: dispatch files mention neither `linear` nor provider
  types; the fixer path holds no board writes and no ticket-system calls.
- AC 11: Wiring: the fixer spawn pass and the fixer watchdog arm run inside
  the existing `RunDispatch` tick behind the unchanged `FromEnv().Enabled()`
  gate; `go build ./...` + vet green.
- AC 12: Docs: `arch/pipeline.md` gains the Fixer loop section (trigger,
  spawn, findings, cap, watchdog) + the env row; the TODO line moves EX-04 to
  done; CLAUDE.md's pipeline package-map row reflects the fixer loop.

## Checks (C1–C12 → AC 1–12)

See checks.md. House rule: every proof shows its PASS lines, not exit codes.

## Risks

- Fixer quality loops: a fixer that "fixes" without addressing findings burns
  rounds until the cap — accepted; the cap is the backstop and EX-06 will
  surface the positions to a human.
- Findings truncation (6000-char tail) could drop early findings on a very
  long review message — accepted: the soul mandates a numbered findings list
  at the top of the final reply; EX-07 pilot falsifies the bound.
- Stalled-fix watchdog uses the same 2h default as reviews; a long fix (proof
  suites) may false-positive — same residual risk EX-03 accepted, tunable via
  the same env.

## Assumptions to falsify at pilot (EX-07)

- The fixer can actually push to the same PR from the task worktree (gh auth
  present agent-side).
- Round cap 3 is the right default for real review churn.
