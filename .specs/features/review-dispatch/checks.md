# EX-03 review-dispatch — checks

Each check names its proof. House rule (skill): cite the PASS/ok lines, never
a bare exit code. DB-backed proofs use the throwaway-pool test harness pattern
from internal/pipeline tests (docker Postgres :5434, `make up`).

- C1 (AC 1) done-path branch. Test in internal/db: seed task with
  `metadata='{"ticket_issue_id":"X"}'` + claimed agent → completing tx →
  status `review`, done_at set, claimed_by NULL. Same test, plain task →
  `done`. PASS lines: TestDonePath_PipelineTaskGoesToReview,
  TestDonePath_PlainTaskStillDone.

- C2 (AC 2) DerivedState. Table test: `changes_requested` →
  Column("Changes Requested"), `ready_to_merge` → Column("Ready to Merge"),
  plus regression arms (ready/claimed/review/pending_approval/failed/done/
  unknown) asserted equal to pre-change literals.
  TestDerivedState_ReviewTransitions.

- C3 (AC 3) reviewer spawn. Test with fake tmux-free seams where possible;
  integration proof spawns against docker Postgres + records: agents row
  (task_id, role=reviewer, status running), agent_souls row exists for
  reviewer, cwd == task worktree. tmux window assertion is env-gated
  (skips when no tmux server) with the skip logged — pane mechanics are
  SpawnFresh's, already covered there; EX-03 owns the DB surface.
  TestDispatch_SpawnsReviewerWithSoulAndBinding.

- C4 (AC 4) zero-author. Unit: author==reviewer id ⇒ dispatch returns
  needs-human flip without inserting an agents row; fresh mint never
  collides (suffix bumping asserted across 3 sequential mints).
  TestZeroAuthor_RejectsSelfReview, TestMintReviewerID_BumpsSuffix.

- C5 (AC 5) review prompt. After dispatch, agent_inbox has one row for the
  reviewer with external_msg_id `review:<task>:1`, origin_channel `task`,
  content JSON type `review`, prompt mentioning worktree + VERDICT.
  Re-dispatch (heal path) does NOT duplicate the row.
  TestReviewPrompt_EnqueuedOnce.

- C6 (AC 6) review_rounds. Spawn → `review_rounds`=1; second round (after
  verdict + re-entry to review) → 2. TestReviewRounds_IncrementsPerSpawn.

- C7 (AC 7) idempotency. Two RunDispatch ticks in a row: second inserts no
  agents row, no inbox row (live guard). Crash-heal: agents row present,
  inbox row absent → next tick enqueues exactly one.
  TestDispatch_IdempotentTick, TestDispatch_HealsMissingPrompt.

- C8 (AC 8) verdict parser. Table test: exact lines (all three values) match;
  `verdict: approve` (case), `VERDICT:approved` (no space),
  `VERDICT: maybe` (unknown value), embedded-in-prose-with-other-lines
  matching only when the VERDICT line is present — latest assistant message
  wins when several rows exist. TestParseVerdict_Table.

- C9 (AC 9) transitions. For each verdict seeded into the reviewer's outbox:
  task status lands the mapped value and task_context gains a `verdict` row
  with agent_id = reviewer in the same tx.
  TestVerdictTransitions_Approve / _RequestChanges / _NeedsHuman.

- C10 (AC 10) reviewer retirement. Post-verdict: agents.status `dead`,
  tmux KillWindow invoked (fake capture), live slot released (a new mint
  succeeds). TestVerdictRetiresReviewer.

- C11 (AC 11) mirror integrity. sync_test.go passes unmodified (regression);
  new statuses flow through DerivedState (C2). PASS line of the full
  internal/pipeline suite cited.

- C12 (AC 12) wiring. cmd start source inspection: RunDispatch gated on
  `FromEnv().Enabled()` beside RunBridge/RunSync; `go build ./...` +
  `go vet ./internal/pipeline/ ./cmd/...` green.

- C13 (AC 13) ResolveExec table test (runner, model) from (extras, cfg):
  pi/standard, pi/high, unknown class, empty extras. No linear/provider
  vocabulary in dispatch files (C14 grep doubles as the proof here).
  TestResolveExec_Table.

- C14 (AC 14) neutrality. `rg -n 'linear' internal/pipeline/dispatch*.go`
  → zero hits; `go list -deps` of the pipeline package shows no provider
  import in dispatch.go. Cited grep output (empty) + build green.

- C15 (AC 15) docs. arch/pipeline.md Dispatch section exists and names the
  trigger/spawn/verdict/watchdog/env contract; CLAUDE.md package map row;
  git diff shows no edits to internal/soul templates or migration 035.

- C16 (AC 16) watchdog. Reviewer last_seen 3h ago, timeout 2h ⇒ dead +
  `pending_approval`; last_seen 5m ago ⇒ untouched.
  TestWatchdog_StallTimeout, TestWatchdog_InsideTimeoutUntouched.

## Full-suite gate

`go test ./internal/pipeline/... ./internal/db/... ./internal/agentspawn/...`
with named PASS lines; known-failing-at-base monitor tests (skill list) are
out of scope and cited if they appear.
