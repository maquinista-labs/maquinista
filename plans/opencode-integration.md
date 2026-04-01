# OpenCode Integration Improvements

Derived from a comparison of the Claude Code source and Volta's current OpenCode runner.
The goal is to close the capability gap so OpenCode agents are as reliable as Claude Code agents.

---

## Problem Summary

Volta's `OpenCodeRunner` was scaffolded but never fully integrated. Four categories of gaps:

| Category | Severity | Impact |
|----------|----------|--------|
| Monitor parsing is Claude Code-specific | High | Orchestrator can't detect stuck/interactive OpenCode agents |
| Session tracking missing | High | Telegram topic→session binding broken for OpenCode |
| `PlannerCommand` semantics wrong | High | Planner mode sends system prompt as user message |
| Missing `LaunchCommand` permission env | Medium | Interactive launch prompts for permissions |
| No model default | Medium | Falls back to opencode's global config — unpredictable |
| No budget control | Low | Unbounded spend on runaway agents |

---

## Tasks

### OC-01 — Add `MonitorProfile` to `AgentRunner` interface

**File:** `internal/runner/runner.go`, `internal/monitor/terminal.go`

Add a `MonitorProfile()` method to `AgentRunner` returning runner-specific TUI parsing parameters:

```go
type MonitorProfile struct {
    SpinnerChars   string   // runes that indicate active status
    SeparatorRunes []rune   // runes that make up chrome separator lines
    MinSeparatorLen int     // minimum rune count for separator detection
    UIPatterns     []UIPattern
}
```

- `ClaudeRunner.MonitorProfile()` returns the current hardcoded values (·✻✽✶✳✢, ─/━, 20, existing patterns)
- `OpenCodeRunner.MonitorProfile()` returns OpenCode-specific values (TBD from observing OpenCode output)
- `monitor.StripPaneChrome`, `ExtractStatusLine`, `IsInteractiveUI` accept a `MonitorProfile` parameter

**Acceptance:** Both runners compile; Claude runner behavior is unchanged; OpenCode gets its own profile.

---

### OC-02 — Fix `PlannerCommand` for OpenCode

**File:** `internal/runner/opencode.go`

Current:
```go
func (o *OpenCodeRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
    return fmt.Sprintf("opencode run --prompt \"$(cat %s)\"", systemPromptPath)
}
```

`--prompt` sends the content as a user message. The planner system prompt needs to frame the
agent's role before any user input. Options (in preference order):

1. Check if `opencode` supports `--system-prompt` flag — use it if so
2. If not, prepend the system prompt file content to the task prompt with a role-framing header
3. As a last resort, document planner mode as unsupported for OpenCode

**Acceptance:** `volta spawn --runner opencode --role planner` starts an agent that treats the
planner prompt as behavioral context, not as a task to complete.

---

### OC-03 — Session tracking fallback for OpenCode

**File:** `internal/agent/agent.go`, `internal/state/session_map.go`

When `r.HasSessionHook() == false`, after spawning the agent write a preliminary `session_map.json`
entry keyed on `tmuxSession:windowID` with the agent ID as the session ID:

```go
if !r.HasSessionHook() {
    state.WriteSessionMapEntry(tmuxSession, agentID, state.SessionMapEntry{
        SessionID:  agentID,  // stable proxy — no real session ID available
        CWD:        cfg.WorkDir,
        WindowName: agentID,
    })
}
```

This lets bot handlers route Telegram messages to OpenCode windows the same way they do for
Claude Code, using the agent ID as the stable key.

**Acceptance:** After `volta spawn --runner opencode`, `volta status` shows the agent; a Telegram
message sent to its topic reaches the correct tmux window.

---

### OC-04 — Fix `LaunchCommand` permission bypass

**File:** `internal/runner/opencode.go`

Current:
```go
func (o *OpenCodeRunner) LaunchCommand(cfg Config) string {
    return "opencode"
}
```

When `SkipPermissions` is true, the env override (`OPENCODE_PERMISSION=skip`) must be set
before the binary runs. Since `LaunchCommand` is used for interactive Telegram session binding
(not via `sendBootstrap`), the env var isn't exported:

```go
func (o *OpenCodeRunner) LaunchCommand(cfg Config) string {
    if o.SkipPermissions {
        return "OPENCODE_PERMISSION=skip opencode"
    }
    return "opencode"
}
```

**Acceptance:** An OpenCode agent started interactively via `LaunchCommand` does not prompt for
tool permissions.

---

### OC-05 — Add model default

**File:** `internal/runner/opencode.go`

```go
func (o *OpenCodeRunner) NonInteractiveArgs(prompt string, cfg Config) []string {
    model := o.Model
    if model == "" {
        model = "anthropic/claude-sonnet-4-5"
    }
    // ...
}
```

Same pattern as `ClaudeRunner` defaulting to `"sonnet"`.

**Acceptance:** `volta run --runner opencode` without explicit model uses a predictable model.

---

### OC-06 — Observe OpenCode's actual TUI output and document UIPatterns

**Action:** Run `opencode` in a tmux pane, capture pane text during normal operation and during
permission/question prompts, identify:

- Spinner / busy indicator characters
- Chrome separator pattern (if any)
- Interactive prompt markers (permission dialogs, questions)

Document findings as comments in `OpenCodeRunner.MonitorProfile()` implementation (OC-01).

**Acceptance:** The monitor correctly detects active/idle/interactive state for an OpenCode agent.

---

## Execution Order

```
OC-06 (observe TUI output)
  └─> OC-01 (MonitorProfile interface + implementations)

OC-02 (PlannerCommand fix)          — independent
OC-03 (session tracking fallback)   — independent
OC-04 (LaunchCommand fix)           — independent
OC-05 (model default)               — independent
```

OC-02 through OC-05 can be done in any order or in parallel.
OC-06 must precede OC-01 (need observed data to fill in OpenCode's profile).

---

## Checklist

- [ ] OC-01: Add `MonitorProfile` to `AgentRunner`, thread into monitor functions
- [ ] OC-02: Fix `PlannerCommand` semantics for OpenCode
- [x] OC-03: Write preliminary `session_map` entry when `HasSessionHook() == false`
- [ ] OC-04: Export `OPENCODE_PERMISSION=skip` in `LaunchCommand` when `SkipPermissions`
- [ ] OC-05: Default model to `anthropic/claude-sonnet-4-5` when `Model == ""`
- [ ] OC-06: Observe and document OpenCode's TUI patterns (prerequisite for OC-01)
