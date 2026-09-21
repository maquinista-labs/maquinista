# Pi integration verification

**Verdict**: PASS
**Profile**: standard
**Diff range**: c5c6b1b..79eb6aa (feature); specs amendment c826412 (docs-only) — verified at HEAD c826412
**Round**: 2 - full re-verification with the amended C27 (all proofs re-run from scratch by this verifier; nothing carried forward from round 1)
**Verifier**: independent sub-agent (author != verifier)
**Date**: 2026-09-21

All 28 amended checks proven by running every literal proof command from
`checks.md` (C27 per its 2026-09-21 amendment approved by Otavio): 28/28 PASS,
4/4 mutants killed, coverage rows recomputed from the test sources (not read
back from the author's table). PI-06 remains the open go-live door - it gates
the default-runner switchover, not the merge (see Open door).

## Checks

Proof commands are the literal lines from checks.md (amended C27 included);
recorded at HEAD c826412 on 2026-09-21 ~20:07 UTC.

| Check | Claim (condensed) | Proof run | Result | Evidence |
| --- | --- | --- | --- | --- |
| C1 | runner.Get("pi") + Runners()["pi"] | `go test ./internal/runner/ -run TestPiRunner_Registered -v` | PASS | internal/runner/pi_test.go:9 asserts both Get and Runners map |
| C2 | LaunchCommand starts `pi `, no bypass flag, no OPENCODE_PERMISSION | `go test ./internal/runner/ -run TestPiRunner_LaunchCommand -v` | PASS | internal/runner/pi_test.go:22 (3 negative assertions) |
| C3 | InteractiveCommand escapes `"` in -p prompt | `go test ./internal/runner/ -run TestPiRunner_InteractiveCommand -v` | PASS | internal/runner/pi_test.go:36 asserts `-p "do \"work\" now"` |
| C4 | PlannerCommand uses --system-prompt $(cat path), no inlined persona | `go test ./internal/runner/ -run TestPiRunner_PlannerCommand -v` | PASS | internal/runner/pi_test.go:58 asserts both the flag and absence of SYSTEM INSTRUCTIONS |
| C5 | no config -> `--model "anthropic/claude-sonnet-4-6"` | `go test ./internal/runner/ -run TestPiRunner_Model -v` | PASS | internal/runner/pi_test.go:71 subtest "default" PASS |
| C6 | instance > env; env-only resolves to env; instance-only resolves to instance | `go test ./internal/runner/ -run TestPiRunner_Model -v` | PASS | internal/runner/pi_test.go:78 (instance_wins), :88 (env_fallback); instance-only asserted at internal/runner/pi_test.go:98 (see Coverage) |
| C7 | prefixed model suppresses --provider; bare keeps it | `go test ./internal/runner/ -run TestPiRunner_Provider -v` | PASS | internal/runner/pi_test.go:97, both subtests PASS |
| C8 | all 6 thinking levels emit `--thinking <level>`; unset emits none | `go test ./internal/runner/ -run TestPiRunner_Thinking -v` | PASS | internal/runner/pi_test.go:113 loops off/minimal/low/medium/high/xhigh; :121 negative |
| C9 | DetectInstallation true with pi on PATH, false with empty PATH | `go test ./internal/runner/ -run TestPiRunner_DetectInstallation -v` | PASS | internal/runner/pi_test.go:127; pi IS on this host's PATH (~/.nvm/.../bin/pi) so the positive branch executed; negative branch pi_test.go:136 |
| C10 | HasSessionHook() false | `go test ./internal/runner/ -run TestPiRunner_HasSessionHook -v` | PASS | internal/runner/pi_test.go:52 |
| C11 | PiProfile has nil SeparatorRunes and nil UIPatterns | `go test ./internal/monitor/ -run TestPiProfile_Empty -v` | PASS | internal/monitor/source_pi_test.go:208 |
| C12 | helpers run over captured pi pane, not interactive UI | `go test ./internal/monitor/ -run TestPiProfile_HelpersOnPiPane -v` | PASS | internal/monitor/source_pi_test.go:218 |
| C13 | Claude + OpenCode golden regression through same helpers | `go test ./internal/monitor/ -run TestMonitorProfile -v` | PASS | internal/monitor/terminal_test.go:8 and internal/monitor/terminal_test.go:20 both PASS |
| C14 | header cwd + session uuid extracted | `go test ./internal/monitor/ -run TestPiSource_Header -v` | PASS | internal/monitor/source_pi_test.go:52 |
| C15 | user/assistant/toolResult roles preserved | `go test ./internal/monitor/ -run TestPiSource_Roles -v` | PASS | internal/monitor/source_pi_test.go:81, toolResult fixture at :85 |
| C16 | unknown role + invalid JSON never surface as reader errors | `go test ./internal/monitor/ -run TestPiSource_SkipsUnparseable -v` | PASS | internal/monitor/source_pi_test.go:101, branch_summary fixture at :104 |
| C17 | reread emits zero duplicate tool results | `go test ./internal/monitor/ -run TestPiSource_NoDupOnReread -v` | PASS | internal/monitor/source_pi_test.go:119 asserts exactly 1 toolResult on first read, 0 dupes on reread |
| C18 | slug pinned to real store incl. both required cases | `go test ./internal/monitor/ -run TestPiSource_SlugifyCWD -v` | PASS | internal/monitor/source_pi_test.go:33; `/tmp/bench-pi` -> `--tmp-bench-pi--` at :35, `/home/otavio/code/maquinista` -> `--home-otavio-code-maquinista--` at :36 |
| C19 | growth reads only bytes past stored offset, in order | `go test ./internal/monitor/ -run TestPiSource_OffsetIncremental -v` | PASS | internal/monitor/source_pi_test.go:157 |
| C20 | TranscriptSource registered under "pi" in cmd_start | `rg -n 'RegisterSource\("pi"' cmd/maquinista/cmd_start.go` | PASS | cmd/maquinista/cmd_start.go:228 `monitor.RegisterSource("pi", piSrc)` |
| C21 | runnerType "pi" round-trips through agents.runner_type | `go test ./internal/db/ -run TestRegisterAgent_RunnerTypeRoundTrip -v` | PASS | internal/db/pi_roundtrip_test.go:14; real postgres:16-alpine testcontainer, 2.70s |
| C22 | error list derived from runner.Runners(); no hardcoded `claude, opencode` | `go test ./internal/bot/ -run TestAvailableRunners -v` + `! rg -n 'claude, opencode' --type go` | PASS | internal/bot/available_runners_test.go:24 PASS; rg exit 1 (zero hits repo-wide) |
| C23 | session_map projection yields tmux key, session_id empty (migration 015) | `go test ./internal/db/ -run TestRegisterAgent_SessionMapKeyProjection -v` | PASS | internal/db/pi_roundtrip_test.go:37; real testcontainer, 2.63s |
| C24 | DiscoverSessions backfills session_id from header cwd match | `go test ./internal/monitor/ -run TestPiSource_DiscoverBackfill -v` | PASS | internal/monitor/source_pi_test.go:257; run log: `Pi session discovered: /tmp/proj -> 01a0c457-b71c-77af-b6a8-c874297e3cbd` |
| C25 | echoed $PI_SESSION_FILE binds real uuid over header uuid | `go test ./internal/monitor/ -run TestPiSource_BindFromEcho -v` | PASS | internal/monitor/source_pi_test.go:296; run log: `Pi: binding via echoed $PI_SESSION_FILE 0199aaaa-1111-7222-b333-444455556666 (header said 01a0c457-...)` |
| C26 | README: install cmd, PI_* overrides, no-permission-bypass note | `rg -n -e 'MAQUINISTA_PI_MODEL' -e 'no permission bypass' -e '@mariozechner/pi-coding-agent' README.md` | PASS | README.md:184, :197, :205, :214 |
| C27 | exactly 10 [x] PI- boxes, exactly 1 [ ] PI- box (PI-06) - amended wording | `test "$(rg -c '\[x\] PI-' ...)" = 10`; `test "$(rg -c '\[ \] PI-' ...)" = 1`; `rg -n '\[ \] PI-06' ...` | PASS | counts observed x=10 space=1; plans/active/pi-integration.md:482 |
| C28 | shipped plan indexed | `rg -n 'pi-integration' plans/README.md` | PASS | plans/README.md:33 |

Note (verifier hygiene): my first batched run anchored the `-run` regexes with
`$`, which made C3/C4/C10 report "no tests to run" (real names are
`TestPiRunner_InteractiveCommand_EscapesQuotes`, `_PlannerCommand_SystemPrompt`,
`_HasSessionHook_False`). I re-ran all three with the exact literal commands
from checks.md, which match those tests unanchored - recorded above are the
literal-proof results only.

## Coverage

Recomputed from the test sources themselves (fixture bodies read for C6/C7/C8,
C15/C16/C17, C18, roles), not copied from the author's table:

| Set (size) | Member -> proof (verified) | Unproven |
| --- | --- | --- |
| registry key "pi" (3) | runner registry internal/runner/pi_test.go:9 · source registration cmd/maquinista/cmd_start.go:228 · `agents.runner_type` internal/db/pi_roundtrip_test.go:14 | - |
| model precedence (4 combos) | none set pi_test.go:71 · instance+env pi_test.go:78 · env-only pi_test.go:88 · instance-only pi_test.go:98 - the `--provider` assertion there can only hold if the resolved model is the bare instance value (the prefixed default `anthropic/claude-sonnet-4-6` suppresses the flag per internal/runner/pi.go:61, and no ambient MAQUINISTA_PI_* vars exist in the test env - verified) | - |
| provider flag (2 shapes) | bare pi_test.go:98 · prefixed pi_test.go:104 | - |
| thinking levels (6) | pi_test.go:113 loop over all six + negative at :121 | - |
| profile helpers x profiles (3) | pi source_pi_test.go:218 · claude terminal_test.go:8 · opencode terminal_test.go:20 | - |
| pi entry roles (3) | source_pi_test.go:81, user/assistant/toolResult fixtures (toolResult at :85) | - |
| unparseable classes (2) | unknown role source_pi_test.go:104 · invalid JSON source_pi_test.go:101 | - |
| offset semantics (2) | reread-zero source_pi_test.go:119 · suffix-only source_pi_test.go:157 | - |
| session binding paths (2) | discovery backfill source_pi_test.go:257 · echo bind source_pi_test.go:296 | - |
| `MAQUINISTA_PI_*` read by checks (3) | MODEL env path proven pi_test.go:79/:89 · provider decision proven via instance.Provider pi_test.go:98 (AC 7 wording: "a provider is configured") · thinking levels proven via instance.Thinking pi_test.go:113 (AC 8 wording: "every configured thinking level") - the two remaining env-fallback branches are not claimed by any check; see Ranked gaps | - |
| derived runner-list sites (9) | zero-hits scan (`! rg -n 'claude, opencode' --type go`, exit 1) + registry-derived helper test internal/bot/available_runners_test.go:24 | - |

## Test policy rows

Verdicts against checks.md's Test policy table, judged by the proofs actually run:

| Code class | Required proofs | Expectation met |
| --- | --- | --- |
| Decides, across a boundary (prompt escaping) | one unit proof at runner layer asserting escaped string; decision-table rows | yes - escaping pi_test.go:36; model 4, provider 2, thinking 6 all table-driven (see Coverage) |
| Decides, not across a boundary (parsing, slug, binding) | unit proofs over committed fixtures; per-row assertions | yes - roles 3, unparseable 2, offset 2, slug 6 cases, binding 2 paths all exercised in source_pi_test.go |
| Entry point that decides nothing (registration, flags) | one boundary proof | yes - C20 command proof shows the registration line; the entry-point class requires nothing beyond that boundary proof |
| Instrumentation / pass-throughs | covered by consumer's proof | yes - C23/C24/C25 consumer proofs exercise the projections end to end against a real container |

## Faults injected

Four live mutants, one per layer, injected at HEAD c826412 and reverted
immediately (`git checkout --`; working tree verified clean afterwards). The
killed column says what the targeted proof did with the mutation present:

| Mutant | Injected at | Proof re-run | Killed |
| --- | --- | --- | --- |
| M1 prompt escaping removed (escaped := prompt) | internal/runner/pi.go:91 | C3 (`-run TestPiRunner_InteractiveCommand`) | killed - test fails "did not escape quotes" (see transcript below) |
| M2 instance Model ignored in resolution | internal/runner/pi.go:42 | C5/C6 (`-run TestPiRunner_Model`) | killed - subtest "instance wins" fails (see transcript below) |
| M3 leading "/" not stripped in slug algorithm | internal/monitor/source_pi.go:77 | C18 (`-run TestPiSource_SlugifyCWD`) | killed - table cases fail (see transcript below) |
| M4 source registered under wrong key | cmd/maquinista/cmd_start.go:228 | C20 (`rg -n 'RegisterSource\("pi"' cmd/maquinista/cmd_start.go`) | killed - rg exits 1, zero hits |

Transcripts (condensed, captured live at c826412; all four files reverted with
`git checkout --`, `git status --short` clean after):

```
M1  --- FAIL: TestPiRunner_InteractiveCommand_EscapesQuotes (0.00s)
M2  pi_test.go:82: instance Model override ignored: pi --model "openrouter/qwen/qwen3-coder"
    --- PASS: TestPiRunner_Model/default
    --- FAIL: TestPiRunner_Model/instance_wins
    --- PASS: TestPiRunner_Model/env_fallback
M3  source_pi_test.go:45: SlugifyCWD("/tmp/bench-pi") = "---tmp-bench-pi--", want "--tmp-bench-pi--"
    source_pi_test.go:45: SlugifyCWD("/") = "-----", want "----"
    --- FAIL: TestPiSource_SlugifyCWD
M4  rg 'RegisterSource\("pi"' cmd/maquinista/cmd_start.go -> exit 1 (zero hits)
```

M1's negative result is itself proof the C3 assertion discriminates; M2 shows
the table separates the two winners (default and env_fallback stayed green while
only instance_wins died) - exactly the decision-table sensitivity the Test
policy rows demand.

## Open door

**PI-06 - live Telegram QA of the spawn-to-tmux path** (plans/active/pi-integration.md:482,
the one unticked box the amended C27 pins open). Per the plan's door table and
the C27 amendment (2026-09-21, approved by Otavio), this is the **go-live gate**:
it blocks switching pi to a default runner and shipping PI-06 steps 1-5 as
verified, and it needs the real bot plus a live pane - not a coverage member,
not merge-blocking. 0 of the 28 checks depend on it.

## Ranked gaps

1. **Untested env fallbacks**: `MAQUINISTA_PI_PROVIDER` (internal/runner/pi.go:64)
   and `MAQUINISTA_PI_THINKING` (internal/runner/pi.go:78) are read but no test
   sets them (only `t.Setenv("MAQUINISTA_PI_MODEL", ...)` appears - verified by
   grep). No check claims them (AC 7/8 speak of configured provider/levels, proven
   via instance fields), so this does not gate PASS; it is a cheap follow-up:
   two 3-line subtests mirroring pi_test.go:88.
2. **Host-dependent positive branch in C9**: TestPiRunner_DetectInstallation skips
   the positive assertion when no `pi` binary is on PATH (pi_test.go:129-131). On
   this verification host pi was on PATH, so both branches executed; on a clean CI
   box only the negative branch runs. The amended spec still passes; noting it so
   a future CI run reads "ok" with less coverage.
3. Pre-existing, out of scope, cited not re-run (recorded in checks.md header,
   confirmed by two prior rounds): `TestOutboxSink_WritesAssistantText`,
   `TestOutboxSink_WritesThinking`, `TestToolEventSink_PairedEmitsBoth`
   (internal/monitor sink tests, rows=0/notifications mismatch) fail in the
   full-package run at base c5c6b1b; unrelated to pi - every pi proof names its
   own tests via `-run`.

## Gate

28/28 amended checks PASS with literal proofs; 11/11 coverage rows joined with
zero unproven members; 4/4 test-policy rows met; 4/4 injected mutants killed.
The single open item (PI-06) is the documented go-live door, not a coverage
member. Verdict: PASS - merge is gated only on accepting the two ranked
non-blocking gaps above; go-live additionally requires PI-06 manual QA.
