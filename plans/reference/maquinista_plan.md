# Maquinista: Unified Agent Orchestration Platform

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

Two existing Go projects need to be ported into a new **maquinista** repository (clean start, not merging into either):

- **Tramuntana** (`github.com/otaviocarvalho/tramuntana`): Telegram UI layer. 1 topic = 1 tmux window = 1 Claude Code process. Packages: bot/, config/, tmux/, state/, queue/, monitor/, render/, minuano/ (bridge), git/, listener/. Commands: serve, hook, version.
- **Minuano** (`github.com/otavio/minuano`): Pull-based task coordination via PostgreSQL. Packages: db/, tmux/, agent/, git/, tui/. Commands: up, down, migrate, add, edit, show, status, tree, search, run, spawn, agents, attach, logs, kill, reclaim, prompt, merge, schedule, cron, planner, approve, unclaim, draft-release. Scripts: minuano-claim, minuano-done, minuano-observe, minuano-handoff, minuano-pick.

The goal: a single `maquinista` binary that combines both projects, adds spec-driven task creation, a pluggable agent runner (claude/opencode/custom), an automatic orchestrator, and decouples Telegram topics from agent sessions.

---

## Repository Structure

```
maquinista/
  cmd/maquinista/
    main.go                      # Root cobra command + global flags (--db, --session, --config)
    cmd_serve.go                 # maquinista serve (Telegram bot + optional orchestrator)
    cmd_hook.go                  # maquinista hook [--install]
    cmd_version.go               # maquinista version
    cmd_up.go                    # maquinista up (docker postgres start)
    cmd_down.go                  # maquinista down (docker postgres stop)
    cmd_migrate.go               # maquinista migrate
    cmd_add.go                   # maquinista add
    cmd_edit.go                  # maquinista edit
    cmd_show.go                  # maquinista show (--json)
    cmd_status.go                # maquinista status (--json, --project)
    cmd_tree.go                  # maquinista tree (--project)
    cmd_search.go                # maquinista search
    cmd_run.go                   # maquinista run (--agents, --worktrees, --runner)
    cmd_spawn.go                 # maquinista spawn (--worktrees, --runner)
    cmd_agents.go                # maquinista agents (--watch)
    cmd_attach.go                # maquinista attach
    cmd_logs.go                  # maquinista logs
    cmd_kill.go                  # maquinista kill (--all)
    cmd_reclaim.go               # maquinista reclaim (--minutes)
    cmd_prompt.go                # maquinista prompt {single, auto, batch}
    cmd_merge.go                 # maquinista merge (--watch)
    cmd_schedule.go              # maquinista schedule {add, list, run, enable, disable}
    cmd_cron.go                  # maquinista cron tick
    cmd_planner.go               # maquinista planner {start, stop, reopen, status}
    cmd_approve.go               # maquinista approve / reject / draft-release
    cmd_unclaim.go               # maquinista unclaim
    cmd_spec.go                  # NEW: maquinista spec {sync, validate}
    cmd_orchestrate.go           # NEW: maquinista orchestrate

  internal/
    config/config.go             # Merged config (tramuntana env + minuano DB settings)
    db/
      db.go                      # Connection pool + migration runner (from minuano)
      queries.go                 # All SQL queries (from minuano, 1250 lines)
      migrations/001-005.sql     # Existing minuano migrations
      migrations/006_maquinista_agent_runner.sql  # NEW: runner_type, runner_config on agents
      migrations/007_topic_observations.sql  # NEW: topic_agent_bindings table
    tmux/tmux.go                 # Merged: tramuntana convention (dir param) + minuano extras
    git/git.go                   # Merged: tramuntana convention (dir param everywhere)
    state/                       # From tramuntana (state.json, session_map, monitor_state)
    bot/                         # From tramuntana (all 20+ handler files)
    bridge/bridge.go             # From tramuntana/internal/minuano/ (CLI bridge)
    monitor/                     # From tramuntana (JSONL poll loop, transcript parser)
    queue/                       # From tramuntana (per-user message queue, flood control)
    render/                      # From tramuntana (format, markdown, screenshot, fonts/)
    listener/                    # From tramuntana (PostgreSQL NOTIFY listener, router)
    agent/agent.go               # From minuano (Spawn, Kill, Heartbeat), refactored for AgentRunner
    tui/tui.go                   # From minuano (bubbletea watch view)
    runner/                      # NEW: Agent runner abstraction
      runner.go                  # AgentRunner interface + registry
      claude.go                  # Claude Code implementation
      opencode.go                # OpenCode implementation
      custom.go                  # Generic CLI implementation
    spec/                        # NEW: Spec file parser
      parser.go                  # Markdown+YAML frontmatter parser
      sync.go                    # Reconcile specs ↔ DB tasks
    orchestrator/                # NEW: Poll-dispatch-reconcile loop
      orchestrator.go
    prompt/prompt.go             # NEW: Shared prompt builders (extracted from cmd_prompt.go)

  hook/hook.go                   # From tramuntana (SessionStart hook)
  scripts/maquinista-*                # Renamed from minuano-* (claim, done, observe, handoff, pick)
  claude/CLAUDE.md               # Agent loop instructions
  claude/planner-system-prompt.md
  docker/docker-compose.yml      # PostgreSQL + optional pgAdmin
  .specs/example.md              # Example spec file
```

---

## Key Design Decisions

### Merged tmux package
- **Base**: tramuntana's conventions — `SendKeys` sends literal text (no implicit Enter), `CapturePane(session, windowID, withAnsi)`, `NewWindow(session, name, dir, claudeCmd, env)` returns `(windowID, error)`
- **Added from minuano**: `WindowExists`, `NewWindowWithDir` (simpler variant, no claudeCmd), `AttachSession`, `SwitchWindow`, `InsideTmux`, `AttachOrSwitch`

### Merged git package
- **Base**: tramuntana's convention — ALL functions take `dir string` parameter
- **Added from minuano**: `HasUncommittedChanges(dir)`, `HasUnmergedChanges(dir, branch, base)`, `AddAndCommit(dir, message)`

### Agent runner abstraction
Both CLI tools must be interchangeable:

| Feature | Claude Code | OpenCode |
|---------|------------|----------|
| Non-interactive | `claude -p "prompt"` | `opencode run "prompt"` |
| Skip permissions | `--dangerously-skip-permissions` | `OPENCODE_PERMISSION` env var / config |
| Output format | `--output-format json/stream-json` | `--format json/default` |
| Model selection | `--model sonnet` | `-m anthropic/claude-3-5-sonnet` |
| Budget limit | `--max-budget-usd 5.00` | N/A |
| Tool filtering | `--allowed-tools "Bash,Edit,Read"` | Agent config in `.opencode/agents/` |
| Nested detection | Must unset `CLAUDECODE=1` | N/A |
| Session persistence | `--no-session-persistence` | N/A |

---

## Phase 1: Port and Merge Codebases (15 tasks)

Goal: A single `maquinista` binary that runs all tramuntana commands (serve, hook, version) and all minuano commands (up, down, migrate, add, status, tree, run, spawn, agents, ...) with zero behavioral regressions.

### P1-01: Initialize maquinista Go module and directory skeleton
- **Priority:** 10 | **Depends on:** —
- Create `github.com/maquinista-labs/maquinista` module, Go 1.24. Directory structure, Makefile (build/test/clean), minimal main.go with empty cobra root. go.mod with all deps: telegram-bot-api/v5, pgx/v5, cobra, goldmark, x/image, bubbletea, lipgloss, cron/v3, godotenv.
- **Test:** `go build ./cmd/maquinista && ./maquinista version`

### P1-02: Port merged config package
- **Priority:** 9 | **Depends on:** P1-01
- Port `tramuntana/internal/config/config.go`. Extend Config with `DatabaseURL`, `MaquinistaDir` (default `~/.maquinista`), tmux session default "maquinista". Keep all tramuntana fields.
- **Source:** `/home/otavio/code/tramuntana/internal/config/config.go`
- **Test:** `go test ./internal/config/...`

### P1-03: Port merged tmux package
- **Priority:** 9 | **Depends on:** P1-01
- Port tramuntana tmux as base. Add minuano's `WindowExists`, `NewWindowWithDir`, `AttachSession`, `SwitchWindow`, `InsideTmux`, `AttachOrSwitch`. Adapt minuano's functions to use tramuntana's SendKeys convention.
- **Sources:** `/home/otavio/code/tramuntana/internal/tmux/tmux.go`, `/home/otavio/code/minuano/internal/tmux/tmux.go`
- **Test:** `go test ./internal/tmux/...`

### P1-04: Port merged git package
- **Priority:** 9 | **Depends on:** P1-01
- Port tramuntana git as base (all functions take `dir`). Add minuano's `HasUncommittedChanges(dir)`, `HasUnmergedChanges(dir, branch, base)`, `AddAndCommit(dir, message)`.
- **Sources:** `/home/otavio/code/tramuntana/internal/git/git.go`, `/home/otavio/code/minuano/internal/git/git.go`
- **Test:** `go test ./internal/git/...`

### P1-05: Port state package from tramuntana
- **Priority:** 8 | **Depends on:** P1-01
- Copy `tramuntana/internal/state/` wholesale. Update import paths only.
- **Source:** `/home/otavio/code/tramuntana/internal/state/`
- **Test:** `go test ./internal/state/...`

### P1-06: Port database package from minuano
- **Priority:** 8 | **Depends on:** P1-01
- Copy `minuano/internal/db/` (db.go, queries.go, migrations/001-005.sql). Update import paths only.
- **Source:** `/home/otavio/code/minuano/internal/db/`
- **Test:** `go test ./internal/db/...`

### P1-07: Port agent package from minuano
- **Priority:** 7 | **Depends on:** P1-03, P1-04, P1-06
- Port `minuano/internal/agent/agent.go`. Update imports to maquinista. Adapt `Spawn` to use `tmux.NewWindowWithDir`. Keep hardcoded claude bootstrap for now (Phase 3 abstracts it).
- **Source:** `/home/otavio/code/minuano/internal/agent/agent.go`
- **Test:** `go test ./internal/agent/...`

### P1-08: Port render, queue, monitor, listener packages from tramuntana
- **Priority:** 7 | **Depends on:** P1-02, P1-05
- Copy wholesale: `internal/render/` (with fonts/), `internal/queue/`, `internal/monitor/`, `internal/listener/`. Update import paths.
- **Sources:** 4 directories from tramuntana
- **Test:** `go test ./internal/render/... ./internal/queue/... ./internal/monitor/... ./internal/listener/...`

### P1-09: Port bot package and bridge from tramuntana
- **Priority:** 6 | **Depends on:** P1-08, P1-03
- Copy `tramuntana/internal/bot/` (20+ files). Port `tramuntana/internal/minuano/bridge.go` to `internal/bridge/bridge.go`. Update all imports.
- **Sources:** `/home/otavio/code/tramuntana/internal/bot/`, `/home/otavio/code/tramuntana/internal/minuano/bridge.go`
- **Test:** `go test ./internal/bot/... ./internal/bridge/...`

### P1-10: Port TUI package from minuano
- **Priority:** 6 | **Depends on:** P1-06
- Copy `minuano/internal/tui/`. Update imports.
- **Source:** `/home/otavio/code/minuano/internal/tui/`
- **Test:** `go test ./internal/tui/...`

### P1-11: Port hook package from tramuntana
- **Priority:** 6 | **Depends on:** P1-03, P1-05
- Copy `tramuntana/hook/hook.go`. Update hook command path to "maquinista hook", state dir to `~/.maquinista`.
- **Source:** `/home/otavio/code/tramuntana/hook/hook.go`
- **Test:** `go build ./hook/...`

### P1-12: Port and rename scripts from minuano
- **Priority:** 6 | **Depends on:** P1-01
- Copy 5 scripts, rename `minuano-*` → `maquinista-*`. Update echo messages.
- **Source:** `/home/otavio/code/minuano/scripts/`
- **Test:** `test -x scripts/maquinista-claim && test -x scripts/maquinista-done`

### P1-13: Port docker and claude directories
- **Priority:** 5 | **Depends on:** P1-01
- Copy docker-compose.yml, claude/CLAUDE.md, claude/planner-system-prompt.md. Update "minuano" → "maquinista" in text.
- **Sources:** `/home/otavio/code/minuano/docker/`, `/home/otavio/code/minuano/claude/`
- **Test:** `test -f docker/docker-compose.yml && test -f claude/CLAUDE.md`

### P1-14: Wire up unified CLI with all subcommands
- **Priority:** 10 | **Depends on:** P1-02 through P1-13
- Create main.go with cobra root command. Port all minuano cmd files (up, down, migrate, add, edit, show, status, tree, search, run, spawn, agents, attach, logs, kill, reclaim, prompt, merge, schedule, cron, planner, approve, reject, draft-release, unclaim). Port tramuntana commands (serve, hook, version). Global flags: `--db`, `--session`, `--config`. Update all prompt builders to reference `maquinista-*` scripts.
- **Sources:** `/home/otavio/code/minuano/cmd/minuano/`, `/home/otavio/code/tramuntana/cmd/tramuntana/`
- **Test:** `go build ./cmd/maquinista && go test ./cmd/maquinista/...`

### P1-15: End-to-end build verification
- **Priority:** 10 | **Depends on:** P1-14
- Verify: `maquinista version`, `maquinista serve --help`, `maquinista status --help`, `maquinista run --help`. Run `go test ./...` and `go vet ./...`.
- **Test:** `go build ./cmd/maquinista && go test ./... && go vet ./...`

---

## Phase 2: Spec File Parser and Sync Command (4 tasks)

Goal: `.specs/*.md` files define tasks declaratively. `maquinista spec sync` reconciles them with the database.

### Spec file format

```markdown
---
id: setup-db-layer
title: Set up database connection layer
priority: 8
depends_on:
  - init-project
test_cmd: go test ./internal/db/...
requires_approval: false
---

Detailed specification body here. Supports full Markdown.
```

### P2-01: Implement spec file parser
- **Priority:** 8 | **Depends on:** P1-15
- Create `internal/spec/parser.go`. `SpecFile` struct: ID, Title, Priority, DependsOn, TestCmd, RequiresApproval, Body, FilePath. `ParseFile(path) (*SpecFile, error)` extracts YAML frontmatter + markdown body. `ParseDir(dir) ([]*SpecFile, error)` globs `*.md`. Hand-rolled YAML parser (avoid new dep) or use `gopkg.in/yaml.v3`.
- **Test:** `go test ./internal/spec/...`

### P2-02: Implement spec sync logic
- **Priority:** 8 | **Depends on:** P2-01, P1-06
- Create `internal/spec/sync.go`. `Sync(pool, specs, projectID, dryRun) (*SyncResult, error)`. Algorithm: load existing project tasks from DB, diff against specs, create/update as needed. New tasks start as "draft". Orphaned DB tasks get warnings (no auto-delete). Dependencies resolved within the spec set.
- **Test:** `go test ./internal/spec/...`

### P2-03: Create `maquinista spec sync` CLI command
- **Priority:** 7 | **Depends on:** P2-02
- `cmd/maquinista/cmd_spec.go`. Subcommands: `maquinista spec sync --dir .specs/ --project X [--dry-run] [--release]`, `maquinista spec validate --dir .specs/`. Release flag calls `db.DraftReleaseAll` after sync.
- **Test:** `go build ./cmd/maquinista && maquinista spec validate --help`

### P2-04: Example specs and documentation
- **Priority:** 5 | **Depends on:** P2-03
- Create `.specs/example.md` demonstrating all frontmatter fields.
- **Test:** `maquinista spec validate --dir .specs/`

---

## Phase 3: Agent Runner Abstraction (6 tasks)

Goal: A pluggable `AgentRunner` interface that supports Claude Code, OpenCode, and custom CLI tools interchangeably.

### AgentRunner interface

```go
// internal/runner/runner.go
type Config struct {
    Binary              string            // "claude", "opencode", custom path
    Model               string            // "opus", "anthropic/claude-3-5-sonnet"
    MaxBudgetUSD        float64           // 0 = no limit (Claude only)
    AllowedTools        []string          // Runner-specific tool filtering
    DisallowedTools     []string
    SkipPermissions     bool              // --dangerously-skip-permissions / OPENCODE_PERMISSION
    NoSessionPersistence bool             // Claude: --no-session-persistence
    Env                 map[string]string // Extra env vars
    Dir                 string            // Working directory
    OutputFormat        string            // "json", "stream-json", "text"
}

type AgentRunner interface {
    Name() string
    InteractiveCommand(prompt string, cfg Config) string
    NonInteractiveArgs(prompt string, cfg Config) []string
    RunNonInteractive(ctx context.Context, prompt string, cfg Config) (*Result, error)
    DetectInstallation() error
    EnvOverrides() map[string]string  // e.g., Claude: unset CLAUDECODE
}
```

### P3-01: Define AgentRunner interface and registry
- **Priority:** 8 | **Depends on:** P1-15
- Create `internal/runner/runner.go` with interface, Config, Result types. Registry: `var Runners map[string]AgentRunner`, `Register(name, runner)`, `Get(name) (AgentRunner, error)`.
- **Test:** `go test ./internal/runner/...`

### P3-02: Implement Claude Code runner
- **Priority:** 8 | **Depends on:** P3-01
- `internal/runner/claude.go`. `InteractiveCommand`: `claude --dangerously-skip-permissions -p "prompt"`. `NonInteractiveArgs`: `[claude, -p, prompt, --output-format, json, --dangerously-skip-permissions, --model, X, --max-budget-usd, Y, --no-session-persistence]`. `EnvOverrides`: `{"CLAUDECODE": ""}`. Register as "claude".
- **Test:** `go test ./internal/runner/...`

### P3-03: Implement OpenCode runner
- **Priority:** 7 | **Depends on:** P3-01
- `internal/runner/opencode.go`. `InteractiveCommand`: `opencode run "prompt"`. `NonInteractiveArgs`: `[opencode, run, prompt, --format, json, -m, provider/model]`. `EnvOverrides`: sets `OPENCODE_PERMISSION` if SkipPermissions. Register as "opencode".
- **Test:** `go test ./internal/runner/...`

### P3-04: Implement Custom/Generic runner
- **Priority:** 6 | **Depends on:** P3-01
- `internal/runner/custom.go`. Wraps arbitrary binary with Go template for command line construction. Register as "custom".
- **Test:** `go test ./internal/runner/...`

### P3-05: Refactor agent package to use AgentRunner
- **Priority:** 8 | **Depends on:** P3-02, P1-07
- Change `agent.Spawn` and `SpawnWithWorktree` to accept `AgentRunner`. Replace hardcoded `claude --dangerously-skip-permissions` in `sendBootstrap` with `runner.InteractiveCommand(prompt, cfg)`. Merge runner's `EnvOverrides()` into tmux window env. Add `--runner` flag to `run` and `spawn` commands (default "claude").
- **Test:** `go test ./internal/agent/... && go test ./cmd/maquinista/...`

### P3-06: Add DB migration for runner metadata
- **Priority:** 7 | **Depends on:** P1-06
- Migration 006: `ALTER TABLE agents ADD COLUMN runner_type TEXT NOT NULL DEFAULT 'claude'`, `ADD COLUMN runner_config JSONB`. Update `RegisterAgent`, `Agent` struct, scan functions, `agents` display.
- **Test:** `go test ./internal/db/...`

---

## Phase 4: Orchestrator Loop (5 tasks)

Goal: `maquinista orchestrate` automatically polls ready tasks, dispatches agents, reconciles dead agents, and processes the merge queue.

### Orchestrator design

```
tick() every N seconds (or wake on NOTIFY):
  1. RECONCILE: check tmux windows, reclaim dead agents' tasks
  2. POLL: count ready tasks, count active agents
  3. DISPATCH: if activeAgents < maxAgents and tasks ready → agent.Spawn
  4. MERGE: process one merge queue entry (if worktrees enabled)
  5. LOG: print status summary
```

### P4-01: Implement core orchestrator loop
- **Priority:** 8 | **Depends on:** P3-05, P1-06, P1-07
- Create `internal/orchestrator/orchestrator.go`. `OrchestratorConfig`: pool, runner, tmuxSession, projectID, maxAgents, pollInterval, useWorktrees, claudeMDPath. `Run(ctx)` implements the poll-dispatch-reconcile loop. Reconcile uses `tmux.WindowExists` to detect dead agents.
- **Test:** `go test ./internal/orchestrator/...`

### P4-02: Add NOTIFY-driven wake-up
- **Priority:** 7 | **Depends on:** P4-01, P1-08
- Use `internal/listener` to consume `task_events`. When task → "ready" or "done", wake orchestrator immediately via channel-based select: `select { case <-ticker.C: ...; case <-notifyCh: ... }`.
- **Test:** `go test ./internal/orchestrator/...`

### P4-03: Create `maquinista orchestrate` command
- **Priority:** 8 | **Depends on:** P4-02
- `cmd/maquinista/cmd_orchestrate.go`. Flags: `--project` (required), `--max-agents` (default 3), `--poll-interval` (default 10s), `--worktrees`, `--runner` (default "claude"). Graceful shutdown on SIGINT: kills all spawned agents, reclaims tasks.
- **Test:** `go build ./cmd/maquinista && maquinista orchestrate --help`

### P4-04: Extract shared prompt builders
- **Priority:** 7 | **Depends on:** P4-03
- Create `internal/prompt/prompt.go`. Extract `buildAutoPrompt`, `buildSinglePrompt`, `buildBatchPrompt` from `cmd_prompt.go` into library functions. Orchestrator uses `prompt.BuildAutoPrompt(project)` for agent startup. Agent naming: `maquinista-{project}-{N}`.
- **Test:** `go test ./internal/prompt/... ./internal/orchestrator/...`

### P4-05: Orchestrator status reporting
- **Priority:** 5 | **Depends on:** P4-03
- `Status() OrchestratorStatus` method: active/idle agent count, ready/done/failed task count, merge queue depth, uptime. Log status every poll interval. `maquinista orchestrate --status` for one-shot status query.
- **Test:** `go test ./internal/orchestrator/...`

---

## Phase 5: Decouple Telegram Topics from Agent Sessions (5 tasks)

Goal: Topics can observe any agent (not just 1:1 bound windows). The orchestrator can report to a designated topic.

### P5-01: Add topic-agent observation model to DB
- **Priority:** 7 | **Depends on:** P1-06
- Migration 007: `topic_agent_bindings(topic_id BIGINT, agent_id TEXT, binding_type TEXT DEFAULT 'observe', created_at)`. Queries: `BindTopicToAgent`, `UnbindTopicFromAgent`, `GetAgentsForTopic`, `GetTopicsForAgent`.
- **Test:** `go test ./internal/db/...`

### P5-02: Update monitor to route by agent observation
- **Priority:** 6 | **Depends on:** P5-01, P1-08
- Extend monitor's output routing: for each window's output, look up the owning agent, then look up all observing topics, route to all. Existing direct-binding routing kept for backward compatibility.
- **Test:** `go test ./internal/monitor/...`

### P5-03: Add /observe and /unobserve Telegram commands
- **Priority:** 6 | **Depends on:** P5-02
- `/observe <agent-id>` binds topic to agent. `/unobserve <agent-id>` unbinds. `/watching` lists observed agents.
- **Test:** `go test ./internal/bot/...`

### P5-04: Orchestrator Telegram notifications
- **Priority:** 5 | **Depends on:** P5-01, P4-03
- `--notify-topic` flag on `maquinista orchestrate`. Sends events (agent spawned, task done, task failed, merge conflict) to the topic via a `NotifyFunc(message string)` callback.
- **Test:** `go test ./internal/orchestrator/...`

### P5-05: Combined serve+orchestrate mode
- **Priority:** 5 | **Depends on:** P5-04
- `maquinista serve --orchestrate --orchestrate-project X --orchestrate-max-agents N --orchestrate-runner Y`. Starts orchestrator goroutine alongside bot. Orchestrator's notify function routes directly to bot's queue.
- **Test:** `go build ./cmd/maquinista && maquinista serve --help`

---

## Task Dependency DAG

```
Phase 1:
  P1-01 ──┬── P1-02 (config)
           ├── P1-03 (tmux)
           ├── P1-04 (git)
           ├── P1-05 (state)
           ├── P1-06 (db)
           ├── P1-12 (scripts)
           └── P1-13 (docker/claude)

  P1-03 + P1-04 + P1-06 ── P1-07 (agent)
  P1-02 + P1-05 ── P1-08 (render/queue/monitor/listener)
  P1-08 + P1-03 ── P1-09 (bot)
  P1-06 ── P1-10 (tui)
  P1-03 + P1-05 ── P1-11 (hook)
  ALL P1-02..P1-13 ── P1-14 (CLI wiring)
  P1-14 ── P1-15 (verification)

Phase 2:
  P1-15 ── P2-01 (parser) ── P2-02 (sync) ── P2-03 (CLI) ── P2-04 (docs)
           P1-06 ────────────┘

Phase 3:
  P1-15 ── P3-01 (interface) ──┬── P3-02 (claude) ──┐
                                ├── P3-03 (opencode)  ├── P3-05 (refactor agent)
                                └── P3-04 (custom)    │
  P1-07 ──────────────────────────────────────────────┘
  P1-06 ── P3-06 (migration)

Phase 4:
  P3-05 + P1-06 + P1-07 ── P4-01 (core loop)
  P4-01 + P1-08 ── P4-02 (NOTIFY)
  P4-02 ── P4-03 (CLI) ──┬── P4-04 (prompts)
                          └── P4-05 (status)

Phase 5:
  P1-06 ── P5-01 (DB migration)
  P5-01 + P1-08 ── P5-02 (monitor routing) ── P5-03 (commands)
  P5-01 + P4-03 ── P5-04 (notifications) ── P5-05 (combined mode)
```

**Total: 35 tasks across 5 phases.** Each phase is independently useful.

---

## Verification Criteria

### Phase 1
- `maquinista version` prints version
- `maquinista serve --help` shows tramuntana bot options
- `maquinista status --help` shows task status options
- `maquinista run --help` shows agent spawn options
- `go test ./...` passes, `go vet ./...` clean
- All `maquinista-*` scripts exist and are executable

### Phase 2
- `maquinista spec validate --dir .specs/` parses without errors
- `maquinista spec sync --dir .specs/ --project test` creates tasks in DB
- Re-sync with no changes shows "unchanged"
- `--release` transitions drafts to ready/pending

### Phase 3
- `maquinista spawn my-agent --runner claude` spawns with Claude Code
- `maquinista spawn my-agent --runner opencode` spawns with OpenCode
- `maquinista agents` shows RUNNER column
- Agent bootstrap uses runner's `InteractiveCommand`

### Phase 4
- `maquinista orchestrate --project test --max-agents 2` polls and dispatches
- Ready task → agent auto-spawned
- Dead tmux window → task reclaimed
- NOTIFY events cause immediate wake-up
- Graceful shutdown kills agents and reclaims tasks

### Phase 5
- `/observe agent-1` routes agent output to topic
- Multiple topics can observe same agent
- Orchestrator `--notify-topic` sends status updates
- `maquinista serve --orchestrate` runs both bot and orchestrator
- Old-style direct topic-window binding still works
