# Pi binding heal checks

Profile: light

## Intent

Two silent-loss bugs remain in the pi transcript relay, both confirmed live on barceloneta on
01/10:

**BUG A — first-turn seed race.** Discovery seeds the tracked offset at the transcript's current
size (`source_pi.go:487`). pi appends the spawn prompt and often the first assistant reply within
seconds of spawn; discovery runs on a slower cycle, so the whole first turn predates the seed and
is skipped forever. Live case: topic t--…-11213 opened 22:33, agent's first reply ("Vale, parado
acá…") never relayed; the user had to re-open a new topic to get any answer.

**BUG B — stale sticky binding.** `resolvePiBindings` pass 1 keeps a persisted binding while the
transcript file exists, with no check that the file was ever written during the pane's lifetime
(`source_pi.go:264-280`). When reconcile re-spawns a pane FRESH (resume didn't take), the pane
boots a brand-new transcript while the DB session_id still names the pre-restart file; pass 1
sticks to the dead file and pass 3's backfill is gated on empty session_id, so it never fires.
Every reply in the new transcript is lost. Live case: "Hola che" typed into t--…-11032 at 20:42
was answered by pi into unclaimed transcript `01a0f4a1` (created 01:24:07, never bound) while the
monitor watched `01a0f474` (mtime 00:35, pre-restart). DB agents rows: @3→01a0f474,
@4→01a0f488, both started_at 01:24:2x — provably stale bindings.

Fix shape: (1) same-epoch bindings — transcript created at/after pane creation minus
`piBindingSlack` — seed offset 0 instead of file size, so the first turn relays exactly once
(user-role entries never reach the outbox sink, `sink_outbox.go:68`; echo is impossible).
Pre-epoch (resumed) bindings keep the size seed — no backlog replay. (2) New pass between pass 1
and 2 in `resolvePiBindings`: a sticky binding whose file mtime predates the pane's creation is
invalidated IFF an unclaimed candidate file (created within the pane-age slack) exists; pass 3's
existing newest-pane↔newest-file sort does the deterministic re-pair. A window the rule fires for
that pass 3 leaves unbound falls back to its persisted file (never silently unbound).

Behavior change to an existing obligation: pi-relay-loop C3 ("offset seeded to file size" on
rebind) asserted the size seed for a same-epoch live file — this feature deliberately flips that
case to seed 0 (relaying what the live transcript already contains is the point of the fix). The
`TestPiSource_RebindsToPaneFile` phase-2 offset assertion is updated accordingly.

When this ships: a fresh topic's first assistant reply reaches Telegram exactly once without a
second user message; a pane that reboots into a new transcript is re-paired to it on the next
discovery cycle; idle resumed panes and live bindings are untouched; no binding is ever dropped
to unbound.

2 defects · 7 checks in 1 slice · 0 one-way doors · 0 open

## Checks

### S1 - binding staleness + seed epoch · 3 files · ~35 KB · ~9k

**C1** - WHEN a pi transcript created at/after the pane's creation is bound at discovery while
already containing the spawn prompt and the first assistant reply, THEN the tracked offset seeds
to 0, the first poll includes the first assistant reply, and an immediately repeated poll yields
nothing further.
Proof: `go test ./internal/monitor/ -run TestPiSource_FirstTurnNotSkipped -v`

**C2** - WHEN the bound transcript predates the pane's creation by more than `piBindingSlack`
(resumed pre-epoch file), THEN the seed offset is the file's current size — no backlog replay.
Proof: `go test ./internal/monitor/ -run TestPiSource_PreEpochSeedsAtSize -v`

**C3** - WHEN a cwd group holds two panes whose persisted session_ids point at transcripts whose
mtimes predate both panes' creation AND two unclaimed newer transcripts exist, THEN the next
discovery re-pairs deterministically newest pane↔newest file and persists both new session_ids
(the 01/10 01:24 production shape).
Proof: `go test ./internal/monitor/ -run TestPiSource_StaleBindingsRepaired -v`

**C4** - WHEN a bound transcript's mtime is at/after the pane's creation (the pane has written
it) while newer unclaimed files exist, THEN the binding is kept — no theft of a live binding.
Proof: `go test ./internal/monitor/ -run TestPiSource_LiveBindingNotStolen -v`

**C5** - WHEN a bound transcript's mtime predates the pane's creation AND no unclaimed candidate
exists within the pane-age slack, THEN the binding is kept (idle resumed pane).
Proof: `go test ./internal/monitor/ -run TestPiSource_StaleBindingNoCandidateKept -v`

**C6** - WHEN two stale-bound panes share a cwd with exactly one unclaimed candidate, THEN the
newer pane adopts the candidate AND the older pane falls back to its persisted file — no window
ends unbound, no binding is dropped.
Proof: `go test ./internal/monitor/ -run TestPiSource_StaleRepairLoserKeepsOwn -v`

**C7** - The repo builds and the pi + monitor + state suites show no NEW failures at HEAD:
`go build ./...` exits 0 and `go test ./internal/monitor/ ./internal/state/ -count=1` passes
except failures identical at base (recorded below).
Proof: `cd ~/code/maquinista && go build ./...`
Proof: `go test ./internal/monitor/ ./internal/state/ -count=1`

Proof: The 3 repaired DB-backed binding tests pass at HEAD:
`go test ./internal/monitor/ -run 'TestPiSource_DiscoverBackfill|TestPiSource_BindFromEcho|TestPiSource_RebindsToPaneFile' -count=1`

Known-broken at base `f03e05e` (verified by full-suite run 01/10 20:49, worktree base):
`TestPiSource_DiscoverBackfill`, `TestPiSource_BindFromEcho`, `TestPiSource_RebindsToPaneFile`
— `3057b89` switched the pane-age anchor to `agents.started_at` (DEFAULT NOW), while these
fixtures embed hardcoded 2026-09-21 creation names that now lose the 90s backfill filter.
Fixture-coherence repair for these three tests is in scope (this feature's new tests share the
harness); they must be green at HEAD. Also failing at base, NOT in scope (outbox/tool-event flush
path, on record since 30/09): `TestOutboxSink_WritesAssistantText`,
`TestOutboxSink_WritesThinking`, `TestToolEventSink_PairedEmitsBoth`.

Full-suite result at HEAD (01/10 21:0x): identical failure set (exactly those three),
`internal/state` ok, `go build ./...` clean → **no NEW failures**.

Extra defect found while building on the harness (fixed in-scope): the binding loop's
no-change short-circuit (`entry.SessionID == sessionID`) skipped offset seeding, so a bound
window with no tracked offset (fresh monitor state over an already-bound pane) polled from 0
and would relay a resumed transcript's entire history. The unchanged-binding branch now seeds
once by the same epoch rule. Proof: `go test ./internal/monitor/
-run 'TestPiSource_PreEpochSeedsAtSize' -count=1` fails without the fix (ok=false, then
backlog), passes with it.

## Coverage

| Set (size) | Member -> proof | Unproven |
| --- | --- | --- |
| seed outcomes (2) | same-epoch transcript C1 -> TestPiSource_FirstTurnNotSkipped · pre-epoch transcript C2 -> TestPiSource_PreEpochSeedsAtSize | - |
| binding validation (4) | stale + candidate C3 -> TestPiSource_StaleBindingsRepaired · live mtime kept C4 -> TestPiSource_LiveBindingNotStolen · stale no candidate kept C5 -> TestPiSource_StaleBindingKeptWithoutCandidate · repair loser fallback C6 -> TestPiSource_StaleRepairLoserKeepsOwn | - |
| repair side effects (2) | session_ids persisted C3 -> TestPiSource_StaleBindingsRepaired · no unbound window C6 -> TestPiSource_StaleRepairLoserKeepsOwn | - |
| repair trigger (1) | rebind on restart C3-old -> TestPiSource_RebindsToPaneFile phase 2 (seed flips size→0) | - |

- No check claims more than the single case its proof exercises.

## Swept

- validation: n/a - no external input parsing added; candidates come from pi's own session dir
- failure modes: C4 (live binding must survive candidates), C5 (no candidate → no fire), C6 (candidate contention between two stale panes)
- idempotency: C1 (second poll emits nothing); staleness rule re-fires harmlessly once repaired (mtime fresh)
- authorization: n/a - same files the source already reads
- concurrency: C6 (two panes, one candidate — deterministic winner by pane age)
- data lifecycle: n/a - offsets only move; no data deleted
- dependency failure: C5 covers stat/age-unavailable degradation (zero notBefore keeps legacy behavior)
- state transitions: C3 (stale binding → re-paired), C6 (invalidated → fallback)
- observability: C3 (single log line when a stale binding is invalidated and re-paired)

## Out of scope

- Why reconcile's resume didn't take on the 01/10 restart (panes booted fresh) — BUG B makes the
  binding self-heal regardless of the resume path's failure mode; root-causing the resume is a
  separate investigation if it recurs.
- Late-delivery of content accumulated in a re-paired transcript (e.g. the 20:42 "Hola" reply
  surfaces when @3 re-pairs hours later) — accepted: one bounded late relay per healed binding,
  which is what the user is waiting for; a staleness cutoff on pi entries (they carry no
  Timestamp today) would silently re-drop the very replies this fix recovers.
- Claude/openclaude sources — their binding paths don't share pi's transcript model.

## Handoff

Single slice, ~35 KB across 3 files — one builder, no split.
