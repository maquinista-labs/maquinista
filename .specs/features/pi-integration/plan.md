# Pi integration (pi-integration)

Sources:

- `plans/active/pi-integration.md` (PI-00…PI-10) - **binding for the obligations**: task scope, skeletons, acceptance criteria, §3.3 fast path, PI-07 verified env note
- `internal/runner/runner.go` - AgentRunner interface contract (8 methods; α exercises runners exclusively via InteractiveCommand)
- `internal/runner/opencode.go` - the pattern this mirrors (modelFlag: instance > env > baked default)
- maquinista skill `references/runner-integration-playbook.md` - integration checklist, registry-name pitfall
- pi 0.73.1 docs notes (environment-variables.md, verified 21/09/2026) - `PI_MODEL`/`PI_PROVIDER`/`PI_SESSION_ID`/`PI_SESSION_FILE` are pi **outputs** injected into its bash-tool children

## Problem

Maquinista orchestrates coding agents in tmux panes, but only Claude-compatible
and OpenCode harnesses can be spawned: `/runner pi`, `/agent_spawn … pi` and
`/planner pi` fail for the pi CLI (badlogic/pi-mono coding-agent), so the box's
lightest multi-provider TUI (181 MB idle vs opencode 433 MB, bench 21/09) is
unusable from the bot, and its transcripts never fan out to Telegram. Operators
who want a non-Anthropic provider today must rewire OpenCode or hand-edit Go.

When this ships: `/agent_spawn foo pi` runs pi in a tmux window, its session
binds without a hook, and every user/assistant exchange fans out to Telegram.

## Out of scope

| Excluded | Why |
| --- | --- |
| RPC mode runner (`pi --mode rpc`) | Full rewrite of the runner↔monitor contract (plan §6) |
| JSON event-stream subscription | Would retire the file-tail source; separate future task |
| SDK embedding (openclaw style) | Violates "agents are tmux processes" invariant |
| pi extensions / packages / skills | Orthogonal to runner integration |
| Tree-aware (leaf-path) transcript outbox | Linear file-order emission is the documented v1 restriction |
| `--append-system-prompt` planner stacking | Plan PI-08: defer until a real need appears |
| Multi-profile auth rotation | Sandbox layer's responsibility (plan §6) |

## Assumptions

| Assumption | Chosen default | Rationale | Confirmed? |
| --- | --- | --- | --- |
| Runner env-override names | `MAQUINISTA_PI_MODEL` / `MAQUINISTA_PI_PROVIDER` / `MAQUINISTA_PI_THINKING` | PI-07 verified note: bare `PI_MODEL`/`PI_PROVIDER` are pi's own injected outputs — using them as inputs collides and misleads docs | n - raised at review |
| DetectInstallation semantics | `exec.LookPath("pi")` only; provider-key requirement documented in README, not gated in code | Interface doc says "checks binary availability"; keyless gating would hide pi from `/runner` and change contract for all runners | n - raised at review |
| PiProfile starts empty (`SeparatorRunes: nil`, `UIPatterns: nil`), PI-00 probe findings become comments; patterns added when observed in the wild | Plan PI-02 skeleton as written | y |
| §3.3 fast path (PI_SESSION_FILE echo binding) is in scope, with OC-03 cross-reference kept as fallback | Plan §3.3 upgrade text (reviewed, pushed f8f1cfc) | y |
| Baked default model `anthropic/claude-sonnet-4-6` | Plan PI-01 constant | y |
| Live 5-step Telegram QA (PI-06 steps 1–5) runs post-build against the real bot — go-live gate, not a coverage member | Lean rule: live reconciliation is prose follow-up, never a PASS row | n - raised at review |

**Open questions:**

| # | Kind | Question | Until answered |
| --- | --- | --- | --- |
| 1 | blocks go-live | Manual QA: real bot `/runner pi` → `/agent_spawn pi-probe pi` → message reaches pane → session backfilled → fan-out fires (PI-06 steps 1–5 log) | Feature is not switched on as default runner; code may merge first |

## Criteria

Grouped by slice - one observable outcome each, never a layer. Numbering runs
across the whole plan.

### S1: pi is a first-class runner (P1)

**Acceptance Criteria**

1. WHEN the runner registry initializes THEN the system SHALL expose a runner named "pi" via `runner.Get("pi")` and in `runner.Runners()`
2. WHEN `PiRunner.LaunchCommand` is called THEN the returned string SHALL start with `pi` and SHALL NOT contain `--dangerously-skip-permissions` or `OPENCODE_PERMISSION`
3. WHEN `PiRunner.InteractiveCommand` is called with a prompt containing double quotes THEN the returned string SHALL contain `-p` followed by the prompt with its double quotes escaped
4. WHEN `PiRunner.PlannerCommand` is called with a system-prompt path THEN the returned string SHALL contain `--system-prompt "$(cat <path>)"` and SHALL NOT contain `SYSTEM INSTRUCTIONS`
5. WHEN no Model is set on the instance and `MAQUINISTA_PI_MODEL` is unset THEN the generated command SHALL contain `--model "anthropic/claude-sonnet-4-6"`
6. WHEN the instance Model and `MAQUINISTA_PI_MODEL` are both set THEN the generated command SHALL use the instance value
7. WHEN Model contains a `/` (provider-prefixed) and Provider is set THEN the generated command SHALL NOT contain `--provider`
8. WHEN Thinking is set to a level THEN the generated command SHALL contain `--thinking <level>`
9. WHEN `pi` is absent from PATH THEN `DetectInstallation` SHALL return false, and WHEN present it SHALL return true
10. The pi runner SHALL report `HasSessionHook() == false` (regression-guard test: flipping it silently breaks Telegram routing)

**Independent test:** `go test ./internal/runner/ -run TestPiRunner -v` from a clean checkout; each named test asserts one criterion above.

### S2: pi panes are parsed by the shared monitor helpers (P2)

**Acceptance Criteria**

11. The system SHALL provide `monitor.PiProfile()` with nil `SeparatorRunes` and nil `UIPatterns`
12. WHEN the profile-aware helpers (`StripPaneChromeFor`, `ExtractStatusLineFor`, `IsInteractiveUIFor`) run over a captured pi pane sample with `PiProfile()` THEN they SHALL return without error and SHALL NOT classify the pane as interactive UI
13. WHEN the same helpers run with the Claude and OpenCode profiles over their existing golden pane samples THEN their outputs SHALL be unchanged (regression)

**Independent test:** `go test ./internal/monitor/ -run 'TestPiProfile|TestProfiles' -v`.

### S3: pi transcripts fan out (P1)

**Acceptance Criteria**

14. WHEN a pi session file's first line is a v3 session header THEN `PiSource` SHALL extract the header `cwd` and session uuid
15. WHEN `ReadNewEntries` encounters `type:"message"` entries with role user, assistant or toolResult THEN each SHALL produce a `ParsedEntry` preserving the role
16. IF a session line carries an unknown role (e.g. `branch_summary`) or is not valid JSON THEN the reader SHALL classify-or-skip it and SHALL NOT return an error
17. WHEN the same file region is read twice THEN the second pass SHALL emit zero duplicate tool-result entries
18. The cwd slug SHALL equal the directory name pi actually creates for a known cwd, pinned against a real directory under `~/.pi/agent/sessions/`
19. WHEN a session file has grown THEN `ReadNewEntries` SHALL emit only bytes past the stored offset, in file order

**Independent test:** `go test ./internal/monitor/ -run TestPiSource -v` (fixtures committed under `internal/monitor/testdata/pi/`).

### S4: pi is wired into bot/CLI surfaces (P1)

**Acceptance Criteria**

20. WHEN the orchestrator starts THEN a `TranscriptSource` SHALL be registered under the runner name "pi" alongside claude/opencode/openclaude in `cmd/maquinista/cmd_start.go`
21. WHEN `RegisterAgent` is called with runnerType "pi" THEN the value SHALL round-trip unchanged through `agents.runner_type`
22. IF a bot command receives an unknown runner name THEN its error SHALL list available runners derived from `runner.Runners()`, and the repo SHALL contain no new hardcoded `claude, opencode` strings (7 flag help texts + 2 bot error strings + bot.go description derived from the registry)

**Independent test:** `go test ./... -run 'TestRegisterAgent|TestRunnerCommand' -v` plus `rg -n 'claude, opencode' --type go` returning zero hits.

### S5: session binding works without a hook (P2)

**Acceptance Criteria**

23. WHEN a pi agent is spawned THEN a preliminary `session_map` row SHALL exist keyed `<tmuxSession>:<windowID>` with `session_id = agentID` (existing OC-03 hookless path exercised, not reimplemented)
24. WHEN `PiSource.DiscoverSessions` finds a pi session file whose header `cwd` matches a hookless agent's cwd THEN it SHALL backfill `agents.session_id` with the real uuid
25. WHEN the transcript contains the echoed `$PI_SESSION_FILE` path (plan §3.3) THEN the source SHALL bind the real session id from that path

**Independent test:** `go test ./internal/monitor/ -run TestPiSource -v` for 24–25; 23 verified by the existing hookless-path tests plus the live QA gate (open question 1).

### S6: operators can configure and read about pi (P3)

**Acceptance Criteria**

26. The README Runners section SHALL document the pi install command, `MAQUINISTA_DEFAULT_RUNNER=pi`, the `MAQUINISTA_PI_MODEL`/`MAQUINISTA_PI_PROVIDER`/`MAQUINISTA_PI_THINKING` overrides, provider-key env passthrough, and the no-permission-bypass note
27. The pi checklist SHALL be fully ticked (PI-00…PI-10 boxes marked `[x]`) in `plans/active/pi-integration.md`
28. The shipped plan SHALL be indexed in `plans/README.md`

**Independent test:** `rg -n 'MAQUINISTA_PI_MODEL|no permission bypass|@mariozechner/pi-coding-agent' README.md` and `rg -c '\[x\] PI-' plans/active/pi-integration.md`.

## Traceability

| ID | Slice | Criteria | Status |
| --- | --- | --- | --- |
| PIRUN-01 | S1 | 1–10 | Pending |
| PIRUN-02 | S2 | 11–13 | Pending |
| PIRUN-03 | S3 | 14–19 | Pending |
| PIRUN-04 | S4 | 20–22 | Pending |
| PIRUN-05 | S5 | 23–25 | Pending |
| PIRUN-06 | S6 | 26–28 | Pending |

## Observable

Every item of every surface this feature exposes. `n/a` needs its reason.

| Surface | Decision | Landing |
| --- | --- | --- |
| bot `/runner` list | pi appears once registered | AC 1 |
| bot `/runner pi` | switches default runner through the existing generic path | AC 1, AC 21 |
| bot `/agent_spawn <name> pi` | spawns pi TUI in a tmux window, session_map row written | AC 2, AC 23 |
| bot `/planner pi` | planner pane boots with `--system-prompt` persona | AC 4 |
| bot unknown-runner error | lists all registered runners, no stale "claude, opencode" | AC 22 |
| Telegram transcript fan-out | pi user/assistant/toolResult exchanges reach the topic; unknown roles do not drop silently | AC 15, AC 16 |
| README runner docs | install, env contract, sandbox note | AC 26 |
| Telegram transcript fan-out - error shape | n/a - no new error surface; existing outbox error handling reused |
| Telegram transcript fan-out - rate limit | n/a - existing relay/dispatcher throttling unchanged |
| CLI flags/commands | n/a - no new maquinista CLI flags; pi's own flags are generated, not parsed |

## Flow

Reuses instead of duplicating: the tmux-pane spawn path, the OC-03 hookless
session_map fallback, the profile-aware monitor helpers, and the outbox→relay→
Telegram transcript pipeline — pi adds one runner and one source, no new sinks.

1. `/agent_spawn foo pi` -> `internal/bot/agent_commands.go` (exists) - resolves runner via `runner.Get("pi")`
2. -> `internal/runner/pi.go` (new) - builds `pi --model … [-p prompt | --system-prompt "$(cat …)"]` command strings
3. -> tmux pane via `agent.Spawn` (exists) - writes preliminary `session_map` row (hookless path, exists)
4. -> `internal/monitor/terminal.go` (exists; `PiProfile()` added) - status/interactive classification of pane text
5. -> `internal/monitor/source_pi.go` (new) - tails `~/.pi/agent/sessions/<slug>/<uuid>.jsonl`, parses v3 entries, backfills `agents.session_id`
6. -> `internal/monitor/sink_outbox.go` (exists) - `ParsedEntry` → `agent_outbox`
7. out: relay → dispatcher → Telegram (all exist)

## Relations

None - no stored-data shape change. `agents.runner_type` already accepts
arbitrary strings (round-trip verified, AC 21); `session_map` reused as-is.

## Surface

None - nothing consumed outside. The registry key "pi" and the
`MAQUINISTA_PI_*` env names are operator-facing contracts and are recorded as
doors below; no new route, API or payload is introduced (bot commands go
through the existing generic runner path).

## Landing

| One-way door | Literal shape | Alternative rejected |
| --- | --- | --- |
| Registry name "pi" shared by runner + source + `agents.runner_type` | `Register("pi", &PiRunner{})` in `init()`; source registered under the same string in `cmd_start.go`; `loadRunnerSessionMap(ctx, pool, "pi")` | Registering one shared source object under two names - the source's `loadRunnerSessionMap` name must equal `agents.runner_type`, so a name-blind shared object misbinds sessions (playbook pitfall) |
| Operator env override namespace | `MAQUINISTA_PI_MODEL` / `MAQUINISTA_PI_PROVIDER` / `MAQUINISTA_PI_THINKING` (documented in README) | Bare `PI_MODEL`/`PI_PROVIDER`/`PI_THINKING` as inputs - pi 0.73.1 injects `PI_MODEL`/`PI_PROVIDER` into its bash-tool children as outputs; collision makes the docs lie and the env semantics ambiguous |
| Session store layout pin | `~/.pi/agent/sessions/<cwd with "/" → "-">/<uuid>.jsonl`, `$PI_CODING_AGENT_DIR` rebase, pinned by fixture from the live store | Re-deriving the slug algorithm from pi docs - plan PI-03 explicitly pins against a real directory so a pi-side change breaks a test, not production |

- Nothing else in this change is hard to reverse: `PiProfile()` shape follows the `OpenCodeProfile()` precedent, env passthrough is additive, docs are docs.

## Impact

| Front | What changes |
| --- | --- |
| domain | new term: `pi` runner - badlogic/pi-mono coding-agent TUI as a first-class `AgentRunner`, lives in `internal/runner/pi.go` |
| domain | existing term: "available runners" - bot error strings and CLI flag help texts currently hardcode `claude, opencode`; after this they are derived from `runner.Runners()`, so every future runner changes them (branchers today: `cmd_run/spawn/agent/orchestrate/orchestrator/start.go` help texts, `internal/bot/agent_commands.go:79,243`, `bot.go:150`) |
| stored data | nothing to migrate - `runner_type` free string already in place; `session_map` reused |
