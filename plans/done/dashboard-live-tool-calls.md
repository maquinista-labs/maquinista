# Plan: Live Tool Calls & Streaming Chat Updates in Dashboard

**Status:** draft  
**Date:** 2026-04-19

## Problem

The Telegram bot shows live tool-call activity (e.g. `🔮 Swooping… (22s · ↓ 416 tokens)`) with messages that appear, update, and disappear as processing evolves. The dashboard chat view has none of this — it's static and only shows persisted timeline entries. Users cannot see what the agent is doing right now.

## Goals

1. Show in-progress tool calls with elapsed time and token count (like Telegram).
2. Update/replace the "thinking" message when the tool call completes.
3. No full-page refresh — push updates to the open chat via WebSocket or SSE.
4. Graceful degradation: if the stream is lost, the chat still shows the last persisted state.

---

## Architecture

### Data flow today

```
Agent process → DB (timeline rows) → Dashboard polls/SSE → Chat renders rows
```

### Proposed data flow

```
Agent process → live_events channel (pg NOTIFY) → SSE endpoint → React hook → Chat overlay
              ↘ DB (timeline rows, persisted)
```

Use PostgreSQL `LISTEN/NOTIFY` — already available, no new infra needed.

---

## Implementation Steps

### 1. Define `live_events` pg NOTIFY channel

- Payload: `{ agent_id, type, data }` as JSON string (pg NOTIFY max 8KB).
- Types to emit:
  - `tool_start` → `{ tool_name, call_id, started_at }`
  - `tool_delta` → `{ call_id, tokens_in, tokens_out, elapsed_ms }`
  - `tool_end` → `{ call_id, elapsed_ms, tokens_in, tokens_out }`
  - `message_start` / `message_delta` / `message_end` (assistant streaming text)

- Go agent process notifies after each Claude API event in the stream loop.
  - File to modify: wherever the Claude API stream is consumed (likely `internal/bot/` or `internal/agent/`).

### 2. SSE endpoint: `/api/agents/[id]/live`

- New Next.js route handler using `Response` with `ReadableStream`.
- Opens a pg `LISTEN live_events` connection per request.
- Filters by `agent_id`.
- Emits SSE events matching the payload types.
- Closes connection cleanly on client disconnect.

```ts
// src/app/api/agents/[id]/live/route.ts
export async function GET(req, { params }) {
  const { id } = await params;
  // pg LISTEN, filter agent_id === id, pipe to SSE stream
}
```

### 3. React hook: `useAgentLive(agentId)`

- Connects to `/api/agents/${id}/live` via `EventSource`.
- Maintains local state: `Map<call_id, LiveToolCall>` where:
  ```ts
  type LiveToolCall = {
    call_id: string;
    tool_name: string;
    started_at: number;
    elapsed_ms: number;
    tokens_in: number;
    tokens_out: number;
    status: "running" | "done";
  };
  ```
- Ticks elapsed time locally via `setInterval` (1s) for the running display.
- Clears a `tool_end` entry after 2s (mirrors Telegram behaviour).

### 4. UI: `LiveToolCallBanner` component

Shown above the chat timeline when there are active tool calls.

```
┌─────────────────────────────────────────────┐
│ 🔮 web_search  22s · ↓ 416 tok              │
│ 🔮 read_file   4s  · ↓ 12 tok               │
└─────────────────────────────────────────────┘
```

- Rendered in `agent-detail-tabs.tsx` or the conversation tab component.
- Dismisses automatically when `status === "done"` entries expire.

### 5. Streaming assistant text (stretch)

- `message_delta` events carry partial assistant text.
- Show a `TypingMessage` row at the bottom of the chat that appends tokens in real time.
- On `message_end`, the persisted `timeline` row arrives and replaces it.

---

## Files to Create / Modify

| File | Change |
|------|--------|
| `internal/agent/stream.go` (or equivalent) | Emit pg NOTIFY on tool_start/delta/end |
| `src/app/api/agents/[id]/live/route.ts` | New SSE endpoint |
| `src/hooks/use-agent-live.ts` | New hook |
| `src/components/dash/live-tool-call-banner.tsx` | New component |
| `src/components/dash/agent-detail-tabs.tsx` | Mount banner in conversation tab |

---

## Open Questions

- **Token counts**: Does the Go agent already track per-call token deltas? If not, add counters alongside each tool call.
- **Reconnection**: `EventSource` auto-reconnects; ensure the SSE endpoint resumes correctly after reconnect (send current in-flight calls on connect).
- **Multiple tabs**: Each open tab connects its own `EventSource`; pg LISTEN is cheap so this is fine.
- **pg connection pool**: SSE route needs a long-lived pg connection; ensure it doesn't starve the pool. Use a dedicated `LISTEN` connection separate from the query pool.

---

## Out of Scope

- Persisting live events to DB (they are ephemeral by design).
- Showing tool call history (that's already in the timeline).
- WebSocket transport (SSE is simpler and sufficient for server→client push).
