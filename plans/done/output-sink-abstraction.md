# Plan: Output Sink Abstraction

**Date:** 2026-04-19
**Status:** draft

---

## 1. Problem Statement

`monitor.poll()` iterates parsed entries and fans out to three hardcoded paths:

1. **Telegram queue** — `enqueueEntry()` → `queue.Enqueue(MessageTask)` — formatting, flood control, Telegram API.
2. **OutboxWriter** — callback → `NewDBOutboxWriter` → `agent_outbox` table insert.
3. **ToolEventWriter** — callback → `NewDBToolEventNotifier` → `pg_notify("tool_event")` → dashboard SSE.

Concrete defects:

- `resolveAgentFromWindow()` fires once inside `NewDBOutboxWriter` and again inside `NewDBToolEventNotifier` — two DB round-trips per tool event.
- The synthetic tool_use re-emission fix (monitor.go:222–230) only exists because the ToolEventWriter path needs to see `tool_use` even when `ParseEntries` has suppressed it for Telegram. This is a Telegram-specific suppression leaking into the dashboard path.
- Entry-type routing is duplicated across three separate loops in `poll()`.
- Adding Slack requires a fourth parallel path throughout monitor.go.

---

## 2. Unified `AgentEvent` Type

Replace `OutboxEvent` and `ToolEvent` with a single canonical event:

```go
// package monitor

type AgentEventKind string

const (
    AgentEventText       AgentEventKind = "text"
    AgentEventThinking   AgentEventKind = "thinking"
    AgentEventToolUse    AgentEventKind = "tool_use"    // standalone (not paired)
    AgentEventToolResult AgentEventKind = "tool_result" // standalone (no prior tool_use in batch)
    AgentEventToolPaired AgentEventKind = "tool_paired" // tool_use+tool_result in same batch;
                                                         // Telegram collapses, dashboard shows both
)

type AgentEvent struct {
    // Routing
    WindowID string // tmux window id, e.g. "@25"
    AgentID  string // resolved agents.id; empty if resolution failed

    // Turn routing
    UserID   int64
    ThreadID int
    ChatID   int64

    // Semantics
    Kind AgentEventKind

    // Content (which fields are non-empty depends on Kind)
    Role      string // "assistant" | "user"
    Text      string
    ToolName  string
    ToolUseID string
    ToolInput string // formatted summary
    IsError   bool
}
```

Key decisions:
- `AgentEventToolPaired` carries everything both Telegram and the dashboard need. Telegram renders it as one combined message. The dashboard SSE sink re-emits it as sequential tool_use + tool_result pg_notify calls. Neither sink needs to know the other's semantics.
- `AgentID` is resolved once per window per poll cycle in `poll()` before iterating sinks — eliminates the double DB round-trip.
- `ParsedEntry` stays unchanged; it is internal to the monitor package. `AgentEvent` is the externally-visible type that sinks consume.

---

## 3. `OutputSink` Interface

```go
// package monitor

// OutputSink receives AgentEvents from the monitor.
// Implementations must be goroutine-safe.
// Must not retain the AgentEvent pointer after Handle returns.
type OutputSink interface {
    Handle(e AgentEvent)
    Name() string
}
```

One method. Lifetime management (DB pool, API clients) is the constructor's responsibility.

---

## 4. `MultiSink` Fan-Out

```go
// package monitor

type MultiSink struct {
    sinks []OutputSink
}

func NewMultiSink() *MultiSink { return &MultiSink{} }

// Add appends a sink. Not goroutine-safe; call before Run.
func (ms *MultiSink) Add(s OutputSink) { ms.sinks = append(ms.sinks, s) }

// Emit delivers e to every registered sink in registration order.
func (ms *MultiSink) Emit(e AgentEvent) {
    for _, s := range ms.sinks {
        s.Handle(e)
    }
}
```

Emit is synchronous. Sinks that need async dispatch (Telegram) own their own goroutines internally.

---

## 5. Where Window ID Resolution Moves

Into `poll()`, called once per window per poll cycle:

```go
var agentID string
if m.pool != nil {
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    agentID, _ = resolveAgentFromWindow(ctx, m.pool, sess.WindowID)
    cancel()
}

for _, pe := range parsed {
    e := buildAgentEvent(sess.WindowID, agentID, userID, threadID, chatID, pe)
    m.sink.Emit(e)
}
```

`resolveAgentFromWindow` moves to `internal/monitor/resolve.go` (still unexported).

---

## 6. `buildAgentEvent` — Paired Tool Detection

```go
func buildAgentEvent(windowID, agentID string, userID int64, threadID int, chatID int64, pe ParsedEntry) AgentEvent {
    e := AgentEvent{
        WindowID:  windowID,
        AgentID:   agentID,
        UserID:    userID,
        ThreadID:  threadID,
        ChatID:    chatID,
        Role:      pe.Role,
        Text:      pe.Text,
        ToolName:  pe.ToolName,
        ToolUseID: pe.ToolUseID,
        ToolInput: pe.ToolInput,
        IsError:   pe.IsError,
    }
    switch pe.ContentType {
    case "text":
        e.Kind = AgentEventText
    case "thinking":
        e.Kind = AgentEventThinking
    case "tool_use":
        e.Kind = AgentEventToolUse
    case "tool_result":
        // When ToolName is non-empty and not "unknown", ParseEntries resolved
        // the pair from pending — the tool_use was suppressed for Telegram.
        // Mark as Paired so dashboard sink re-emits both sides.
        if pe.ToolName != "" && pe.ToolName != "unknown" {
            e.Kind = AgentEventToolPaired
        } else {
            e.Kind = AgentEventToolResult
        }
    }
    return e
}
```

---

## 7. Sink Designs

### TelegramSink

Wraps the existing `queue.Queue`. Zero changes to queue internals.

```go
type TelegramSink struct{ queue *queue.Queue }

func (t *TelegramSink) Name() string { return "telegram" }

func (t *TelegramSink) Handle(e AgentEvent) {
    if e.ChatID == 0 { return } // no Telegram binding

    var task queue.MessageTask
    task.UserID, task.ThreadID, task.ChatID, task.WindowID =
        e.UserID, e.ThreadID, e.ChatID, e.WindowID

    switch e.Kind {
    case AgentEventText:
        text := render.FormatText(e.Text)
        if e.Role == "user" { text = "👤 " + text }
        task.Parts, task.ContentType = []string{text}, "content"
    case AgentEventThinking:
        task.Parts, task.ContentType = []string{render.FormatThinking(e.Text)}, "content"
    case AgentEventToolUse:
        task.Parts, task.ContentType = []string{render.FormatToolUse(e.ToolName, e.ToolInput)}, "tool_use"
        task.ToolUseID = e.ToolUseID
    case AgentEventToolResult, AgentEventToolPaired:
        // Telegram collapses paired tool_use+result to one message.
        task.Parts = []string{render.FormatToolResult(e.ToolName, e.ToolInput, e.Text, e.IsError)}
        task.ContentType, task.ToolUseID = "tool_result", e.ToolUseID
    default:
        return
    }
    t.queue.Enqueue(task)
}
```

### OutboxSink

```go
type OutboxSink struct {
    pool *pgxpool.Pool
    amap *mailbox.ActiveInboxMap
}

func (s *OutboxSink) Name() string { return "outbox" }

func (s *OutboxSink) Handle(e AgentEvent) {
    if s.pool == nil || e.AgentID == "" || e.Role != "assistant" { return }
    var text string
    switch e.Kind {
    case AgentEventText:    text = render.FormatText(e.Text)
    case AgentEventThinking: text = render.FormatThinking(e.Text)
    default: return
    }
    if text == "" { return }

    // AgentID already resolved — no second DB round-trip.
    ctx := context.Background()
    content, _ := json.Marshal(struct {
        Type string `json:"type"`
        Text string `json:"text"`
        Role string `json:"role,omitempty"`
    }{"text", text, e.Role})

    var inReplyTo *uuid.UUID
    if s.amap != nil {
        if raw := s.amap.Get(e.AgentID); raw != "" {
            if id, err := uuid.Parse(raw); err == nil { inReplyTo = &id }
        }
    }
    // ... begin/AppendOutbox/commit (same as NewDBOutboxWriter body)
}
```

### ToolEventSink

```go
type ToolEventSink struct{ pool *pgxpool.Pool }

func (s *ToolEventSink) Name() string { return "tool_event" }

func (s *ToolEventSink) Handle(e AgentEvent) {
    if s.pool == nil || e.AgentID == "" || e.ToolUseID == "" { return }
    ctx := context.Background()
    switch e.Kind {
    case AgentEventToolPaired:
        // Re-emit suppressed tool_use so dashboard banner sees both sides.
        s.notify(ctx, e.AgentID, "tool_use", e.ToolName, e.ToolUseID, false)
        s.notify(ctx, e.AgentID, "tool_result", e.ToolName, e.ToolUseID, e.IsError)
    case AgentEventToolUse:
        s.notify(ctx, e.AgentID, "tool_use", e.ToolName, e.ToolUseID, false)
    case AgentEventToolResult:
        s.notify(ctx, e.AgentID, "tool_result", e.ToolName, e.ToolUseID, e.IsError)
    }
}

func (s *ToolEventSink) notify(ctx context.Context, agentID, typ, name, useID string, isError bool) {
    payload, _ := json.Marshal(map[string]any{
        "agent_id": agentID, "type": typ,
        "tool_name": name, "tool_use_id": useID, "is_error": isError,
    })
    if _, err := s.pool.Exec(ctx, "SELECT pg_notify($1, $2)", "tool_event", string(payload)); err != nil {
        log.Printf("tool_event sink: pg_notify: %v", err)
    }
}
```

The re-emission of the paired tool_use is now explicit and contained entirely in this sink. The monitor never needs to know that the dashboard wants tool_use before tool_result.

---

## 8. `cmd_start.go` After Phase 3

```go
sink := monitor.NewMultiSink()
sink.Add(monitor.NewTelegramSink(q))
if pool != nil {
    sink.Add(monitor.NewOutboxSink(pool, activeInboxMap))
    sink.Add(monitor.NewToolEventSink(pool))
}
mon := monitor.New(cfg, b.State(), ms, sink, pool)
```

### Adding Slack (future validation)

```go
// internal/monitor/sink_slack.go
type SlackSink struct{ client *slack.Client; channelID string }
func (s *SlackSink) Name() string { return "slack" }
func (s *SlackSink) Handle(e AgentEvent) { /* format + post */ }

// cmd_start.go — zero monitor.go changes
if cfg.SlackToken != "" {
    sink.Add(monitor.NewSlackSink(slackClient, cfg.SlackChannel))
}
```

---

## 9. Migration Phases

| Phase | What | Risk | Rollback |
|---|---|---|---|
| 1 | Add `AgentEvent`, `AgentEventKind`, `OutputSink`, `MultiSink`, `buildAgentEvent` — additive only | None | Delete new files |
| 2 | Implement `TelegramSink`, `OutboxSink`, `ToolEventSink` with tests | Low — old callbacks still wired | Delete sink files |
| 3 | Wire `MultiSink` into `Monitor`, replace three poll loops with one, update `cmd_start.go` | Medium | Revert monitor.go + cmd_start.go |
| 4 | Delete `outbox.go`, `tool_events.go`, remove `OutboxWriter`/`ToolEventWriter` fields | Low — logic already in sinks | Restore from git |

Each phase is one commit. `make test` must pass green between phases.

---

## 10. File Layout

| File | Disposition |
|---|---|
| `internal/monitor/monitor.go` | Modified: remove `OutboxWriter`/`ToolEventWriter` fields; add `pool`, `sink`; collapse poll loops; remove `enqueueEntry` |
| `internal/monitor/event.go` | **New**: `AgentEvent`, `AgentEventKind`, `buildAgentEvent` |
| `internal/monitor/sink.go` | **New**: `OutputSink` interface, `MultiSink` |
| `internal/monitor/resolve.go` | **New**: `resolveAgentFromWindow` moved from outbox.go |
| `internal/monitor/sink_telegram.go` | **New**: `TelegramSink` |
| `internal/monitor/sink_outbox.go` | **New**: `OutboxSink` (replaces outbox.go) |
| `internal/monitor/sink_tool_event.go` | **New**: `ToolEventSink` (replaces tool_events.go) |
| `internal/monitor/outbox.go` | **Deleted** after Phase 3 |
| `internal/monitor/tool_events.go` | **Deleted** after Phase 3 |
| `cmd/maquinista/cmd_start.go` | Modified: construct MultiSink, wire sinks |
| `internal/monitor/event_test.go` | **New**: `TestBuildAgentEvent_*` |
| `internal/monitor/sink_test.go` | **New**: `TestMultiSink_FanOut` |
| `internal/monitor/sink_outbox_test.go` | **New**: ports existing outbox_test.go cases |
| `internal/monitor/sink_tool_event_test.go` | **New**: ToolEventSink unit tests |

---

## 11. Interaction with Other Plans

- **`retire-legacy-tmux-paths.md`**: When `queue.Queue` is retired, only `TelegramSink` changes. Zero monitor.go impact.
- **`per-agent-sidecar.md` Phase 2**: When transcript tailing moves into per-agent sidecars, they call `sink.Emit` directly. `OutputSink`/`MultiSink` are reused unchanged.
- **Dashboard live tool calls**: The `tool_event` pg_notify payload and channel name are unchanged — `ToolEventSink` emits the same JSON. Dashboard SSE code needs no changes.

---

## 12. Comparison: This Approach vs. Alternatives

### Chosen: Minimal Interface, Inline Fan-Out

One-method interface. `MultiSink` is 12 lines. Each sink is a named type with a constructor and `Handle`. No reflection, no plugin registry, no lifecycle hooks.

**Advantages:** Fits in a 15-minute code review. Compile-time enforcement via Go interface. No global state — `MultiSink` constructed in `cmd_start.go` and passed in. Staged migration preserves existing callbacks while new types are introduced.

**Disadvantages:** `Handle` is synchronous on the monitor goroutine — a slow sink delays all others. Mitigation: sinks that need async dispatch own their own goroutines (Telegram delegates to `queue.Queue`). No built-in retry or backpressure at `MultiSink` level — each sink handles its own.

---

### Hermes-Agent: `BasePlatformAdapter` ABC + `DeliveryRouter`

**Pattern:** Abstract base class with `send(chat_id, content, reply_to, metadata)`, a `DeliveryRouter` fan-out, and a factory+enum registry. 19 concrete adapters (Telegram, Slack, Discord, Webhook, WhatsApp, Signal, Matrix, ...).

**What it gets right:** Clean adapter abstraction where platforms are genuinely symmetric — all send a message to a chat. Factory+enum makes adding a new platform mechanical: add to enum, implement class, add factory case.

**Why it doesn't fit here:** Maquinista's destinations are not symmetric. Telegram is stateful (flood control, per-user channels, message-ID tracking for edits). The outbox is a transactional DB write. The dashboard uses pg_notify with a specific JSON payload. A shared `send(content)` signature would force all three into the same shape, requiring either a kitchen-sink metadata bag or separate sub-methods — recreating the problem at one level up.

The `HookRegistry` lifecycle system is appealing but solves a different problem (plugin-time side-effects before delivery). If pre-delivery hooks are needed later, adding a `[]Hook` slice to `MultiSink.Emit` is a small extension.

---

### OpenClaude: `AnalyticsSink` + Queue-Before-Attach + Notification Hooks

**Pattern:** `AnalyticsSink` interface with `logEvent`/`logEventAsync`. Events queue in memory until `attachAnalyticsSink()` is called at startup (drain via `queueMicrotask()`). Notification hook system lets plugins register handlers for `'Notification'` events that execute before OS notification dispatch.

**What it gets right:** The buffering behaviour is elegant for analytics where events fire before the logger is ready. The plugin hook pattern (register by event name, execute in order) is a clean extension point for custom destinations without touching core code.

**Why it doesn't fit here:** Maquinista has no startup race — the monitor doesn't start until `cmd_start.go` has fully constructed the sink. A buffering layer adds complexity without solving an actual problem. The registry string lookup (`registerHook("Notification", handler)`) adds indirection that makes call graphs harder to trace — valuable in a large multi-team codebase with third-party plugins, unnecessary when there are three destinations known at compile time.

---

### Summary

| Criterion | Chosen (minimal interface) | Hermes (adapter ABC) | OpenClaude (sink+hooks) |
|---|---|---|---|
| Complexity | Low — 1 interface method | Medium — class hierarchy | Medium — buffer + registry |
| Asymmetric destination semantics | Each sink handles its own | Forces a common API shape | Each sink handles its own |
| Startup buffering race | N/A (none exists) | N/A | Solved via queue-before-attach |
| Extension point for new destination | Add type + register in cmd_start.go | Enum + factory case | Registry string lookup |
| Testability | Direct construction with stubs | Mock adapter | Mock sink |
| Go idiomatic | Yes | No (Python ABC) | Partial |

The honest tradeoff: the chosen approach is the least exciting and the most appropriate. It eliminates the three stated problems (double resolution, scattered routing, Slack requires four paths) with the smallest possible surface area change. The Hermes and OpenClaude patterns are more powerful but import complexity that the current problem doesn't warrant.
