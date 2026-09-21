# Pi integration checks

Profile: standard
Plan: `.specs/features/pi-integration/plan.md`

28 checks in 6 slices · 3 one-way doors · 1 open, of which 1 blocks go-live

Known pre-existing failure at base `c5c6b1b`: `TestToolEventSink_PairedEmitsBoth`
(internal/monitor/sink_tool_event_test.go:135) fails in the full-package run.
Unrelated to this feature; every proof below names its own test via `-run`, so
this never gates a proof. Recorded so the Verifier does not misattribute it.

## Checks

### S1 - pi is a first-class runner · 2 files · ~6 KB · ~1.5k

**C1** - The runner registry exposes a runner named "pi" through both `runner.Get("pi")` and `runner.Runners()` (PIRUN-01, AC 1)
Proof: `go test ./internal/runner/ -run TestPiRunner_Registered -v`

**C2** - `PiRunner.LaunchCommand` output starts with `pi` and contains neither `--dangerously-skip-permissions` nor `OPENCODE_PERMISSION` (PIRUN-01, AC 2)
Proof: `go test ./internal/runner/ -run TestPiRunner_LaunchCommand -v`

**C3** - `PiRunner.InteractiveCommand` escapes double quotes inside the `-p` prompt argument (PIRUN-01, AC 3)
Proof: `go test ./internal/runner/ -run TestPiRunner_InteractiveCommand -v`

**C4** - `PiRunner.PlannerCommand` with a system-prompt path emits `--system-prompt "$(cat <path>)"` and never inlines the persona text (no `SYSTEM INSTRUCTIONS` literal) (PIRUN-01, AC 4)
Proof: `go test ./internal/runner/ -run TestPiRunner_PlannerCommand -v`

**C5** - With instance Model unset and `MAQUINISTA_PI_MODEL` unset, the command carries `--model "anthropic/claude-sonnet-4-6"` (PIRUN-01, AC 5)
Proof: `go test ./internal/runner/ -run TestPiRunner_Model -v`

**C6** - Instance Model wins over `MAQUINISTA_PI_MODEL`; the instance-only and env-only combinations resolve to instance and env respectively (PIRUN-01, AC 6)
Proof: `go test ./internal/runner/ -run TestPiRunner_Model -v`

**C7** - A provider-prefixed Model (contains `/`) suppresses `--provider` even when a provider is configured; a bare model keeps `--provider <id>` (PIRUN-01, AC 7)
Proof: `go test ./internal/runner/ -run TestPiRunner_Provider -v`

**C8** - Every configured thinking level (`off`, `minimal`, `low`, `medium`, `high`, `xhigh`) emits `--thinking <level>` (PIRUN-01, AC 8)
Proof: `go test ./internal/runner/ -run TestPiRunner_Thinking -v`

**C9** - `DetectInstallation` returns false when no `pi` binary is on PATH and true when one is (PIRUN-01, AC 9)
Proof: `go test ./internal/runner/ -run TestPiRunner_DetectInstallation -v`

**C10** - `PiRunner.HasSessionHook()` returns false (PIRUN-01, AC 10)
Proof: `go test ./internal/runner/ -run TestPiRunner_HasSessionHook -v`

### S2 - pi panes parsed by shared monitor helpers · 3 files · ~5 KB · ~1.2k

**C11** - `monitor.PiProfile()` returns nil `SeparatorRunes` and nil `UIPatterns` (PIRUN-02, AC 11)
Proof: `go test ./internal/monitor/ -run TestPiProfile_Empty -v`

**C12** - `StripPaneChromeFor`, `ExtractStatusLineFor` and `IsInteractiveUIFor` run over a captured pi pane sample with `PiProfile()` without error and do not classify it as interactive UI (PIRUN-02, AC 12)
Proof: `go test ./internal/monitor/ -run TestPiProfile_HelpersOnPiPane -v`

**C13** - The Claude and OpenCode profiles keep their existing golden-sample behavior through the same helpers (regression) (PIRUN-02, AC 13)
Proof: `go test ./internal/monitor/ -run TestMonitorProfile -v`

### S3 - pi transcripts fan out · 4 files · ~14 KB · ~3.5k

**C14** - `PiSource` extracts the header `cwd` and session uuid from a v3 session-file first line (PIRUN-03, AC 14)
Proof: `go test ./internal/monitor/ -run TestPiSource_Header -v`

**C15** - Every `type:"message"` entry with role user, assistant or toolResult yields a `ParsedEntry` preserving that role (PIRUN-03, AC 15)
Proof: `go test ./internal/monitor/ -run TestPiSource_Roles -v`

**C16** - An unknown role (e.g. `branch_summary`) and an invalid-JSON line are classified-or-skipped and never surface as reader errors (PIRUN-03, AC 16)
Proof: `go test ./internal/monitor/ -run TestPiSource_SkipsUnparseable -v`

**C17** - Reading the same file region twice emits zero duplicate tool-result entries (PIRUN-03, AC 17)
Proof: `go test ./internal/monitor/ -run TestPiSource_NoDupOnReread -v`

**C18** - The cwd slug equals the directory name pi actually creates, pinned to the real store: `/tmp/bench-pi` → `--tmp-bench-pi--`, `/home/otavio/code/maquinista` → `--home-otavio-code-maquinista--` (observed 21/09) (PIRUN-03, AC 18)
Proof: `go test ./internal/monitor/ -run TestPiSource_SlugifyCWD -v`

**C19** - When the session file has grown, `ReadNewEntries` emits only bytes past the stored offset, in file order (PIRUN-03, AC 19)
Proof: `go test ./internal/monitor/ -run TestPiSource_OffsetIncremental -v`

### S4 - pi wired into bot/CLI surfaces · 5 files · ~8 KB · ~2k

**C20** - `cmd/maquinista/cmd_start.go` registers a `TranscriptSource` under "pi" beside claude/opencode/openclaude (PIRUN-04, AC 20)
Proof: `rg -n 'RegisterSource\("pi"' cmd/maquinista/cmd_start.go`

**C21** - `db.RegisterAgent` with runnerType "pi" stores and returns the value unchanged through `agents.runner_type` (PIRUN-04, AC 21)
Proof: `go test ./internal/db/ -run TestRegisterAgent_RunnerTypeRoundTrip -v`

**C22** - The unknown-runner error text is derived from `runner.Runners()` (a freshly registered fake runner appears in it) and no Go source carries the hardcoded string `claude, opencode` (PIRUN-04, AC 22)
Proof: `go test ./internal/bot/ -run TestAvailableRunners -v`
Proof: `! rg -n 'claude, opencode' --type go` (exit 1 = zero hits)

### S5 - session binding without a hook · shared with S3 · ~18 KB · ~4.5k

**C23** - After the spawn-path `RegisterAgent` write, `state.LoadSessionMap` yields the `<tmuxSession>:<windowID>` key for the new agent row, with `session_id` still empty for a hookless runner (agents-table projection, migration 015) (PIRUN-05, AC 23)
Proof: `go test ./internal/db/ -run TestRegisterAgent_SessionMapKeyProjection -v`

**C24** - `PiSource.DiscoverSessions` backfills `agents.session_id` with the real uuid when a pi session header `cwd` matches a hookless agent's cwd (PIRUN-05, AC 24)
Proof: `go test ./internal/monitor/ -run TestPiSource_DiscoverBackfill -v`

**C25** - When the transcript carries the echoed `$PI_SESSION_FILE` path (plan §3.3), the source binds the real session id parsed from that path (PIRUN-05, AC 25)
Proof: `go test ./internal/monitor/ -run TestPiSource_BindFromEcho -v`

### S6 - operators configure and read about pi · 3 files · ~3 KB · ~0.8k

**C26** - README documents the pi install command (`@mariozechner/pi-coding-agent`), `MAQUINISTA_PI_MODEL`/`MAQUINISTA_PI_PROVIDER`/`MAQUINISTA_PI_THINKING` overrides, and the no-permission-bypass sandbox note (PIRUN-06, AC 26)
Proof: `rg -n -e 'MAQUINISTA_PI_MODEL' -e 'no permission bypass' -e '@mariozechner/pi-coding-agent' README.md`

**C27** - The pi checklist is fully ticked: exactly 11 `[x] PI-` boxes and zero `[ ] PI-` boxes in `plans/active/pi-integration.md` (PIRUN-06, AC 27)
Proof: `test "$(rg -c '\[x\] PI-' plans/active/pi-integration.md)" = 11`
Proof: `! rg -n '\[ \] PI-' plans/active/pi-integration.md`

**C28** - The shipped plan is indexed in `plans/README.md` (PIRUN-06, AC 28)
Proof: `rg -n 'pi-integration' plans/README.md`

## Coverage

| Set (size) | Member -> proof | Unproven |
| --- | --- | --- |
| registry key "pi" (3 places) | runner registry C1 · source registration C20 · `agents.runner_type` C21 | - |
| model precedence (4 input combos) | none set C5 · instance+env C6 · instance-only C6 · env-only C6 | - |
| provider flag (2 model shapes) | provider-prefixed model C7 · bare model C7 | - |
| thinking levels (6) | C8, table-driven over all 6 | - |
| profile helpers x profiles (3) | pi C12 · claude C13 · opencode C13 | - |
| pi entry roles (3) | C15, table-driven over all 3 (user, assistant, toolResult) | - |
| unparseable input classes (2) | unknown role C16 · invalid JSON C16 | - |
| offset semantics (2) | reread emits nothing C17 · growth reads suffix only C19 | - |
| session binding paths (2) | hookless discovery backfill C24 · echoed `$PI_SESSION_FILE` C25 | - |
| `MAQUINISTA_PI_*` namespace (3) | MODEL C6 · PROVIDER C7 · THINKING C8 | - |
| derived runner-list sites (9) | C22, table-driven over all 9 (one zero-hits scan + the registry-derived helper test) | - |

- The spawn-to-tmux live path (PI-06 steps 1-5) is the go-live gate (plan open
  question 1), not a coverage member - it needs the real bot and a live pane.
- No check claims more than the cases its proof exercises; C13 reuses the
  existing golden-sample regression tests rather than new copies.

## Test policy

| Code | Required proofs | Coverage expectation |
| --- | --- | --- |
| Decides, reached across a boundary (prompt escaping into a shell command line) | one at its own layer asserting the escaped output string | one asserted case per decision-table row (model 4, provider 2, thinking 6, quote-escaping 1) |
| Decides, not reached across a boundary (source parsing, slug, binding match) | one at its own layer over committed fixtures | one asserted case per row of each decision table (roles 3, unparseable 2, offset 2) |
| Entry point that decides nothing (cmd_start registration, flag definitions, b.reply forwarding) | one boundary proof (command exit code) | registration line present; no behavioral enumeration |
| Instrumentation, pass-throughs | none of its own | covered by the consumer's proof |

Evidence:

- `internal/runner/opencode.go`: model precedence over 3 sources, provider suppression, prompt assembly - decides; already proven at unit layer by `TestOpenCodeRunner_*` with no binary or tmux needed. Same shape, same level for `internal/runner/pi.go`.
- `internal/monitor/source_opencode.go`: file-tail parsing with offset state and classify-or-skip - decides; proven at unit layer in `internal/monitor` with fixtures. Same shape for `internal/monitor/source_pi.go`.
- `internal/db/queries.go` `RegisterAgent` + `internal/state/session_map.go` `LoadSessionMap`: stored-data round-trip - decides; repo harness is `internal/dbtest.PgContainer` (testcontainers Postgres).
- `cmd/maquinista/cmd_start.go` line 219-225 pattern and cobra flag definitions: forward a single call, no conditional - instrumentation; command proof suffices.

Cost: 19 unit proofs across 4 new/extended test files, 2 container-backed DB
proofs, 4 command proofs. Without these rows the three precedence tables and
the role map would be proven only by a path that happens to traverse them.

## Swept

- validation: C3 (prompt text crossing into a shell argument), C16 (malformed session lines)
- failure modes: C9 (binary absent degrades to false), C16 (bad line never kills the reader)
- idempotency: C17 (reread emits zero duplicates)
- authorization: existing - bot commands sit behind the AllowedUsers gate; C2 pins that pi adds no permission-bypass flag (sandbox is the documented operator duty)
- concurrency: existing - one monitor poll loop per source with per-session offset state, unchanged by this feature
- data lifecycle: existing - agents rows and the session_map projection are reused as-is; no new stored data (plan Relations: None)
- dependency failure: C9, C16
- state transitions: C21, C23 (spawn row), C24 (session_id NULL to real uuid), C19 (offset advance)
- observability: n/a - no new logging requirement; outbox error handling reused (plan Observable rows)

## Handoff

Arithmetic: new/modified bytes across S1-S6 ≈ 36 KB ≈ 9k tokens against the
150k budget - a single builder takes the whole feature, no split.

- **Boundary:** C1-C28 closed at `<sha>`
- **Settled mid-build:** none yet
- **Abandoned:** none yet
