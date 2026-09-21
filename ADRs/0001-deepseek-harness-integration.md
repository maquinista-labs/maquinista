# ADR-0001: DeepSeek Harness Integration

- **Status:** Proposto (pending Otavio's ok)
- **Date:** 2026-09-21
- **Deciders:** Otavio
- **Scope:** maquinista runner subsystem (`internal/runner`, `internal/monitor`, bot/CLI surfaces)

## Context

Maquinista integrates agent harnesses as pluggable runners behind the `AgentRunner`
interface (`internal/runner/runner.go:25-58`): `LaunchCommand`, `InteractiveCommand`,
`PlannerCommand`, `DetectInstallation`, `EnvOverrides`, `HasSessionHook`,
`MonitorProfile`. A registry (`runner.Register`) plus a per-runner
`TranscriptSource` (`monitor.RegisterSource`, wired in `cmd/maquinista/cmd_start.go:219-225`)
is the full anatomy. Current runners:

| Runner | Runner file | Source file | Notes |
|---|---|---|---|
| `claude` | claude.go (47 ln) | source_claude.go (316 ln) | reference implementation, SessionStart hook |
| `openclaude` | openclaude.go (49 ln) | source_openclaude.go (272 ln) | Claude-compatible fork: **reuses `ClaudeProfile()`** and the Claude JSONL transcript shape |
| `opencode` | opencode.go (92 ln) | source_opencode.go (477 ln) | needed its own profile + SQLite-reading source |
| `custom` | custom.go (79 ln) | — | template-based escape hatch, empty monitor profile |

`plans/active/pi-integration.md` (PI-00…PI-08) is the newest worked example of the
checklist; `plans/active/opencode-integration.md` (now archived) was the first.

### What "the DeepSeek harness" resolves to

DeepSeek's **stable, documented** terminal path is **Claude Code pointed at their
Anthropic-compatible API** (verified 2026-09-21, `api-docs.deepseek.com/guides/anthropic_api` and
`/quick_start/agent_integrations/claude_code`):

- `ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic`
- `ANTHROPIC_AUTH_TOKEN=<DeepSeek key>`, `ANTHROPIC_MODEL=deepseek-flash[1m]`
- per-tier overrides: `ANTHROPIC_DEFAULT_OPUS/SONNET/HAIKU_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL`
- model-name auto-mapping: `claude-opus*` → `deepseek-v4-pro` (billed as V4 Pro),
  `claude-sonnet/haiku*` → `deepseek-flash`; unknown names fall back to `deepseek-flash`

So the DeepSeek harness **is the Claude harness with different credentials**: same
binary, same TUI, same `~/.claude/projects/...` JSONL transcripts, same client-side
SessionStart hook. That makes this the `openclaude` case, not the `opencode`/`pi` case.

**Correction (added later the same day):** DeepSeek *does* now ship a first-party
harness — [`dsh`](https://github.com/deepseek-ai/deepseek-harness), MIT, **developer
preview** with an explicit "THERE WILL BE COMPATIBILITY-BREAKING CHANGES" warning.
It changes the option space (see Option E) but not the near-term decision: the
Anthropic-API recipe above remains their stable integration path.

## Options Considered

| | A. Thin `DeepSeekRunner` (claude-compatible wrapper) | B. Env-only (no code) | C. Full first-class pipeline (own source+profile) | D. DeepSeek models via other harnesses (pi/opencode) |
|---|---|---|---|---|
| Effort | ~1 dev day | ~0 (docs only) | 3-5 dev days | 0 (works today: `--model deepseek/...`) |
| First-class `/runner deepseek` | ✓ | ✗ (masquerades as `claude`) | ✓ | n/a (different harness name) |
| claude + deepseek agents **side by side** | ✓ (per-spawn `EnvOverrides`) | ✗ (process-wide env: all claude agents go DeepSeek) | ✓ | ✓ |
| New code | ~150-250 ln incl. tests | 0 | ~500+ ln | 0 |
| Transcript/monitor work | none — reuse `ClaudeProfile()` + Claude-source path (openclaude precedent) | n/a | redundant: transcripts are byte-identical Claude JSONL | already exists per harness |
| Risk | session-hook behavior under BASE_URL override unverified → gate on DS-00 probe; hookless fallback (OC-03) exists if it fails | config drift, confusing transcripts, no model defaults | over-engineering (pitfall: don't build what the compatibility layer already gives) | doesn't answer the ask; probe cost of a new TUI for a model swap |

## Decision

**Option A — register a `deepseek` runner that drives the stock `claude` binary with
DeepSeek env overrides**, mirroring `openclaude.go` (49 lines, `MonitorProfile() =
monitor.ClaudeProfile()`).

Not B: env-only works only for "everyone is DeepSeek" and makes `/runner` lie.
Not C: there is no new transcript format, TUI, or session layout to integrate —
the Claude pipeline already handles every byte. Not D: already possible, and it's a
different harness wearing DeepSeek as a model.

**Deferred — Option E (first-party `dsh`)**: revisit when (a) `dsh` exits developer
preview / stabilizes its plugin + session APIs, or (b) maquinista wants a
non-interactive, job-style runner interface — `dsh` would be the natural first
citizen for it, not a special case.

### Option E — first-party `dsh` (added 2026-09-21, deferred)

DeepSeek Harness (`dsh`) is a Node/Cordis agent harness, "everything is a plugin":
no privileged core — model adapter, tool registry, **session log**, and the agent
loop itself are all plugins replaceable from config patches. Extensibility is real
but **in-process**: third parties ship out-of-tree TS plugins (`apply(ctx)` modules)
installed via `dsh plugin` (pnpm) into a profile, plus user `cordis.patch.yml` /
`--patch` overlay layers that can replace any config row; there is no out-of-process
extension API at the plugin layer (automation clients instead get ACP stdio,
SDK JSON-RPC stdio, Python SDK, webhooks). Shipped profiles: `web`, `headless`
(one-shot job → persisted session → prints answer → exits), `sdk`, `sdk-minimal`,
`acp`; `desktop` is Electron-reserved and **no interactive TUI profile ships**
(the `tui` profile in their docs is hypothetical).

Why not now:

- **Different runner shape**: `headless` is one-shot, `acp`/`sdk` are long-lived
  stdio servers — maquinista's `AgentRunner` is built around interactive tmux panes
  (`LaunchCommand`/`InteractiveCommand` + `MonitorProfile` pane scraping). Either
  we squeeze `dsh` into the pane model (fighting its design) or we extend the
  runner interface for job-style runners (real design work, beyond this ADR's scope).
- **No transcript reuse**: sessions live in dsh's append-only `SessionEvent` log
  (Cordis `ctx.sessions`), not `~/.claude` JSONL — a new `TranscriptSource` from
  scratch (the PI-03-class task Option A avoids), against a format with no
  stability guarantee in dev preview.
- **Churn risk**: self-declared breaking changes ahead; MIT and open, but the
  integration surface we'd code against (profiles, patches, session log) is
  exactly what preview iterations rewrite.

Rough shape if/when revisited: `dsh --profile headless` per job or `--profile acp`
long-lived; new source reading the session store; est. **3-5 dev days** once the
interface question is settled — closer to the opencode case than the openclaude one.

## Effort Estimate

| Task | Files | Size | Est |
|---|---|---|---|
| DS-00 Probe: `claude` + DeepSeek env in a tmux pane; confirm SessionStart hook fires, JSONL lands in `~/.claude/projects`, model mapping works | — (manual QA) | — | 30-45 min |
| DS-01 `DeepSeekRunner`: commands = `claude` + env block; `DetectInstallation` = `claude` on PATH **and** `DEEPSEEK_API_KEY` set; `HasSessionHook()=true`; `MonitorProfile()=ClaudeProfile()`; model default `deepseek-flash[1m]` with `DEEPSEEK_MODEL` env override | `internal/runner/deepseek.go` + test | ~60 + ~80 ln | 2-3 h |
| DS-02 `DeepSeekSource`: openclaude pattern — `loadRunnerSessionMap(ctx, pool, "deepseek")`, same `~/.claude/projects` discovery | `internal/monitor/source_deepseek.go` + test | ~40-150 ln (delegate or clone) | 2-4 h |
| DS-03 Wiring: `RegisterSource("deepseek", …)` (cmd_start.go); derive unknown-runner help from `runner.Runners()` instead of the 7 hardcoded flag strings + 2 bot error strings (`agent_commands.go:79,243`) | cmd_start.go, cmd_*.go, bot | ~15 ln | 30 min |
| DS-04 Docs: README Runners section, `DEEPSEEK_*` env table, `arch/runners.md` row | README, arch/runners.md | docs | 30 min |
| DS-05 E2E QA: `/runner deepseek`, `/agent_spawn x deepseek`, transcript fan-out to Telegram, planner pane | — (manual) | — | ~1 h |

**Total: ≈ 1 dev day (6-9 h)** — the cheap end of the runner spectrum, because the
compatibility layer removes the two expensive tasks every other integration had
(TUI probing PI-00 / transcript parser PI-03). Pi and opencode were multi-day
because each brought a new TUI and a new session store; DeepSeek brings neither.

**Load-bearing assumption to falsify in DS-00** (hypothesis, not fact): Claude Code's
SessionStart hook is client-side and fires identically under `ANTHROPIC_BASE_URL`
override. If it doesn't, the OC-03 hookless fallback (preliminary session_map row +
source-side discovery) already landed and covers it — no redesign.

## Consequences

- **Positive:** DeepSeek models become a first-class runner choice per agent; cheap
  model tier available for coder/planner roles without losing the Claude UX; the
  hardcoded-runner-list cleanup benefits every future runner (pi included).
- **Negative:** one more Claude-shaped source file to keep in sync (mitigate: prefer
  delegation over cloning if `ClaudeSource` allows a runner-name parameter).
- **Neutral:** billing note — `claude-opus*` prompts silently become V4-Pro priced;
  operators choosing models by Claude name must know the mapping (documented in DS-04).
- Pricing deliberately out of scope here (verify at platform.deepseek.com when needed).

## References

- `internal/runner/runner.go:25-58` — AgentRunner interface (this session, a3c5cc1)
- `internal/runner/openclaude.go` — the precedent this ADR copies
- `plans/active/pi-integration.md` — task checklist template (PI-00…PI-08)
- DeepSeek Anthropic-API guide — https://api-docs.deepseek.com/guides/anthropic_api (fetched 2026-09-21 via r.jina.ai)
- DeepSeek × Claude Code recipe — https://api-docs.deepseek.com/quick_start/agent_integrations/claude_code (fetched 2026-09-21)
- OC-03 hookless session fallback — see opencode integration plan, `HasSessionHook()=false` path
- deepseek-ai/deepseek-harness — https://github.com/deepseek-ai/deepseek-harness (Option E source; README + docs/architecture.md + apps/cli/README.md, fetched 2026-09-21)
- dsh plugin dev guide — https://deepseek-harness.github.io/deepseek-harness/en/develop/basic/ (fetched 2026-09-21 via r.jina.ai)
- Cordis ("everything is a plugin" framework) — https://github.com/cordiverse/cordis; paper https://arxiv.org/abs/2608.25512
