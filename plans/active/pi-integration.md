# Pi Agent Integration

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

Adds the **`pi`** coding agent (badlogic / Mario Zechner — [`@mariozechner/pi-coding-agent`](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent)) as a first-class runner alongside Claude Code, OpenClaude and OpenCode.

The model mirrors `opencode-integration.md`: implement the `runner.AgentRunner` interface, give it its own `monitor.MonitorProfile`, and add a `TranscriptSource` that reads pi's JSONL session files.

---

## 1. Why Pi

Pi is a minimal, extensible coding-agent TUI that ships four modes (interactive / print / `--mode json` / `--mode rpc`) and a real SDK. Relative to the runners we already have:

| Aspect | Claude Code | OpenCode | OpenClaude | **Pi** |
|---|---|---|---|---|
| Binary | `claude` | `opencode` | `openclaude` | **`pi`** (npm global) |
| Permission bypass | `--dangerously-skip-permissions` | `OPENCODE_PERMISSION=skip` | `--dangerously-skip-permissions` | **none by design** — run in sandbox/container |
| System prompt flag | `--system-prompt` | ✗ (has to be faked) | `--system-prompt` | **`--system-prompt` + `--append-system-prompt`** |
| Session storage | `~/.claude/projects/<cwd>/<uuid>.jsonl` | SQLite at `~/.local/share/opencode/opencode.db` | `~/.claude/…` (Claude-compatible) | **`~/.pi/agent/sessions/<cwd-slug>/<uuid>.jsonl`** |
| Session hook | SessionStart hook → writes our `session_map` | none | SessionStart hook | **none** — fallback path (like OpenCode) |
| Session resume | `--session <uuid|file>` / `-c` / `-r` | own session id | Claude-compatible | **`--session <uuid|file>` / `-c` / `-r`** |
| Non-interactive | `-p "prompt"` | `opencode run "prompt"` | `-p "prompt"` | **`-p "prompt"`** |
| Transcript format | JSONL, linear | SQLite tables | Claude-compatible JSONL | **JSONL, tree (id/parentId)** |
| Model selection | `--model sonnet` | `--model provider/id` | `--model` | **`--model <pattern>` + `--provider <id>`** |
| Thinking control | implicit via model | n/a | implicit | **`--thinking off|minimal|low|medium|high|xhigh`** |
| Providers supported | Anthropic | OpenRouter, Anthropic, … | Anthropic-compatible | **Anthropic, OpenAI, Google, Bedrock, Vertex, Groq, Cerebras, xAI, OpenRouter, Vercel AI Gateway, Ollama, Hugging Face, Kimi, MiniMax, …** |
| Extensibility | Skills, Hooks, MCP | Plugins | Claude-compatible | **TS extensions + skills + prompt templates + themes + pi-packages (npm/git)** |

Pi covers the "we want a multi-provider, no-fuss CLI runner we can drop behind tmux" gap. It also gives us a practical route into the broader multi-provider ecosystem without rewiring OpenCode to do it.

### Reference integrations we're learning from

- **openclaw** (`/home/otavio/code/openclaw/docs/pi.md`, `packages/coding-agent/`): openclaw **embeds** pi via `@mariozechner/pi-coding-agent`'s SDK — `createAgentSession()`, `SessionManager`, multi-profile auth rotation, custom tool injection. That's appropriate for a messaging gateway that hosts the agent loop in-process. **Maquinista does not want this.** We are a tmux/subprocess orchestrator — agents live as TUI processes the human can attach to. So we integrate pi the same way we integrate Claude: as an `AgentRunner` that launches the CLI in a tmux window.
- **pi-mono docs** (`packages/coding-agent/docs/`): authoritative for CLI flags, RPC framing, and the JSONL session file format (see §A below).

---

## 2. Problem Summary

| Gap | Severity | Impact |
|---|---|---|
| No `PiRunner` implementing `runner.AgentRunner` | High | `/runner pi` and `/agent_spawn … pi` fail. |
| No monitor `MonitorProfile` for pi's TUI | High | Status detection, interactive-UI detection broken for pi panes. |
| No `TranscriptSource` for pi's JSONL sessions | High | Telegram transcript fan-out does not fire for pi agents. |
| No session-map discovery path for hook-less runners (pi) | Medium | OC-03 landed the generic fallback; we reuse it. Must be verified for pi. |
| No model / provider / thinking-level defaults in config | Medium | Operators have to hand-edit everything. |
| No tests for pi-specific wiring | Medium | Future edits to `runner.go` will silently regress pi. |

---

## 3. Architectural Notes

### 3.1 Subprocess, not SDK

openclaw embeds pi via `createAgentSession`. Maquinista runs each agent as its own tmux window: the human (or the bot) can `tmux attach` and interact, and the orchestrator tails transcripts out-of-band. That model needs a **CLI wrapper**, not a library import. No Node process in the Go binary. `PiRunner` returns shell command strings the same way `ClaudeRunner` does.

### 3.2 Permissions

Pi has **no permission flow and no bypass flag**. The design expects sandboxing to live one level up (container / bwrap / firejail / plain user namespace). Because maquinista launches agents inside the user's trusted dev environment, this is acceptable. The `LaunchCommand` is therefore simpler than claude/opencode — no env var, no flag. Document this explicitly in `PiRunner` so nobody reintroduces `--dangerously-skip-permissions` by reflex.

### 3.3 Session hook

Pi has no hook mechanism analogous to Claude Code's `SessionStart`. `HasSessionHook()` returns `false` — same fallback path OC-03 uses for OpenCode:

1. On `Spawn`, we write a preliminary `session_map` row keyed `<tmuxSession>:<windowID>` with `session_id = agentID` (stable proxy).
2. `PiSource.DiscoverSessions` later cross-references `~/.pi/agent/sessions/<cwd-slug>/*.jsonl` to find the real UUID, and backfills `agents.session_id`.

### 3.4 Transcript format

Pi session files are line-delimited JSON (**v3 format**) with a tree shape (`id` / `parentId`) rather than a flat list. Important implications:

- The first line is a **`SessionHeader`** — `{"type":"session","version":3,"id":"uuid","timestamp":"…","cwd":"…"}` — which anchors the discovery cross-reference (we match `cwd` against `agents.cwd`).
- Subsequent lines are `{"type":"message", …}`, `{"type":"compaction", …}`, `{"type":"branch_summary", …}`, `{"type":"custom", …}`, etc. See §A.
- Byte-offset tailing works fine because the file is append-only per session; the tree structure matters for *context building*, not for *transcript fan-out*. Maquinista's monitor cares about the latter.
- `version: 1` and `version: 2` are auto-migrated by pi on open, so in practice we see v3.

### 3.5 Hook hostility

Pi loads `AGENTS.md` / `CLAUDE.md` from the cwd (walking up) at startup. Our bootstrap writes an agent-specific `CLAUDE.md` into the worktree (see `agent/agent.go`), so pi will pick it up for free — no extra wiring needed.

---

## 4. Tasks

### PI-00 — Observe Pi's TUI output in a tmux pane

**Action:** `tmux new -d -s pi-probe "pi"`, then drive it through: idle, busy (with tool call), compaction, `/login` prompt, `/tree`, `/settings`. Capture `tmux capture-pane -p -t pi-probe` snapshots at each state. Identify:

- Spinner / "thinking" indicator characters (if any — pi uses its own TUI lib, `pi-tui`).
- Chrome separator pattern (pi draws a footer with cwd / session / tokens / cost / model).
- Interactive-prompt markers (`/login` provider picker, `/model` picker, `/tree` tree view, `/compact` progress).
- Message queue indicators (steering vs. follow-up, visible in the editor border).

Document findings as comments on `PiProfile()` (PI-02) and `PiSource.IsInteractiveUI` (PI-03).

**Acceptance:** Observations checked in as comments. No behavioral change yet.

---

### PI-01 — `PiRunner` implementing `AgentRunner`

**File:** `internal/runner/pi.go` (new), `internal/runner/pi_test.go` (new).

```go
package runner

import (
    "fmt"
    "os"
    "os/exec"
    "strings"

    "github.com/maquinista-labs/maquinista/internal/monitor"
)

type PiRunner struct {
    // Model is an optional model pattern or ID. Empty = pi picks from its
    // settings.json / active profile. Supports the "provider/id:thinking"
    // shorthand (e.g. "anthropic/claude-sonnet-4-6:high").
    Model string
    // Provider is an optional provider slug (anthropic, openai, google, ...).
    // Overridden by a "provider/" prefix in Model.
    Provider string
    // Thinking is an optional thinking level: off|minimal|low|medium|high|xhigh.
    Thinking string
}

func init() { Register("pi", &PiRunner{}) }

const defaultPiModel = "anthropic/claude-sonnet-4-6"

func (p *PiRunner) Name() string { return "pi" }

// flags assembles the provider/model/thinking flags common to all invocations.
// Honors env overrides (PI_MODEL, PI_PROVIDER, PI_THINKING) before falling
// back to the built-in default. Kept as a private helper so Launch/Interactive/
// Planner all agree.
func (p *PiRunner) flags() string {
    var parts []string
    model := strings.TrimSpace(p.Model)
    if model == "" {
        if env := strings.TrimSpace(os.Getenv("PI_MODEL")); env != "" {
            model = env
        }
    }
    if model == "" {
        model = defaultPiModel
    }
    parts = append(parts, fmt.Sprintf("--model %q", model))

    provider := strings.TrimSpace(p.Provider)
    if provider == "" {
        provider = strings.TrimSpace(os.Getenv("PI_PROVIDER"))
    }
    // Only add --provider when Model does NOT already carry a provider prefix.
    if provider != "" && !strings.Contains(model, "/") {
        parts = append(parts, fmt.Sprintf("--provider %q", provider))
    }

    thinking := strings.TrimSpace(p.Thinking)
    if thinking == "" {
        thinking = strings.TrimSpace(os.Getenv("PI_THINKING"))
    }
    if thinking != "" {
        parts = append(parts, fmt.Sprintf("--thinking %s", thinking))
    }
    return strings.Join(parts, " ")
}

func (p *PiRunner) LaunchCommand(cfg Config) string {
    // Pi has no permission-bypass flag — by design. It assumes you ran it in
    // a sandbox. We rely on the wider user environment to provide isolation.
    return fmt.Sprintf("pi %s", p.flags())
}

func (p *PiRunner) InteractiveCommand(prompt string, cfg Config) string {
    escaped := strings.ReplaceAll(prompt, "\"", "\\\"")
    return fmt.Sprintf("pi %s -p \"%s\"", p.flags(), escaped)
}

// PlannerCommand uses pi's --system-prompt (replaces the default system prompt).
// Pi still appends AGENTS.md / CLAUDE.md / skills after the override, matching
// what `claude --system-prompt "$(cat ...)"` gives us.
func (p *PiRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
    return fmt.Sprintf("pi %s --system-prompt \"$(cat %s)\"", p.flags(), systemPromptPath)
}

func (p *PiRunner) DetectInstallation() bool {
    _, err := exec.LookPath("pi")
    return err == nil
}

func (p *PiRunner) EnvOverrides() map[string]string {
    // PI_CODING_AGENT_DIR pins pi's config/session root so it doesn't drift
    // between users when multiple accounts run on the same box. Empty map =
    // use pi's default (~/.pi/agent).
    return map[string]string{}
}

func (p *PiRunner) HasSessionHook() bool { return false }

func (p *PiRunner) MonitorProfile() monitor.MonitorProfile { return monitor.PiProfile() }
```

Tests (`pi_test.go`) mirror `opencode_test.go`:

- `TestPiRunner_Name`
- `TestPiRunner_InteractiveCommand` — asserts `-p` and prompt interpolation.
- `TestPiRunner_LaunchCommand_NoPermissionFlag` — explicitly asserts the command does **not** contain `--dangerously-skip-permissions` or `OPENCODE_PERMISSION` (regression guard for 3.2).
- `TestPiRunner_ModelDefault` — asserts `--model "anthropic/claude-sonnet-4-6"` appears when nothing set, and that `Model` instance override wins.
- `TestPiRunner_ModelWithProviderPrefix_NoRedundantProvider` — `Model: "openai/gpt-4o"`, `Provider: "anthropic"` → no `--provider` in the output.
- `TestPiRunner_ThinkingLevel` — `Thinking: "high"` produces `--thinking high`.
- `TestPiRunner_PlannerCommand_UsesSystemPromptFlag` — asserts `--system-prompt "$(cat …)"` and the path interpolates. Asserts the word `SYSTEM INSTRUCTIONS` is **not** present (that's OpenCode's role-framing workaround; pi doesn't need it).
- `TestPiRunner_Registered` — `Get("pi")` succeeds, name matches.
- `TestPiRunner_HasSessionHook_False` — regression guard: if anyone flips this to `true`, the Spawn path will skip the preliminary session_map write and break Telegram routing for pi.

**Acceptance:** `go test ./internal/runner/...` green. `/runner pi` in the bot switches the default runner and `/agent_spawn foo pi` spawns.

---

### PI-02 — `PiProfile()` in `internal/monitor/terminal.go`

Add a fourth profile alongside `ClaudeProfile`, `OpenCodeProfile`, and (implicitly) OpenClaude:

```go
// PiProfile returns the Pi TUI parsing parameters. Pi uses its own TUI library
// (pi-tui) with differential rendering; the footer line shows cwd, session
// name, token/cache usage, cost, and active model. The editor border's color
// signals thinking level but carries no separator rune we can key on.
//
// Populated from observations in PI-00.
func PiProfile() MonitorProfile {
    return MonitorProfile{
        // TBD from PI-00. Likely "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏" (Braille spinner) or "·•" style.
        SpinnerChars: "",
        // Pi's footer is a status line not a rule; no single rune defines it.
        // Leave empty to use the "never match separator" branch.
        SeparatorRunes: nil,
        MinSeparatorLen: 0,
        // UIPatterns: /login provider picker, /model selector, /tree view,
        // permission-less approval UIs from extensions. Start nil; extend as
        // we meet them in the wild.
        UIPatterns: nil,
    }
}
```

`StripPaneChromeFor`, `ExtractStatusLineFor`, `IsInteractiveUIFor` already handle empty `SeparatorRunes` and nil `UIPatterns` correctly (see `OpenCodeProfile`), so no changes to the profile-aware functions are needed.

**Acceptance:** `go test ./internal/monitor/...` green. Claude / OpenCode / OpenClaude profiles unchanged.

---

### PI-03 — `PiSource` implementing `TranscriptSource`

**File:** `internal/monitor/source_pi.go` (new).

Mirrors `source_claude.go` because both tail JSONL session files. Differences from Claude:

1. **Session path layout**: `~/.pi/agent/sessions/<slugified-cwd>/<uuid>.jsonl` (slashes in cwd replaced with `-`). Slugify the same way pi does (see `packages/coding-agent/src/core/session-manager.ts`). When `$PI_CODING_AGENT_DIR` is set, rebase to that.
2. **Header line**: first line is the `SessionHeader` (`{"type":"session","version":3,…,"cwd":"…"}`). Match `cwd` to `agents.cwd` to bind a file to an agent; once bound, write the UUID back to `agents.session_id` via the generic `loadRunnerSessionMap` path.
3. **Entry parsing**: most maquinista fan-out cares about `{"type":"message", "message": {"role": "user"|"assistant"|"toolResult"|…}}`. Extended roles (`bashExecution`, `custom`, `branchSummary`, `compactionSummary`) exist — map unknown roles to an `OtherEntry` so the outbox doesn't drop them silently but also doesn't error.
4. **Tree shape**: maquinista's transcript outbox is linear. The simplest working approach is to treat the JSONL as an append-only log (byte-offset tail) and emit entries in file order, **ignoring** `parentId` branches. `/tree` use in pi is rare and Telegram routing doesn't need to render it. Document this restriction; a future task can teach the outbox to prefer the leaf path.
5. **Tool-call dedup**: like Claude's source, track `(toolCallId → last emitted status)` so we don't re-emit identical tool results when the session file is reread.

Skeleton:

```go
type PiSource struct {
    config         *config.Config
    pool           *pgxpool.Pool
    appState       *state.State
    monitorState   *state.MonitorState
    sessionsRoot   string // $PI_CODING_AGENT_DIR/sessions, default ~/.pi/agent/sessions
    pendingTools   map[string]PendingTool
    fileMtimes     map[string]time.Time
    lastSessionMap map[string]state.SessionMapEntry
}

func NewPiSource(cfg *config.Config, pool *pgxpool.Pool, st *state.State, ms *state.MonitorState) *PiSource { … }
func (p *PiSource) Name() string { return "pi" }
func (p *PiSource) DiscoverSessions() []ActiveSession { /* walk sessionsRoot, cross-reference with loadRunnerSessionMap(ctx, pool, "pi") */ }
func (p *PiSource) ReadNewEntries(sess ActiveSession, offset int64) ([]ParsedEntry, int64, error) { /* byte-offset JSONL tail */ }
func (p *PiSource) ExtractStatusLine(paneText string) (string, bool) { return ExtractStatusLineFor(paneText, PiProfile()) }
func (p *PiSource) IsInteractiveUI(paneText string) bool { return IsInteractiveUIFor(paneText, PiProfile()) }
```

Register in the monitor init path wherever `ClaudeSource`, `OpenCodeSource`, `OpenClaudeSource` are registered. (Grep `RegisterSource(`.)

Unit tests:

- `TestPiSource_HeaderParse` — feed a `{"type":"session", …}` line, confirm cwd is extracted.
- `TestPiSource_MessageParse` — user/assistant/toolResult lines parsed into `ParsedEntry`.
- `TestPiSource_SlugifyCWD` — `/home/otavio/code/maquinista` → the expected slug. **Pin against an actual pi-generated directory from `~/.pi/agent/sessions/`** — don't re-derive the algorithm from the docs. If pi ever changes it, this test is how we catch it.
- `TestPiSource_UnknownRoleNotFatal` — a `branch_summary` entry doesn't error the reader.
- `TestPiSource_ToolResultDedup` — re-reading the same file yields zero emissions on the second pass.

**Acceptance:** A pi agent spawned in a worktree produces Telegram transcript output for every user/assistant exchange.

---

### PI-04 — DB / state wiring for `runner_type = "pi"`

**Files:** `internal/db/queries.go`, `internal/state/state.go`, `internal/config/config.go`, migrations if needed.

1. `RegisterAgent` already accepts arbitrary `runnerType` — no schema change. **Verify** with a quick integration test that `runnerType = "pi"` round-trips through `agents.runner_type`.
2. `state.GetWindowRunner` defaults to `"claude"` — leave as-is. Windows for pi agents get `runner_type = "pi"` set explicitly at spawn time.
3. `config.Config.DefaultRunner` already reads `MAQUINISTA_DEFAULT_RUNNER`. Document `pi` as a valid value in `README.md`.
4. `loadRunnerSessionMap(ctx, pool, "pi")` — the generic loader in `internal/monitor/` already accepts the runner name as a parameter; confirm nothing hard-codes `"claude" | "opencode"`.

**Acceptance:** `grep -rn '"claude"\|"opencode"' internal/` yields no new hits after this task — all runner-name plumbing goes through the registry.

---

### PI-05 — Telegram bot commands

**Files:** `internal/bot/agent_commands.go`, `internal/bot/window_picker.go`, `internal/bot/directory_browser.go`, `internal/bot/planner_commands.go`.

The existing `/runner` handler already iterates `runner.Runners()` — it will list `pi` automatically once PI-01 lands. The hardcoded error strings ("Available: claude, opencode") need a **one-line fix**:

```go
// was: "Available: claude, opencode"
// now: derive from runner.Runners()
available := strings.Join(maps.Keys(runner.Runners()), ", ")
b.reply(chatID, threadID, fmt.Sprintf("Unknown runner %q. Available: %s", args[1], available))
```

Apply the same fix anywhere `"claude, opencode"` is hardcoded (`grep -rn 'claude, opencode'`).

`/agent_spawn <name> pi [role]` works through the generic path once PI-01 registers pi.

**Acceptance:** `/runner`, `/runner pi`, `/agent_spawn foo pi`, and `/planner pi` all work end-to-end against a real pi install.

---

### PI-06 — Session-tracking fallback verified for pi

OC-03 already landed the generic `HasSessionHook() == false` fallback: after `Spawn`, write a preliminary session_map entry keyed on `<tmuxSession>:<windowID>` with `session_id = agentID`. This task is **verification**, not implementation:

1. Spawn a pi agent (`/agent_spawn pi-probe pi`).
2. Before pi has written its first session file, send a Telegram message to the topic.
3. Confirm the message reaches the tmux window (via `upsertHooklessAgentCWD` + `GetAgentByTmuxWindow`).
4. Wait for pi to write its session file.
5. Confirm `PiSource.DiscoverSessions` backfills `agents.session_id` with the real UUID, and subsequent transcript fan-out uses that UUID as the session key.

If step 5 fails, the fix goes in `PiSource.DiscoverSessions` (read the JSONL header, match `cwd`, update `agents.session_id`), not in `agent.Spawn`.

**Acceptance:** Manual QA log in the plan checklist confirms steps 1–5 pass.

---

### PI-07 — Model / provider / thinking defaults & config

**Files:** `internal/config/config.go`, `README.md`, `cmd/maquinista/...`.

Add pass-through env vars so operators don't need to patch Go to switch models:

| Env var | Effect |
|---|---|
| `PI_MODEL` | Default model pattern/ID (e.g. `anthropic/claude-sonnet-4-6`, `openai/gpt-4o:high`). |
| `PI_PROVIDER` | Default provider when `PI_MODEL` is a bare model ID. |
| `PI_THINKING` | `off` \| `minimal` \| `low` \| `medium` \| `high` \| `xhigh`. |
| `PI_CODING_AGENT_DIR` | Overrides pi's config root (rare; for multi-tenant hosts). Passed through unchanged. |

`PiRunner.flags()` (PI-01) already reads these. Document them in `README.md` under **Runners**.

**Acceptance:** `MAQUINISTA_DEFAULT_RUNNER=pi PI_MODEL=openai/gpt-4o:high maquinista` spawns agents that run against `openai/gpt-4o` with high thinking. No code changes beyond env lookup.

---

### PI-08 — Planner support

**File:** `internal/runner/pi.go` (already covered in PI-01), plus sanity check in `internal/bot/planner_commands.go`.

Pi's `--system-prompt <file>` is a drop-in replacement for Claude's flag — no role-framing hack. `PlannerCommand` (PI-01) produces `pi <flags> --system-prompt "$(cat <path>)"`. Also supports `--append-system-prompt` for stacking a planner preamble on top of pi's default; defer to future work unless a real need appears.

**Acceptance:** `/planner pi` spawns an agent that treats `plans/reference/planner-prompt.md` (or whatever `MAQUINISTA_PLANNER_PROMPT` points at) as its system prompt. Asserted by an integration test that greps the tmux window's pane text for the planner's signature opening phrase.

---

### PI-09 — Tests

Package / scope coverage:

- `internal/runner/pi_test.go` — PI-01 cases (above).
- `internal/monitor/source_pi_test.go` — PI-03 cases (above).
- `internal/bot/...` — one handler test that `/runner pi` switches the default runner. Existing `TestRunnerCommand` in `agent_commands_test.go` (if it exists) should parameterize over all registered runners; otherwise add a new one.
- Integration: a `//go:build integration` test that spawns a pi agent in a tmpdir worktree, sends a prompt via `InteractiveCommand`, waits for the session file, confirms one assistant message was captured by `PiSource.ReadNewEntries`. Skipped unless `pi` is on `$PATH`.

**Acceptance:** `go test ./...` green; `go test -tags=integration ./internal/monitor/...` green locally.

---

### PI-10 — Docs

**Files:** `README.md`, `plans/README.md` (index), `plans/reference/architecture-comparison.md` (mention pi alongside OpenCode), `docs/runners.md` if that file exists.

README section:

```markdown
### Pi

[Pi](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent) is a
minimal, multi-provider coding agent CLI by Mario Zechner. Install it:

    npm install -g @mariozechner/pi-coding-agent

Tell maquinista to use it:

    export MAQUINISTA_DEFAULT_RUNNER=pi
    export PI_MODEL=anthropic/claude-sonnet-4-6   # or openai/gpt-4o:high, etc.

Or per-spawn: `/agent_spawn foo pi`.

Pi has **no permission bypass flag** — run maquinista inside your existing
sandbox (container, bwrap, or trusted dev shell).
```

Add a row to `plans/README.md`'s active table linking to this file.

**Acceptance:** Docs land with the rest of the series.

---

## 5. Execution Order

```
PI-00 (observe TUI)
  ├─> PI-02 (PiProfile)
  └─> PI-03 (PiSource)

PI-01 (PiRunner)            ─┐
PI-04 (DB/state wiring)     ─┼─> PI-05 (bot commands)
                              └─> PI-06 (session fallback verify)

PI-07 (config/env)          ─ independent
PI-08 (planner)             ─ independent (lands with PI-01 in practice)
PI-09 (tests)               ─ last, after everything else lands
PI-10 (docs)                ─ last
```

PI-01 through PI-04 can proceed in parallel once PI-00 is done (you need the observations before you can fill PI-02 and PI-03 meaningfully). PI-05 depends on PI-01. PI-06 requires a real pi install plus PI-01 and PI-03 landed. PI-09/PI-10 are the terminal cleanup.

---

## 6. Out of Scope / Future Work

Things that are *interesting* but do **not** block shipping pi-as-a-runner:

- **RPC mode (`pi --mode rpc`)**: a future `PiRunnerRPC` could speak pi's JSONL RPC protocol over stdin/stdout and eliminate tmux-pane scraping. High-value but a full rewrite of the runner<->monitor contract — out of scope here.
- **JSON event stream (`pi --mode json`)**: similarly useful for transcript fan-out; would retire `PiSource`'s file-tail approach in favor of an event subscription.
- **SDK embedding (openclaw's approach)**: requires a Node sidecar to host `createAgentSession`; violates maquinista's "agents are tmux processes" invariant. Only pursue if we ever add a non-tmux frontend.
- **Pi extensions / skills / packages**: pi's extension system (TypeScript plugins installed via `pi install npm:…` or `pi install git:…`) is powerful but orthogonal to maquinista's runner integration. A future plan can define a `maquinista-pi-package` that ships maquinista-aware tools (mailbox, `ask_agent`, shared_context) directly to pi, paralleling what Claude's hooks do today.
- **Tree-aware transcript outbox**: emitting a coherent leaf-path view when a pi session has branches. Currently we linearize by file order.
- **Multi-profile auth rotation** (openclaw does this): pi has per-provider credential storage already; multi-profile rotation is arguably the responsibility of whatever sandboxes maquinista, not of maquinista itself.

---

## 7. Checklist

- [ ] PI-00: Observe pi's TUI output across idle / busy / compaction / `/login` / `/tree`; capture findings as code comments.
- [ ] PI-01: `PiRunner` + `pi_test.go`, registered via `init()`.
- [ ] PI-02: `PiProfile()` added to `internal/monitor/terminal.go`.
- [ ] PI-03: `PiSource` in `internal/monitor/source_pi.go`, registered alongside the other sources, JSONL header + message parsing, CWD slug matched, unknown-role-tolerant.
- [ ] PI-04: DB/state wiring confirmed runner-name-agnostic; no new hardcoded strings.
- [ ] PI-05: `/runner`, `/runner pi`, `/agent_spawn … pi` work; "Available: …" strings derived from `runner.Runners()`.
- [ ] PI-06: Session-tracking fallback verified end-to-end for a real pi agent (manual QA log attached).
- [ ] PI-07: `PI_MODEL` / `PI_PROVIDER` / `PI_THINKING` / `PI_CODING_AGENT_DIR` env vars documented and honored.
- [ ] PI-08: `PlannerCommand` uses `--system-prompt`; integration test confirms the planner persona survives.
- [ ] PI-09: Unit tests + opt-in integration test green.
- [ ] PI-10: README runner section + `plans/README.md` index entry + `architecture-comparison.md` mention.

---

## Appendix A — Pi Session File Reference

Sessions live at:

```
$PI_CODING_AGENT_DIR/sessions/<cwd-slug>/<uuid>.jsonl
  (default PI_CODING_AGENT_DIR = ~/.pi/agent)
```

CWD slug: the working directory with `/` replaced by `-` (verify against a real pi-created directory before relying on the exact algorithm — pin it in `TestPiSource_SlugifyCWD`).

File layout (v3):

```jsonl
{"type":"session","version":3,"id":"<uuid>","timestamp":"…","cwd":"/abs/path"}
{"type":"message","id":"<8-hex>","parentId":null,"timestamp":"…","message":{"role":"user","content":"…","timestamp":…}}
{"type":"message","id":"<8-hex>","parentId":"<prev-id>","timestamp":"…","message":{"role":"assistant","content":[{"type":"text","text":"…"}],"provider":"anthropic","model":"claude-sonnet-4-6","usage":{…},"stopReason":"stop","timestamp":…}}
{"type":"message","id":"<8-hex>","parentId":"<prev-id>","timestamp":"…","message":{"role":"toolResult","toolCallId":"call_…","toolName":"bash","content":[…],"isError":false,"timestamp":…}}
{"type":"compaction","id":"…","parentId":"…","timestamp":"…","summary":"…","firstKeptEntryId":"…","tokensBefore":50000}
{"type":"branch_summary","id":"…","parentId":"…","timestamp":"…","fromId":"…","summary":"…"}
{"type":"model_change","id":"…","parentId":"…","timestamp":"…","provider":"openai","modelId":"gpt-4o"}
{"type":"thinking_level_change","id":"…","parentId":"…","timestamp":"…","thinkingLevel":"high"}
{"type":"label","id":"…","parentId":"…","timestamp":"…","targetId":"<some-id>","label":"checkpoint-1"}
{"type":"session_info","id":"…","parentId":"…","timestamp":"…","name":"Refactor auth module"}
{"type":"custom","id":"…","parentId":"…","timestamp":"…","customType":"…","data":{…}}
{"type":"custom_message","id":"…","parentId":"…","timestamp":"…","customType":"…","content":"…","display":true}
```

Entry shape notes for `PiSource`:

- Every entry except `session` carries `id`, `parentId`, `timestamp`.
- Tree structure is via `parentId` — we ignore branching and tail linearly; see §3.4.
- `message.role` values we care about: `user`, `assistant`, `toolResult`, `bashExecution`. Others pass through as a generic `OtherEntry`.
- `SessionHeader` carries `cwd` — that's our anchor for binding a file to an agent.

## Appendix B — Pi CLI Flag Cheat Sheet

Flags the runner uses, pinned from [pi README](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent):

| Flag | Effect |
|---|---|
| `-p`, `--print` | Non-interactive: print reply and exit. Reads piped stdin. |
| `--mode json` | JSON-lines event stream (future: replace file tail). |
| `--mode rpc` | JSONL stdio RPC (future: replace tmux). |
| `--provider <name>` | anthropic / openai / google / bedrock / vertex / groq / cerebras / xai / openrouter / vercel-ai-gateway / ollama / huggingface / kimi / minimax / zai / opencode / mistral. |
| `--model <pattern>` | `provider/id` or pattern with optional `:thinking`. |
| `--thinking <level>` | off \| minimal \| low \| medium \| high \| xhigh. |
| `--system-prompt <file>` | Replace the default system prompt (planner). |
| `--append-system-prompt <file>` | Append to the default system prompt. |
| `-c`, `--continue` | Continue most recent session. |
| `-r`, `--resume` | Browse and pick a session. |
| `--session <uuid|file>` | Use a specific session. |
| `--no-session` | Ephemeral. |
| `--session-dir <path>` | Custom session root. |
| `--tools read,bash,edit,write` | Restrict built-in tools. |
| `--no-tools` | Disable all built-in tools. |
| `--no-context-files`, `-nc` | Ignore AGENTS.md / CLAUDE.md walk. |
| `-e <path|npm|git>` | Load extension (repeatable). |
| `--skill <path|npm|git>` | Load skill (repeatable). |

## Appendix C — References

- Pi README: https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent
- Pi session file format: `packages/coding-agent/docs/session.md`
- Pi RPC protocol: `packages/coding-agent/docs/rpc.md`
- openclaw's Pi integration docs (embedded SDK approach): `../openclaw/docs/pi.md`
- openclaw's embedded runner source: `../openclaw/src/agents/pi-embedded-runner/`
- Existing maquinista runner pattern: `internal/runner/claude.go`, `internal/runner/opencode.go`, `internal/runner/openclaude.go`
- OpenCode integration plan (the analog we're mirroring): `plans/active/opencode-integration.md`
