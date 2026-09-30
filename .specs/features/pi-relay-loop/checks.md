# Pi relay loop checks

Profile: light

## Intent

The pi transcript relay on barceloneta re-emits the same session entries on every 2s monitor
pass (`new_entries=2` → outbox row → Telegram "42" spam, forever), because `PiSource` is the
only source that does not persist its read offset (`monitor.go:112` discards `newOffset` by
contract; claude/openclaude self-track via `MonitorState.UpdateOffset`). Separately, discovery
bound the agent to a dead smoke-test transcript because pi TUI creates its session file lazily
(~26s after pane start: birth 23:42:55 vs bind at 23:42:45) and the binding is never
re-evaluated once persisted — so the live pane's real session is never relayed.

When this ships: each assistant message reaches Telegram exactly once; an agent whose transcript
appears late (or whose pane is respawned) rebinds to the live transcript without operator
surgery; a partially written last line is neither duplicated nor lost.

2 checks-adjacent defects · 7 checks in 1 slice · 0 one-way doors · 0 open

## Checks

### S1 - pi source self-tracking + live binding · 2 files · ~30 KB · ~8k

**C1** - After a read that consumes bytes, the tracked offset for the session key equals the
consumed byte count, so an immediately repeated poll returns zero entries (offset loop
regression; each named test must appear as `--- PASS`).
Proof: `go test ./internal/monitor/ -run TestPiSource_SelfTracksOffset -v`

**C2** - A transcript whose final line is not newline-terminated yields entries only for
complete lines, leaves the tracked offset at the start of the partial line, and a later read
after the line completes returns exactly that one message — no duplicate, no loss.
Proof: `go test ./internal/monitor/ -run TestPiSource_PartialLineNoDupNoLoss -v`

**C3** - When a pi session file appears after the pane exists (mtime ≥ pane_created) while a
stale binding is persisted, the next discovery rebinds to the newer file, logs the rebind, and
seeds the tracked offset to the new file's size (no backlog relay).
Proof: `go test ./internal/monitor/ -run TestPiSource_RebindsToPaneFile -v`

**C4** - When the pane creation time is unavailable, discovery keeps the current
newest-parseable-file behavior and changes no persisted binding.
Proof: `go test ./internal/monitor/ -run TestPiSource_DiscoverBackfill -v`

**C5** - An echoed `$PI_SESSION_FILE` still overrides a timestamp-derived binding.
Proof: `go test ./internal/monitor/ -run TestPiSource_BindFromEcho -v`

**C6** - C1 holds through the monitor-shaped loop: offset is read from `MonitorState.GetTracked`
and passed back in as `lastOffset` each pass, exactly as `monitor.go:98-105` does — the test
uses that loop, not a manually advanced offset; its second pass asserting zero entries is the
regression body.
Proof: `go test ./internal/monitor/ -run TestPiSource_SelfTracksOffset -v`

**C7** - The repo builds and the pi + monitor + state suites show no NEW failures at HEAD:
`go build ./...` exits 0 and `go test ./internal/monitor/ ./internal/state/ -count=1`
reports only three PRE-EXISTING sink failures
(`TestOutboxSink_WritesAssistantText`, `TestOutboxSink_WritesThinking`,
`TestToolEventSink_PairedEmitsBoth`) — all three fail identically at base (verified via
stash on 30/09; outbox/tool-event flush path, unrelated to this feature, filed separately).
Every named check test must appear as `--- PASS`.

Proof: `cd ~/code/maquinista && go build ./...`
Proof: `go test ./internal/monitor/ ./internal/state/ -count=1`

C1 fails at base (offset discarded → second poll re-emits); C3 fails at base (binding skip at
`source_pi.go:240`). Both were confirmed failing pre-fix; their tests are the negative proofs.

## Coverage

| Set (size) | Member -> proof | Unproven |
| --- | --- | --- |
| read outcomes (3) | bytes consumed C1 · no new bytes C1 · partial final line C2 | - |
| binding candidates (4) | newer-than-pane C3 · stale-only (first bind) C3 · pane age unavailable C4 · echo override C5 | - |
| rebind side effects (3) | session map entry C3 · offset seeded to file size C3 · rebind logged C3 | - |

- No check claims more than the single case its proof exercises.

## Swept

- validation: n/a - no external input parsing added; candidates come from pi's own session dir
- failure modes: C2 (mid-write read), C4 (pane age unavailable)
- idempotency: C1 (repeated poll emits nothing)
- authorization: n/a - no new privilege surface; same files the source already reads
- concurrency: C2 (partial write observed mid-append)
- data lifecycle: n/a - no data deleted; offsets only advance
- dependency failure: C4 (tmux pane age unavailable → legacy behavior)
- state transitions: C3 (bound → rebound, with offset reseed)
- observability: C3 (single log line on rebind)

## Out of scope

- Dispatcher-side outbox dedup (content hashing) — the loop's root cause is upstream; layering
  a second guard changes delivery semantics for all sources.
- Backfill of messages lost during the mis-bound window (23:42-23:48 on barceloneta) — those
  replies exist only in the live transcript and were never relayed; recovery is re-asking.
- `monitor.go:112` `_ = newOffset` — the sibling convention is source-side persistence
  (`source_claude.go:201`, `source_openclaude.go:156`); the discard stays as-is.

## Handoff

Single slice, ~30 KB across 2 files — one builder, no split.
