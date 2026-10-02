# EX-03 review-dispatch verification

**Verdict**: PASS (16/16 checks proven; gate green — see Verdict section)
**Profile**: light (default — checks.md carries no `Profile:` line; proofs at HEAD with located assertions, no fault injection)
**Round**: 1 - full
**Diff range**: ba116bf..HEAD (beca23d spec, c9ae4f3 implementation, d9dca5d checks re-point)
**Worktree**: `maquinista.ex03-dispatch`, branch `ex03-dispatch`
**Verifier**: independent sub-agent, fresh context — author != verifier. Every proof below was
executed by the verifier from the worktree root at HEAD (d9dca5d); exit codes captured
`; echo rc=$?`.

## Checks

| C# | Proof (short) | Result | Evidence |
| --- | --- | --- | --- |
| C1 | `go test ./internal/db/ -run TestMarkDone -v` | PASS | `--- PASS: TestMarkDone_PipelineTaskGoesToReview`, `--- PASS: TestMarkDone_PlainTaskStillDone`, rc=0; markdone_test.go:55-63 asserts status `review`, done_at non-nil, claimed_by NULL, plus the result context row for zero-author (:66-76); markdone_test.go:92-95 asserts plain task still `done` |
| C2 | `go test ./internal/pipeline/ -run TestDerivedState_ReviewTransitions -v` + `-run TestSync_DerivedState -v` (one shared invocation, see Notes) | PASS | `--- PASS: TestDerivedState_ReviewTransitions`, `--- PASS: TestSync_DerivedState`, rc=0; new arms dispatch_test.go:430-438 (`changes_requested`→ColChangesRequested, `ready_to_merge`→ColReadyToMerge) + label :439-441 `Ready to Merge`; regression arms ready/claimed/review/pending_approval/failed/done + unmapped-unknown at sync_test.go:35-51 (pre-existing, unmodified) |
| C3 | `go test ./internal/pipeline/ -run TestDispatch_SpawnsReviewerWithSoulAndBinding -v` | PASS | `--- PASS: TestDispatch_SpawnsReviewerWithSoulAndBinding`, rc=0; agents row role=reviewer/task_id/status=running asserted dispatch_test.go:204-212; task-bound spawn params (AgentID/TaskID/WorktreePath/RunnerType) :196-201; second-tick no-op tail :223-233 — see Notes for the unasserted agent_souls-row and cwd-on-row sub-claims |
| C4 | `go test ./internal/pipeline/ -run TestZeroAuthor_RejectsSelfReview -v` plus TestMintReviewerID_BumpsSuffix (one shared invocation, see Notes) | PASS | `--- PASS: TestZeroAuthor_RejectsSelfReview`, `--- PASS: TestMintReviewerID_BumpsSuffix`, rc=0; no spawn + `pending_approval` flip asserted dispatch_test.go:168-173; fresh mint `reviewer-t9` :133-135, bump past live+dead ids to `reviewer-t9-r3` :146-149 |
| C5 | same run as C3 + `TestDispatch_HealsMissingPrompt` | PASS | prompt-row assertions dispatch_test.go:217-221: count=1 with `external_msg_id='review:ta:1'`, `origin_channel='task'`, `content->>'type'='review'`; second tick does not duplicate (:223-233); heal non-dup `--- PASS: TestDispatch_HealsMissingPrompt`, exactly one row :251-254; prompt body (worktree + VERDICT) built at dispatch.go:352-355 — text unasserted, see Notes |
| C6 | `go test ./internal/pipeline/ -run TestReviewRounds_IncrementsPerSpawn -v` | PASS | `--- PASS: TestReviewRounds_IncrementsPerSpawn`, rc=0; round 1 via TestDispatch_SpawnsReviewerWithSoulAndBinding :214-216; second round after verdict + re-entry to review: 2 spawns, `reviewer-tc-r2`, review_rounds=2 at dispatch_test.go:281-289 |
| C7 | tail of C3's test + `TestDispatch_HealsMissingPrompt` | PASS | second RunDispatch tick strict no-op (no agents row, no inbox row): dispatch_test.go:223-233 (spawns unchanged, review_rounds still 1); crash-heal: `--- PASS: TestDispatch_HealsMissingPrompt`, agents row present + inbox row absent seeded :243-244, two promptPass ticks enqueue exactly one :246-254 |
| C8 | `go test ./internal/pipeline/ -run TestParseVerdict_Table -v` | PASS | `--- PASS: TestParseVerdict_Table`, rc=0; all three exact values dispatch_test.go:84-86; case `verdict: approve` rejected :88; `VERDICT:approved` no-space and `VERDICT: maybe` unknown → malformed :89-90; trailing words :91; prose line rejected, VERDICT-line-in-prose matched :85,92; latest-message-wins at DB level via two outbox rows with verdict in the newer one, TestVerdictTransitions :310-317 |
| C9 | `go test -run '^TestVerdictTransitions$' -v` | PASS | `--- PASS: TestVerdictTransitions` + `--- PASS: TestVerdictTransitions/approve`, `/request_changes`, `/needs_human`, rc=0; each subtest: status lands mapped value dispatch_test.go:323-325 and task_context verdict row with agent_id=reviewer :326-336, same tx via applyVerdict (dispatch.go:515-539) |
| C10 | tails of each TestVerdictTransitions subtest | PASS | `--- PASS: TestVerdictTransitions/approve` etc. all rc=0; agents.status `dead` asserted dispatch_test.go:337-345; live slot released: post-verdict fresh mint succeeds :346-349; KillWindow nil-injected per checks.md mid-build note (dispatch_test.go:319) |
| C11 | full internal/pipeline suite (see Gate) + `git diff --stat -- internal/pipeline/sync_test.go` | PASS | sync_test.go unmodified (absent from diff --stat); `--- PASS` for all 33 pipeline tests incl. pre-existing TestSync_DerivedOverwriteRule (explicit-wins precedence) and TestSync_DerivedState, 0 FAIL, package `ok` 63.2s rc=0; new statuses flow through DerivedState per C2 |
| C12 | source inspection cmd_start.go + `go build ./...` + `go vet ./internal/pipeline/ ./cmd/...` | PASS | cmd_start.go:464 gate `if tCfg := pipeline.FromEnv(); tCfg.Enabled() && pool != nil` wraps cmd_start.go:470 `pipeline.RunDispatch(ctx, pool, pipeline.DispatchConfigFromEnv(...), spawner, tmux.KillWindow)` in its own goroutine, beside RunBridge (cmd_start.go:477) and RunSync (cmd_start.go:482) inside the same gate; build rc=0, vet rc=0 |
| C13 | `go test ./internal/pipeline/ -run TestResolveExec_Table -v` + the C14 grep | PASS | `--- PASS: TestResolveExec_Table`, rc=0; table dispatch_test.go:110-115: pi+standard → (pi, m-std), pi+high → (pi, m-high), unknown class → std, empty extras runner → cfg, empty model = runner's own chain; no linear/provider vocabulary in dispatch files (C14's empty grep doubles as this proof) |
| C14 | `rg -n 'linear' internal/pipeline/dispatch*.go` + import/deps inspection + build | PASS | literal rg rc=1 (zero hits in dispatch.go+dispatch_test.go); case-insensitive sweep hits only the neutrality comment dispatch.go:13 ("holds no provider dependency"); import block (dispatch.go:16-28) = stdlib + pgxpool + internal/mailbox only; `go list -deps` of the pipeline package greps case-insensitively for linear or ticket vocabulary — rc=1 (no hits); build rc=0 |
| C15 | arch/CLAUDE inspection + `git diff --stat -- internal/soul/ internal/db/migrations/` | PASS | arch/pipeline.md:64 `## Review dispatch (EX-03)` names trigger (entry+zero-author, :71-84), spawn (:89-92), verdict (:98-110), watchdog (:112-116); env contract table row `MAQUINISTA_REVIEW_TIMEOUT` arch/pipeline.md:127; CLAUDE.md:93 package-map row names review dispatch + dispatch.go; diff --stat over internal/soul/ + internal/db/migrations/ is empty (no edits) |
| C16 | `go test ./internal/pipeline/ -run TestWatchdog -v` | PASS | `--- PASS: TestWatchdog_StallTimeout`, `--- PASS: TestWatchdog_InsideTimeoutUntouched`, rc=0; stalled reviewer → `pending_approval` + watchdog verdict row dispatch_test.go:386-397; reviewer with fresh outbox activity → untouched `review`/`running` :414-423; stall trigger is no-outbox-activity within timeout (dispatch.go:551-555) — see Notes on the checks.md `last_seen` wording |

## Gate

`go test ./internal/pipeline/... ./internal/db/... ./internal/agentspawn/...` (with -v) — rc=0;
65 named `--- PASS` lines, 0 `--- FAIL`; `ok` for internal/pipeline (63.2s), internal/db
(67.9s), internal/agentspawn (0.009s). All named C-checks re-confirmed inside the gate log
(grep `--- PASS: <name>` = 1 each, incl. TestVerdictTransitions/approve). The three
pre-excluded internal/monitor tests (TestOutboxSink_WritesAssistantText,
TestOutboxSink_WritesThinking, TestToolEventSink_PairedEmitsBoth — verified failing at base
ba116bf, rows=0 want 1) are outside the three gate packages; cited as known, not re-run.
C11's explicit-wins precedence test (TestSync_DerivedOverwriteRule) and the rest of the
unmodified sync_test.go are green inside the same run.

## Notes

- **Profile**: checks.md carries no `Profile:` line; this report runs the default `light`
  shape — proofs at HEAD with located assertions, no Faults injected section (same shape as
  the role-souls precedent report).
- **C3 assertion gap (minor):** checks.md says the proof records "agent_souls row exists for
  reviewer, cwd == task worktree". The built test uses the fakeSpawner seam (documented
  "EX-03 owns the DB surface", dispatch_test.go:14-17): it asserts the agents row
  (role/task_id/status, :204-212) and the spawn params incl. WorktreePath (:196-201), but
  never SELECTs the agents row's cwd and asserts no agent_souls row. Materially the wiring is
  in source — pipeline_dispatch.go:35-36 passes `CWD: p.WorktreePath` and
  `SoulTemplateID: pipeline.ReviewerSoulTemplate` into SpawnFresh, and agentspawn.go:84
  clones the template into agent_souls — but no automated assertion in the diff would catch a
  regression dropping the soul clone or the cwd passthrough. Soul/cwd mechanics are
  SpawnFresh's own surface (checks.md itself notes pane mechanics are SpawnFresh's), and
  agentspawn_test.go has no soul-clone test either.
- **C5 assertion gap (minor):** the prompt row's existence, key, channel and type are
  asserted (dispatch_test.go:217-221) but its body is not; "prompt mentioning worktree +
  VERDICT" holds by source (dispatch.go:352-355 — "the task worktree" and the exact
  `VERDICT: approve | ...` line), unasserted by test.
- **C16 wording imprecision (minor):** checks.md C16 phrases the trigger as "last_seen 3h
  ago"; the built mechanism is no-outbox-activity within the timeout (dispatch.go:551-555,
  documented at arch/pipeline.md:112-116 and dispatch.go:72-75). Behavior claims (dead +
  `pending_approval`; untouched inside timeout) are asserted; additionally the watchdog test
  does not assert the reviewer row flips to `dead` (only the verdict path asserts agent
  death, dispatch_test.go:337-345; code sets it at dispatch.go:579).
- **C4 wording imprecision (minor):** "suffix bumping asserted across 3 sequential mints" —
  the built test makes 2 mint calls covering 3 ids (fresh `reviewer-t9`; then
  `reviewer-t9-r3` after a live and a dead block, dispatch_test.go:132-149). The AC 4 claim
  (fresh mint never returns a pre-existing id) is asserted.
- **First verifier run filtered 6 tests out silently:** my initial `-run` pattern anchored
  prefix elements with `$` (`TestDispatch_$` etc.), so TestDispatch_*, TestWatchdog_* and
  TestMarkDone_* matched nothing while rc stayed 0 — exactly the empty-filter trap verify.md
  warns about. Caught by the GO PROOF RULE (names absent from output), pattern fixed,
  re-run; the second run shows all 15 named tests + 3 subtests as `--- PASS`.
- **sync_test.go unmodified:** confirmed not in `git diff ba116bf..HEAD --stat` (only sync.go
  gained the two DerivedState arms). linear.go/provider.go gained the ColReadyToMerge
  const/label/column-name — the only 'linear'-mentioning files remain linear.go/_test.
- **C2/C3/C5/C6/C7/C9/C10 share one `-run` invocation** across two packages (single `go test
  ./internal/pipeline/ ./internal/db/ -run ... -v`); rows cite the shared rc=0.

## Verdict

All 16 checks pass with their named tests as `--- PASS` (and rc=0) plus located file:line
assertions targeting the check-defined values; the three-package full-suite gate is green
(65 named PASS lines, 0 FAIL). Findings are four minor test-assertion/wording imprecisions
(C3 agent_souls + cwd-on-row unasserted, C5 prompt body unasserted, C16 outbox-recency vs
`last_seen` wording and unasserted watchdog agent-death, C4 "3 sequential mints" wording) —
none changes an outcome; the material claims hold by source inspection at the cited lines.

VERDICT: approve
