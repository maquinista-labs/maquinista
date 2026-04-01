# Current State: Plan/Spec Generation Before Task Launch

> Goal: Document all existing ways a plan or spec is generated prior to task execution, covering both CLI and Telegram entry points, so we can identify duplication and simplification opportunities.

---

## Overview

There are **5 distinct workflows** for generating a plan/spec and transitioning tasks from "idea" to "ready for execution":

| # | Entry Point | Medium | Output | Status After |
|---|-------------|--------|--------|--------------|
| 1 | `/t_plan <description>` | Telegram | JSON batch of tasks | `ready` (immediate) |
| 2 | `/plan [project]` + `/plan release` | Telegram | Draft tasks via agent | `draft` → `ready` |
| 3 | `volta add` (direct) | CLI | Single task | `ready` or `draft` |
| 4 | `volta schedule` + `volta cron tick` | CLI | Recurring task batches from template | `draft` → `ready` |

---

## Workflow 1: `/t_plan` — Telegram Interactive Plan

**Entry point:** `internal/bot/plan_commands.go`

**Flow:**
```
User: /t_plan "add dark mode to settings"
  → handlePlanCommand()
  → buildPlanningPrompt() — wraps description in structured prompt
  → executePlan() — sends prompt to a tmux Claude window
  → Monitor watches output for "PLAN_JSON:" marker
  → HandlePlanFromMonitor() — parses JSON array of PlanTask structs
  → showPlanApproval() — renders inline keyboard with task list
  → User clicks [Approve]
  → handlePlanApprove() — calls bridge.AddWithDeps() for each task
  → Tasks created with status "ready" (or "pending" if deps unmet)
```

**Plan format (in-flight, not persisted as files):**
```go
type PlanTask struct {
    Title    string  // imperative description
    Body     string  // implementation details
    Priority int     // 1–5
    After    []int   // 0-based indices into same array (deps)
}
```

**Key characteristics:**
- Entirely in-band: plan lives only in the Claude output buffer until parsed
- Dependencies expressed as array indices, not task IDs
- Plan approval is a single Telegram UI interaction
- Tasks go directly to `ready` — no draft/release step
- No spec file produced or persisted

---

## Workflow 2: `/plan` — Telegram Planner Mode

**Entry points:**
- `internal/bot/planner_commands.go` — bot-side commands
- `cmd/volta/cmd_planner.go` — CLI equivalents (`volta planner start|stop|reopen|status`)

**Flow:**
```
User: /plan myproject
  → plannerStart()
  → Creates a Telegram forum topic: "Planner: myproject"
  → Spawns a Claude Code tmux window with planner-system-prompt.md
  → User converses with Claude to define tasks
  → Claude calls: volta add --status draft --project myproject [--after <ids>] [--priority N]
  → Tasks accumulate in DB as status="draft"

User: /plan release
  → plannerRelease()
  → Calls: minuano draft-release --all --project myproject
  → Transitions: draft → ready (or → pending if deps unmet)
  → Pending tasks auto-promote to ready as deps complete
```

**Planner system prompt:** `claude/planner-system-prompt.md`
- Instructs Claude to use `volta add --status draft` only
- Teaches dependency syntax (`--after <id>`)
- Warns against spawning executors or using `volta run`

**Key characteristics:**
- Interactive conversation loop — Claude is the planner agent
- Session tracked in DB (`planner_sessions` table)
- Tasks are created incrementally, not in a batch
- Explicit release step required before tasks are claimable
- No structured spec file produced

---

## Workflow 3: `volta add` / `/p_add` — Direct Task Creation

**Entry points:**
- `cmd/volta/cmd_add.go` — `volta add <title>`
- `internal/bot/add_task.go` — `/p_add` Telegram wizard

**Flow (CLI):**
```
volta add "Optimize DB query" --priority 8 --after task-abc --requires-approval
  → db.CreateTask() — status="ready" by default, or "draft" with --status draft
  → db.AddDependency() for each --after
```

**Flow (Telegram wizard):**
```
User: /p_add "Optimize DB query"
  → Multi-step wizard:
      1. Shows priority inline keyboard (1–10)
      2. Prompts for optional body (user replies to message)
      3. createTask() → bridge.Add()
  → Task created immediately as "ready"
```

**Key characteristics:**
- Single-task creation only — no batch or dependency graph
- No planning AI involved
- CLI default is `ready`; wizard default is also `ready`
- Simplest and most direct path

---

## Workflow 4: `volta schedule` — Recurring Scheduled Tasks

**Entry points:**
- `cmd/volta/cmd_schedule.go` — schedule management subcommands
- `cmd/volta/cmd_cron.go` — `volta cron tick` daemon

**Flow:**
```
1. Author a template JSON file:
   [
     { "ref": "setup", "title": "Prepare env", "body": "...", "priority": 8, "after": [] },
     { "ref": "run",   "title": "Execute job",  "body": "...", "priority": 7, "after": ["setup"] }
   ]

2. Register the schedule:
   volta schedule add weekly-job \
     --cron "0 9 * * 1" \
     --template ./my-template.json \
     --project myproject

3. Run the cron daemon (long-running process):
   volta cron tick
     → Polls every 30s
     → Fetches enabled schedules where next_run <= NOW()
     → Calls instantiateTemplate() for each due schedule
     → Creates tasks as status="draft", resolves deps via ref names
     → Updates last_run and computes new next_run

4. Release draft tasks:
   volta approve draft-release --all --project myproject
     → draft → ready (or → pending if deps unmet)
```

**Schedule management commands:**
```
volta schedule list [--project <id>]        — NAME | CRON | NEXT RUN | LAST RUN | ENABLED
volta schedule run <name>                    — trigger immediately (skips cron timer)
volta schedule enable <name>                 — re-enable and recompute next_run
volta schedule disable <name>                — pause without deleting
```

**Template format** (`TemplateNode` in `cmd/volta/cmd_schedule.go`):
```go
type TemplateNode struct {
    Ref              string   // unique key for dependency resolution
    Title            string
    Body             string
    Priority         int      // defaults to 5 if 0
    TestCmd          string   // stored in task metadata
    RequiresApproval bool
    After            []string // refs of dependency nodes (not task IDs)
}
```

**Cron expression format:** 5-field (`minute hour dom month dow`), e.g. `0 9 * * 1` = every Monday at 9 AM.

**Key characteristics:**
- Cron-triggered or manually run via `volta schedule run`
- Template dependencies use `ref` strings resolved at instantiation — not task IDs
- Tasks always created as `draft`; explicit release step required
- No Telegram commands — CLI-only
- Schedule state (`last_run`, `next_run`, `enabled`) persisted in DB

---

## Current Inconsistencies

### 1. Status at creation differs by workflow

| Workflow | Default status after creation |
|----------|-------------------------------|
| `/t_plan` | `ready` (immediate) |
| `/plan` mode | `draft` (requires release) |
| `volta add` / `/p_add` | `ready` (immediate) |
| `volta schedule` | `draft` (requires release) |

### 2. Dependency format is inconsistent

| Workflow | Dependency format |
|----------|-------------------|
| `/t_plan` PlanTask | Array indices (`After: [0, 1]`) |
| `volta add` | Task ID strings (`--after abc-123`) |
| `/plan` agent | Task ID strings (agent uses `--after <id>`) |
| `volta schedule` template | Ref strings (`after: ["setup"]`) |

### 3. Approval/review gates differ

| Workflow | Human review point |
|----------|--------------------|
| `/t_plan` | Pre-creation: Telegram approve/cancel |
| `/plan` mode | Post-creation: `/plan release` |
| `volta add` | Optional: `--requires-approval` per task |
| `volta schedule` | Post-creation: `draft-release` required |

### 4. Planner agent vs direct task creation

`/t_plan` and `/plan` both use AI to generate tasks, but:
- `/t_plan` has Claude output a JSON batch in one shot, then parses it
- `/plan` has Claude run imperatively (calling `volta add` multiple times)

These two approaches are architecturally different despite solving the same problem.

---

## Key Files Reference

| File | Role |
|------|------|
| `internal/bot/plan_commands.go` | `/t_plan` flow, PLAN_JSON parsing |
| `internal/bot/planner_commands.go` | `/plan` bot commands |
| `cmd/volta/cmd_planner.go` | CLI planner commands |
| `internal/bot/add_task.go` | `/p_add` wizard |
| `cmd/volta/cmd_add.go` | `volta add` CLI |
| `cmd/volta/cmd_schedule.go` | Schedule management + `instantiateTemplate()` |
| `cmd/volta/cmd_cron.go` | `volta cron tick` daemon |
| `internal/orchestrator/orchestrator.go` | Main orchestrator loop |
| `internal/db/queries.go` | CreateTask, DraftRelease, AddDependency, Schedule queries |
| `internal/bridge/bridge.go` | Bridge to minuano CLI |
| `claude/planner-system-prompt.md` | Planner agent instructions |

---

## Open Questions for Simplification

1. Should `/t_plan` adopt the draft/release pattern to match all other batch workflows?
2. Can the two "AI planner" approaches (`/t_plan` one-shot JSON vs `/plan` interactive) be unified?
3. Should dependency format be standardized everywhere — task IDs for `volta add`/`/plan`, ref strings for templates, dropping array indices from `/t_plan`?
