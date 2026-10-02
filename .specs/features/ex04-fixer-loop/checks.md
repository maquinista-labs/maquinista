# EX-04 fixer-loop — checks

Each check names its proof. House rule (skill): cite the PASS/ok lines, never
a bare exit code. DB-backed proofs use the pipeline package's testcontainer
harness (postgres:16-alpine, per dispatch_test.go).

- C1 (AC 1) fixer spawn. Test with fake spawner capturing params + real DB:
  seed task in `changes_requested` (pipeline metadata, worktree) + verdict
  row `VERDICT: request_changes` → dispatch fixer pass → spawn called once
  with Role `fixer`, template `pipeline-fixer`, cwd = worktree; agents row
  exists (role fixer, task-bound). Second candidate with a live agent (any
  role) → not spawned. PASS lines: TestFixerSpawn_SpawnsForChangesRequested,
  TestFixerSpawn_SkipsWhenLiveAgent.
- C2 (AC 2) fix prompt. After the pass: agent_inbox has exactly one row for
  the fixer with external_msg_id `fix:<task>:<round>`, origin_channel
  `task`, content type `fix`, prompt embedding the reviewer findings text;
  task_context has the kind `fix` row for the round.
  TestFixerSpawn_EnqueuesFixPromptOnce.
- C3 (AC 3) heal. Live fixer + missing prompt row → heal pass enqueues
  exactly one; running the heal twice enqueues no second row.
  TestFixerPrompt_HealsMissing.
- C4 (AC 4) episode idempotency. Second fixer pass over the same
  changes_requested episode (fix row present) → no spawn call.
  TestFixerSpawn_EpisodeIdempotent.
- C5 (AC 5) round cap. Seed task in `review` with review_rounds = cap;
  reviewer outbox carries `VERDICT: request_changes`; verdict pass → task
  `pending_approval`, verdict row notes the cap, reviewer retired. Same
  scenario with rounds below cap → `changes_requested`.
  TestApplyVerdict_RoundCapParks, TestApplyVerdict_UnderCapLandsChanges.
- C6 (AC 6) verdict regression. approve → `ready_to_merge`; needs_human →
  `pending_approval`; EX-03 transition arms unchanged.
  TestVerdictTransitions (subtests approve/request_changes/needs_human) at
  the new cap logic — existing test re-run green unmodified.
- C7 (AC 7) re-entry. MarkDone on a fixer-completed pipeline task → status
  `review`, done_at set, claimed_by NULL; then dispatch pass mints a fresh
  reviewer id (`reviewer-<task>` or `-rN`, ≠ any fixer id) and bumps
  review_rounds. TestMarkDone_FixerCompletedGoesToReview,
  TestFixerLoop_NextRoundMintsFreshReviewer.
- C8 (AC 8) fixer watchdog. Live fixer on changes_requested, no outbox
  activity past timeout → task `pending_approval`, watchdog verdict row,
  fixer dead; inside timeout → untouched.
  TestFixerWatchdog_StallParks, TestFixerWatchdog_InsideTimeoutUntouched.
- C9 (AC 9) exec resolution. ResolveExec table re-run (pi/standard, pi/high,
  unknown, empty) + generalized resolver on `pipeline-fixer` extras returns
  the std-model path. TestResolveExec_Table, TestResolveTemplateExec_Fixer.
- C10 (AC 10) neutrality. `rg -n 'linear' internal/pipeline/dispatch*.go` →
  zero hits; no provider imports; fixer pass SQL writes tasks/task_context/
  agents/agent_inbox only. Cited grep output (empty) + build green.
- C11 (AC 11) wiring. cmd start unchanged from EX-03's gate (source
  inspection: RunDispatch call site); `go build ./...` +
  `go vet ./internal/pipeline/ ./cmd/...` green.
- C12 (AC 12) docs. arch/pipeline.md has `## Fixer loop (EX-04)` naming
  trigger/spawn/findings/cap/watchdog; env table names
  MAQUINISTA_REVIEW_ROUNDS_MAX; TODO line updated; CLAUDE.md pipeline row
  mentions the fixer loop; `git diff base..HEAD` shows no edits to soul
  templates or migrations.

## Full-suite gate

`go test ./internal/pipeline/... ./internal/db/... ./internal/agentspawn/...`
with named PASS lines. Known-failing-at-base tests, pre-excluded by name
(verified failing at base 37565b4 on 02/10 — cite, do not re-run):
internal/monitor TestOutboxSink_WritesAssistantText,
TestOutboxSink_WritesThinking, TestToolEventSink_PairedEmitsBoth
(rows=0, want 1). Anything else red in the gate packages is in scope.
