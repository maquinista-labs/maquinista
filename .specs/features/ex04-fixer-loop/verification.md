# EX-04 fixer-loop verification

**Verdict**: PASS (12/12 checks proven; gate green — see Verdict section)
**Profile**: light (default — checks.md carries no `Profile:` line; proofs at HEAD with located assertions, no fault injection)
**Round**: 1 - full
**Diff range**: 4e4ab07..HEAD (4e4ab07 base spec, a381b9a implementation, 697e84a checks re-point; the CODE diff vs the EX-03 merge is 37565b4..HEAD)
**Worktree**: `maquinista.ex04-fixer`, branch `ex04-fixer`
**Verifier**: independent sub-agent, fresh context — author != verifier. Every proof below was
executed by the verifier from the worktree root at HEAD (697e84a); exit codes captured
`; echo rc=$?`.

## Checks

| C# | Proof (short) | Result | Evidence |
| --- | --- | --- | --- |
| C1 | `go test ./internal/pipeline/ -run 'TestFixerSpawn_SpawnsForChangesRequested\|TestFixerSpawn_SkipsWhenLiveAgent' -v` (shared with C2–C4, see Notes) | PASS | `--- PASS: TestFixerSpawn_SpawnsForChangesRequested (2.37s)`, `--- PASS: TestFixerSpawn_SkipsWhenLiveAgent (2.03s)`, rc=0; spawn once with AgentID `fixer-f1`, Role `fixer`, SoulTemplateID `pipeline-fixer` (FixerSoulTemplate), TaskID `f1`, WorktreePath `/tmp/wt-f1`, RunnerType `pi` asserted fixer_test.go:64-74; agents row materialized by the spawner and SELECTed back → (role=fixer, task_id=f1) :77-84; second scenario seeds a live fixer (seedFixer :96) → spawns=0 asserted :102-104 — see Notes on checks.md's "live agent (any role)" wording |
| C2 | same shared invocation as C1–C4 | PASS | `--- PASS: TestFixerSpawn_EnqueuesFixPromptOnce (2.23s)`, rc=0; agent_inbox count=1 for agent_id `fixer-f3` with external_msg_id `fix:f3:2`, origin_channel `task`, content type `fix` asserted fixer_test.go:119-123; prompt embeds the reviewer findings text (`tests missing`, `checks.md claim unproven`) plus the done reminder `maquinista-done f3` :124-134; task_context kind `fix` row for the round exists with content `round 2` :135-142; external_msg_id minted at dispatch.go:963, findings = reviewer's newest outbox text (latestFindings dispatch.go:885-903) |
| C3 | same shared invocation as C1–C4 | PASS | `--- PASS: TestFixerPrompt_HealsMissing (2.17s)`, rc=0; live fixer + missing prompt row (seeded fixer-f5, no inbox row) → two fixerPass ticks enqueue exactly one (count=1 for `fix:f5:1`) asserted fixer_test.go:175-183; heal SQL fixerPromptHealSQL dispatch.go:758-774 run by fixerPromptPass :844-880; dedup via EnqueueInbox's (origin_channel, external_msg_id) conflict target |
| C4 | same shared invocation as C1–C4 | PASS | `--- PASS: TestFixerSpawn_EpisodeIdempotent (2.15s)`, rc=0; first pass spawns 1 (fix row commits FIRST, dispatch.go:914-918, before the prompt tx :920-928), second fixerPass over the same changes_requested episode spawns 0 extra asserted fixer_test.go:153-163; episode key = no fix row for the current episode, fixerCandidatesSQL :750-753 |
| C5 | `go test ./internal/pipeline/ -run 'TestApplyVerdict_RoundCapParks\|TestApplyVerdict_UnderCapLandsChanges' -v` | PASS | `--- PASS: TestApplyVerdict_RoundCapParks (2.22s)`, `--- PASS: TestApplyVerdict_UnderCapLandsChanges (2.08s)`, rc=0; rounds=3 (=cap) → landed+persisted `pending_approval` fixer_test.go:201-206, verdict row content notes `round cap 3` :207-214, reviewer retired (`dead`) :215-221; rounds=1 (<cap) → `changes_requested` returned and persisted :236-238; cap decided atomically in the guarded UPDATE (CASE WHEN changes_requested AND review_rounds >= $3 THEN pending_approval) dispatch.go:597-603 — see Notes on the outbox/verdictPass wording |
| C6 | `go test ./internal/pipeline/ -run '^TestVerdictTransitions$' -v` | PASS | `--- PASS: TestVerdictTransitions (9.24s)` + `--- PASS: TestVerdictTransitions/approve (3.08s)`, `/request_changes (3.31s)`, `/needs_human (2.85s)`, rc=0; the three mappings unchanged (approve→ready_to_merge, request_changes→changes_requested, needs_human→pending_approval) dispatch_test.go:303-305; each subtest asserts the status lands the mapped value :327-329, verdict row content+author :330-340, reviewer dead :341-349, live slot freed via post-verdict mint :350-353; git diff of dispatch_test.go over 37565b4..HEAD is signature-only (maxRounds param at :323, fakeSpawner honors p.Role) — EX-03 transition arms unmodified |
| C7 | `go test ./internal/pipeline/ ./internal/db/ -run 'TestMarkDone\|TestFixerSpawn_SecondEpisodeSpawns' -v` | PASS | `--- PASS: TestMarkDone_FixerCompletedGoesToReview (3.41s)`, `--- PASS: TestFixerSpawn_SecondEpisodeSpawns (3.07s)`, rc=0; MarkDone by the fixer on a fixer-completed pipeline task → status `review`, done_at set, claimed_by NULL asserted fixer_test.go:257-270; then dispatchPass spawns exactly 1 with AgentID prefixed `reviewer-f8` + Role `reviewer` (never a fixer id) :274-286 and review_rounds bumped to 2 :287-289; second-episode variant spawns exactly `[fixer-fb]` with a round-2 fix row :372-396 (the re-pointed real name for the planned TestFixerLoop_NextRoundMintsFreshReviewer); db.MarkDone regression: `--- PASS: TestMarkDone_PipelineTaskGoesToReview (3.26s)`, `--- PASS: TestMarkDone_PlainTaskStillDone (3.24s)` — internal/db has no files in the code diff (MarkDone untouched) |
| C8 | `go test ./internal/pipeline/ -run 'TestFixerWatchdog_StallParks\|TestFixerWatchdog_InsideTimeoutUntouched' -v` | PASS | `--- PASS: TestFixerWatchdog_StallParks (2.58s)`, `--- PASS: TestFixerWatchdog_InsideTimeoutUntouched (2.13s)`, rc=0; live fixer on changes_requested with no outbox activity past the 30m timeout → task `pending_approval` asserted fixer_test.go:304-306, watchdog verdict row (`fix stalled`) :307-315, fixer `dead` :316-322; fixer with fresh outbox activity (`fixing finding 1`) → untouched `changes_requested` :339-341; fixer arm = liveFixersSQL dispatch.go:637-644 + the shared no-outbox-activity stall filter :647-650, note text :674 |
| C9 | `go test ./internal/pipeline/ -run 'TestResolveExec_Table\|TestResolveTemplateExec_Fixer' -v` | PASS | `--- PASS: TestResolveExec_Table (0.00s)`, `--- PASS: TestResolveTemplateExec_Fixer (2.02s)`, rc=0; pure table dispatch_test.go:114-119 (pi/standard→(pi,m-std), pi/high→(pi,m-high), no-high-configured→m-std, empty-extras-runner→cfg runner, empty-model→runner's own chain, unknown class→std); template resolver on the REAL migrated extras: `pipeline-fixer` → (pi, m-std) and `pipeline-reviewer` unchanged → (pi, m-high) asserted fixer_test.go:347-368 |
| C10 | `rg -n 'linear' internal/pipeline/dispatch*.go` + import/deps inspection + build | PASS | literal rg rc=1 (zero hits); case-insensitive sweep of dispatch.go + dispatch_test.go also rc=1 — even the neutrality comment no longer contains the word (dispatch.go:19 says "holds no provider dependency"); import block dispatch.go:22-37 = stdlib + pgx/pgxpool + internal/mailbox only, no provider imports; `go list -deps ./internal/pipeline \| grep -ci -e linear -e ticket` rc=1 (zero hits); fixer-pass write surface enumerated in source: task_context fix row dispatch.go:914-918, agent_inbox via mailbox.EnqueueInbox dispatch.go:958-965 (INSERT INTO agent_inbox, mailbox.go:71) — the agents row is written by the spawner (SpawnFresh), tasks are untouched by the fixer pass, and no provider/table vocabulary appears; build rc=0 |
| C11 | source inspection cmd_start.go + `go build ./...` + `go vet ./internal/pipeline/ ./cmd/...` | PASS | cmd/maquinista/cmd_start.go is absent from the code diff (unchanged from EX-03's gate); call site verified: cmd_start.go:464 gate `if tCfg := pipeline.FromEnv(); tCfg.Enabled() && pool != nil` wraps the :468-471 goroutine `pipeline.RunDispatch(ctx, pool, pipeline.DispatchConfigFromEnv(cfg.TmuxSessionName), spawner, tmux.KillWindow)`; EX-04's only cmd change is the spawner adapter now defaulting empty Role/SoulTemplateID to the reviewer pair (pipeline_dispatch.go:35-42) while the fixer pass passes both explicitly (dispatch.go:811-819); build rc=0, vet rc=0 |
| C12 | arch/pipeline.md + CLAUDE.md inspection + `git diff --stat 37565b4..HEAD -- internal/soul/ internal/db/migrations/` | PASS | all five required aspects are named in arch/pipeline.md: trigger (:116-118 — changes_requested + worktree + request_changes verdict row), spawn (:120-122 — mint `fixer-<task>[-rN]`, pipeline-fixer extras), findings (:126-128 — newest-message tail ≤6000 chars, dedup `fix:<task>:<round>`, next-tick heal), cap (:106-110 — atomic round-cap park), watchdog (:136-142 — "or fixer (in changes_requested)… a stalled fixer is bounded the same way"); env table row `MAQUINISTA_REVIEW_ROUNDS_MAX` :154 (default 3); TODO updated "fixer re-claim loop: **shipped (EX-04)**" :193-194; CLAUDE.md:93 pipeline row names "reviewer/fixer spawn, verdict parsing + round cap, fixer loop, watchdog"; soul + migrations diff EMPTY (no edits) — see Notes for the heading-literal deviation |

## Gate

`go test ./internal/pipeline/... ./internal/db/... ./internal/agentspawn/... -v -count=1` — rc=0;
77 named `--- PASS` lines (74 top-level + 3 TestVerdictTransitions subtests), 0 `--- FAIL`;
`ok` for internal/pipeline (71.645s), internal/db (50.366s), internal/agentspawn (0.006s).
All 12 EX-04 named tests re-confirmed inside the gate log (grep `--- PASS: <name> (` = 1 each),
incl. the three TestVerdictTransitions subtests. The three pre-excluded internal/monitor tests
(TestOutboxSink_WritesAssistantText, TestOutboxSink_WritesThinking,
TestToolEventSink_PairedEmitsBoth — recorded failing at base 37565b4 on 02/10 per checks.md,
rows=0 want 1) sit outside the three gate packages; cited as known, not re-run. EX-03's
unmodified tests (TestDispatch_SpawnsReviewerWithSoulAndBinding, TestWatchdog_*,
TestMintReviewerID_BumpsSuffix, TestZeroAuthor_RejectsSelfReview, TestParseVerdict_Table,
TestVerdict_MalformedWaits, TestReviewRounds_IncrementsPerSpawn, TestSync_*) are green inside
the same run.

## Notes

- **Profile**: checks.md carries no `Profile:` line; this report runs the default `light`
  shape — proofs at HEAD with located assertions, no Faults injected section (same shape as
  the EX-03 report this record mirrors).
- **C12 heading-literal deviation (minor):** checks.md C12 says arch/pipeline.md "has
  `## Fixer loop (EX-04)`". No such heading exists; the fixer loop is documented as the
  `**Fixer pass (EX-04).**` block inside the `## Review dispatch (EX-03)` section
  (arch/pipeline.md:116-134), with the round cap folded into the verdict-transition bullet
  (:106-110) and the fixer arm named in the watchdog paragraph (:136-142). Every required
  aspect (trigger/spawn/findings/cap/watchdog) plus the env row, TODO update, CLAUDE.md row
  and the no-soul/migrations claim is proven; only the heading literal deviates.
- **C1 "any role" wording (minor):** the test proves the live-FIXER block (role-scoped
  candidate SQL dispatch.go:747-749, symmetric with EX-03's reviewer filter). A live agent of
  another role does not pre-filter the candidate query — instead the cross-role case is
  structurally excluded at spawn time by `uq_agents_task_live`
  (migrations/011_task_pipeline.sql:20-22, "at most one live agent per task"), surfacing as a
  spawn error that logs + retries next tick (dispatch.go:820-823). Not directly tested;
  materially the window is narrow (applyVerdict always sets the reviewer row dead, :620).
- **C5 wording (minor):** checks.md phrases C5 as "reviewer outbox carries
  `VERDICT: request_changes`; verdict pass →". The cap tests drive `applyVerdict` directly
  (the same call the verdict pass makes at dispatch.go:523) without seeding an outbox row or
  running verdictPass; the outbox-scan → verdictPass → applyVerdict wiring is separately
  covered unmodified by TestVerdictTransitions/request_changes. The cap/under-cap outcomes
  themselves are asserted end-to-end at the DB level.
- **TestFixerSpawn_SecondEpisodeSpawns log artifact (minor):** with the fake spawner
  (insertRow=false) the round-2 fix row commits first and the prompt enqueue for `fixer-fb`
  hits the agent_inbox FK (no agents row) — visible as a logged SQLSTATE 23503. The test
  asserts the spawn + round-2 fix row (:389-395); in production SpawnFresh inserts the agents
  row synchronously before recordFixEpisode, so the FK is satisfied and a prompt miss heals
  via fixerPromptPass. Harness artifact, not a production path issue.
- **Shared invocations:** C1–C4 shared one `-run` over internal/pipeline; C5+C6 shared one;
  C7 shared one over internal/pipeline + internal/db; C8+C9 shared one. Rows cite the shared
  rc=0. All `-run` patterns were alternation lists without prefix-anchoring mistakes — every
  named test appears as `--- PASS` (the empty-filter trap EX-03's verifier hit did not recur).
- **Pre-excluded monitor tests** cited per checks.md (failing at base 37565b4), outside the
  gate packages; not re-run per the checks.md instruction.

## Verdict

All 12 checks pass with their named tests as `--- PASS` (and rc=0) plus located file:line
assertions targeting the check-defined values; the three-package full-suite gate is green
(77 named PASS lines, 0 FAIL). Findings are three minor wording/literal imprecisions (C12
heading literal, C1 "any role" block layered via the unique-live index rather than the
candidate filter, C5 cap test driving applyVerdict directly) — none changes an outcome; the
material claims hold by test assertion and source inspection at the cited lines.

VERDICT: approve
