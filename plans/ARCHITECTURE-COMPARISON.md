# Architecture Comparison: Minuano + Tramuntana vs. Claws Alternatives

## 1. Context & Framing

### The Two Systems

**Minuano** is a PostgreSQL-backed coordination layer for pull-based task orchestration. Agents claim tasks from a shared queue, write observations and results to `task_context`, and inherit context across dependency chains. No persistent agent processes — a bash script and a SQL schema are enough to run it.

**Tramuntana** is a Telegram bot that bridges human operators to Claude Code sessions and Minuano projects. It manages per-user message queues, JSONL-based transcript monitoring (byte-offset polling), and Telegram thread/window/project bindings. It is stateless between restarts except for `monitor_state.json`.

Together they form a pull/zero-token model: no agent is running unless a human or a task trigger causes it to run. Context accumulates in Postgres. No Mayor, no always-on coordinator, no role hierarchy.

### Inspirations Examined

**Gas Town / Beads**: A push-based, role-heavy orchestration system backed by git. Agents have persistent identity, a Mayor broadcasts work, and a Refinery/Witness pattern handles merge queue conflicts. Context lives in git history and mailboxes.

**OpenClaw**: A full-stack TypeScript implementation with a WebSocket gateway, session archives, multi-agent routing, per-group activation modes, and sequential vs. parallel queue processing per session.

**Claws alternatives** (lighter implementations):
- **Nanobot** (Python): asyncio.Queue, inbound/outbound bus separation, two-layer memory (MEMORY.md + HISTORY.md), LLM-driven consolidation
- **Picoclaw** (Go): three-channel bus (inbound/outbound/outboundMedia), port of nanobot's architecture
- **OpenClaw** (TypeScript): full-stack reference with WebSocket gateway
- **Tinyclaw** (Bun/TypeScript): four-layer context compaction, SQLite FTS5 history search, Blackboard inter-agent pattern, SubagentManager

### Why Pull Over Push

Gas Town's push model requires an always-on Mayor and persistent agent identities. Every idle moment costs tokens or requires a polling daemon. Minuano replaces all of this with `SELECT ... FOR UPDATE SKIP LOCKED` — a task is claimed atomically, processed, and released. No tokens spent waiting. No crash recovery ceremony. Postgres handles the coordination that Gas Town's Mayor handles in LLM context.

---

## 2. Queue Processing

### Tramuntana's Queue (Current)

Each Telegram user gets a dedicated goroutine with a buffered Go channel (capacity 100). Incoming messages are processed serially per user:

- **Content merging**: consecutive text messages are concatenated up to 3800 characters before being sent to Claude Code, reducing round-trips
- **In-place tool result editing**: instead of posting a new Telegram message for each tool call result, Tramuntana edits the previous message — dramatically reduces message count in active sessions
- **FloodControl**: on Telegram 429 (rate limit), the user's goroutine enters a 30-second ban; ephemeral messages (status updates) are dropped during the ban, not queued

### Claws Alternatives

| System | Queue Design | Flood Control |
|--------|-------------|---------------|
| Nanobot | `asyncio.Queue` with inbound/outbound separation | None |
| Picoclaw | 3-channel bus: inbound / outbound / outboundMedia, buffer 64 | None |
| OpenClaw | Sequential vs. parallel modes per session, per-group activation | Not documented |
| Tinyclaw | Event pub/sub, `packages/queue/` | TimeoutEstimator for subagent calls |

### What to Borrow

**OpenClaw's sequential/parallel queue modes**: Tramuntana currently always merges. A per-topic toggle — "batch mode: queue strictly, don't merge" vs. "stream mode: merge up to 3800 chars" — would be useful for topics where message ordering matters (e.g., a topic driving a migration run step-by-step).

**Tinyclaw's TimeoutEstimator**: Tramuntana's `/t_auto` loop runs subagent calls with no timeout prediction. Tinyclaw tracks historical call durations and estimates timeouts dynamically. This would prevent `/t_auto` from silently stalling when a subagent takes far longer than expected.

### What Not to Borrow

**Nanobot's bus abstraction**: Clean architecture, but Tramuntana is Telegram-only by design. The current queue-embedded-in-bot approach is fine for a single-frontend system. Abstracting it adds indirection without benefit unless a second frontend is planned.

**Gas Town's Refinery/Witness merge queue**: Uses LLM agents to resolve merge conflicts. Minuano's SQL-based merge queue (task dependencies + `task_context` handoffs) does the same work at zero token cost.

---

## 3. Session Memory

### Current State

**Minuano** stores memory in `task_context` rows, typed as:
- `observation` — facts discovered during task execution
- `result` — outputs produced
- `handoff` — context passed to dependent tasks
- `inherited` — context copied from parent task chain
- `test_failure` — captured test output for debugging continuity

Context is task-scoped and propagates through dependency chains via `inherited`. Full-text search uses a `tsvector` column on `content`. There is no project-level persistent memory that survives across all task chains.

**Tramuntana** tracks monitor state (byte offsets into JSONL transcripts), thread/window/project bindings, and per-user queue state. No LLM memory layer. No consolidation.

### Claws Approaches

**Nanobot/Picoclaw**: Two-layer memory:
1. `MEMORY.md` — long-term semantic facts, human-readable, LLM-maintained
2. `HISTORY.md` — timestamped chronological log, append-only JSONL for cache efficiency

When unconsolidated message count exceeds a threshold, the LLM consolidates HISTORY.md entries into MEMORY.md. Simple, effective, costs one consolidation call per session.

**OpenClaw**: Session archives + subagent follow-up tracking. Consolidation similar to nanobot's approach.

**Tinyclaw**: Four-layer compaction:
- L0: Rule-based pre-compression (remove boilerplate, truncate repeated patterns)
- L1: Shingle deduplication (near-duplicate detection across chunks)
- L2: LLM summarization of old chunks
- L3: Tiered summaries (summaries of summaries for very long sessions)

SQLite FTS5 for full-text search of session history.

### Potential Improvements

**1. Project-level MEMORY.md in Minuano**: `task_context` is ephemeral to a task chain. A persistent `project_memory.md` file (or a `project_memory` table) per project would give agents a place to write stable facts — "the staging DB is at host X", "always run migrations before tests" — that survive across task chain resets. Agents would read this on task claim via a `minuano-remember` command.

**2. Tinyclaw's four-layer compaction for Tramuntana JSONL**: When `monitor_state.json` byte offsets grow large, old transcript chunks accumulate. L2 summarization of aged chunks would let Tramuntana replay session context without re-reading gigabytes of JSONL. Low priority until transcripts actually become a problem.

**3. Extend Minuano's `SearchContext()` for cross-project search**: The `tsvector` index already exists on `task_context.content`. Removing the project filter and exposing results grouped by project would make it a system-wide knowledge search. This is a small SQL change with high value.

---

## 4. Code Search

### Current State

**Minuano**: `SearchContext()` in `queries.go` queries `task_context.content` using PostgreSQL `tsvector` within a project scope. Results include task ID, context type, and matching content.

**Tramuntana**: No code search UI. `/c_get` is a file browser (directory listing + file read). The history viewer paginates raw JSONL. There is no command that hits Minuano's search index.

### Claws Approaches

**Tinyclaw**: SQLite FTS5 indexes all session history. Agents and humans can query past sessions by keyword. Full-text ranked results.

**OpenClaw**: No dedicated code search exposed at the agent level. Agents find code by running shell tools themselves.

**Nanobot/Picoclaw**: Agents are given `ReadFile`, `ListDir`, and `ExecTool`. Code search happens by the agent running ripgrep via `ExecTool` — no framework-level search abstraction.

### Potential Improvements

**1. Expose `SearchContext` via Tramuntana**: Add `/p_search <query>` Telegram command. It calls `minuano search <project> <query>`, formats results as a Telegram message with task IDs and excerpts. Low implementation effort; the SQL is already written.

**2. Cross-session transcript search**: Tramuntana could index JSONL monitor output using a Go full-text library (e.g., bleve) so past agent sessions become searchable by topic. Useful when debugging a recurring issue — "what did the agent say last time about migration locks?"

**3. Semantic search layer**: None of the claws projects use vector embeddings. Adding `pgvector` to Minuano's Postgres would enable similarity search across `task_context` — "find tasks similar to this one", "find observations about connection pooling". This is a larger investment but would make Minuano's accumulated context genuinely queryable by meaning, not just keywords.

---

## 5. Agent Communication

### Current State

**Minuano**: Agents communicate indirectly via `task_context`. Observations written by Agent A become inherited context for Agent B on a dependent task. There is no direct messaging. `LISTEN/NOTIFY` is used only for approval events and crash notifications — not for agent-to-agent messages.

**Tramuntana**: Agents don't communicate with each other. The bot is a human ↔ Claude Code interface. Each Telegram topic is isolated.

### Claws Approaches

**Gas Town**: `gt mail check --inject` — explicit agent mailbox per agent identity. The Mayor broadcasts to all registered agents. Agents have a persistent inbox they poll on startup.

**Tinyclaw**: Blackboard pattern — a shared data structure that all agents can read and write. Enables collaborative coordination without direct messaging. `SubagentManager` tracks agent lifecycle (spawned, running, completed, failed).

**Nanobot/Picoclaw**: Subagent spawning via `SpawnTool` — a parent agent forks a child, waits for the result, continues. Linear parent-child hierarchy, no lateral communication.

**OpenClaw**: Multi-agent routing with session-to-session reply-back. Node registry for agent presence. Agents can address messages to other agents by session ID.

### Potential Improvements

**1. Agent mailbox via `agent_messages` table**: A simple Postgres table — `(id, sender, recipient, content, created_at, read_at)` — with a `minuano-mail` script for sending and reading. Already on the Minuano roadmap. Enables broadcast messages ("schema changed, re-read migration.sql") without requiring agents to poll a shared file. Zero ongoing tokens.

**2. Tinyclaw's Blackboard as `shared_context` table**: When multiple agents work on independent tasks in the same project, file collision is a real risk. A `shared_context` table scoped to project (not task) would let Agent A write "currently editing migration.sql" and Agent B read that before claiming a task that touches the same file. This fills the gap between isolated `task_context` (task-scoped) and the mailbox (message-based).

**3. Tramuntana topic-to-topic routing**: Each Telegram topic is currently isolated. A lightweight relay — configured via a Tramuntana command — that lets an agent in topic A post an observation to topic B's thread would enable human-visible inter-agent communication without DB polling. Useful for monitoring: "agent in topic A finished migrations, topic B's test run can start."

---

## 6. What to Borrow vs. Skip

| Concept | Source | Verdict | Rationale |
|---------|--------|---------|-----------|
| Per-user buffered channel queue | Tramuntana (own) | Keep as-is | Already solid; flood control is good |
| Sequential/parallel queue modes | OpenClaw | Consider | Useful for Tramuntana batch topic mode |
| TimeoutEstimator for subagents | Tinyclaw | Consider | `/t_auto` loop would benefit |
| Clean bus abstraction | Nanobot | Low priority | Tramuntana is Telegram-only by design |
| MEMORY.md + HISTORY.md | Nanobot | **Borrow** | Complement `task_context` for project-level persistent memory |
| Four-layer context compaction | Tinyclaw | Consider later | Overkill now; useful if JSONL transcripts grow large |
| tsvector code search | Minuano (own) | Extend | Already implemented; expose via Tramuntana `/p_search` |
| FTS5 session search | Tinyclaw | Consider | If transcript search becomes a genuine need |
| Blackboard shared scratchpad | Tinyclaw | **Borrow** | Fills the file collision gap in parallel agent workflows |
| Agent mailbox table | Gas Town-inspired | **Borrow** | Already on roadmap; low effort, high value |
| Refinery/Witness merge agents | Gas Town | **Skip** | SQL merge queue is strictly cheaper — zero tokens |
| Persistent agent identity | Gas Town | **Skip** | `task_context` already handles history without identity overhead |
| Role hierarchy | Gas Town | **Skip** | Zero-role model is a deliberate feature, not a gap |
| Mayor always-on coordinator | Gas Town | **Skip** | DB triggers replace this at zero ongoing cost |

---

## 7. Recommended Next Steps

Priority order for extending Minuano/Tramuntana or contributing to Maquinista:

### P0 — High value, low complexity

**Shared scratchpad (Blackboard)**
Add a `shared_context` table to Minuano: `(id, project_id, key, value, agent_id, updated_at)`. Agents write "currently editing X" on task claim and clear on task completion. A `minuano-lock` script wraps the read-check-write. Pure SQL, zero tokens, prevents file collision in parallel workflows.

**Agent mailbox**
Add `agent_messages` table: `(id, project_id, sender, recipient, content, created_at, read_at)`. Add `minuano-mail send <recipient> <message>` and `minuano-mail check` scripts. Broadcast support via `recipient = '*'`. Already on the Minuano roadmap.

### P1 — High value, moderate complexity

**Project-level MEMORY.md**
One `project_memory.md` file per Minuano project directory. Agents read it on task claim (prepended to system prompt or injected as `inherited` context). Agents write to it via `minuano-remember "<fact>"`. Survives task chain resets, accumulates stable project knowledge over time.

**Expose `SearchContext` in Tramuntana**
Add `/p_search <query>` command to Tramuntana. Calls `minuano search <project> <query>`, formats results as Telegram message with task IDs and content excerpts. Minuano's `tsvector` index already handles the heavy lifting.

### P2 — Moderate value, moderate complexity

**Sequential/parallel queue mode in Tramuntana**
Per-topic setting stored in Tramuntana's state. "Batch mode" queues messages strictly (no merging, strict ordering). "Stream mode" is the current behavior. Configurable via a Tramuntana command (e.g., `/t_queue_mode batch|stream`).

### P3 — Lower priority, higher complexity

**Transcript search**
Index Tramuntana's JSONL monitor output for cross-session search. Options: bleve (embedded Go FTS), or a `transcript_chunks` table in Postgres with `tsvector`. Enables searching past sessions by topic. Useful at scale; not urgent.

**Semantic search via pgvector**
Add `pgvector` extension to Minuano's Postgres. Embed `task_context` entries on write. Enable `/p_search --semantic <query>` in Tramuntana. High implementation cost; highest long-term value for a growing knowledge base.

---

*Generated: 2026-03-01*
*Systems: Minuano (PostgreSQL coordination) + Tramuntana (Telegram interface)*
*Comparisons: Gas Town/Beads, OpenClaw, Nanobot, Picoclaw, Tinyclaw*
