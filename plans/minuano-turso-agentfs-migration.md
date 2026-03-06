# Minuano: PostgreSQL to Turso + AgentFS Migration

## Context

Minuano is a Go task coordination tool using PostgreSQL for task state management with `SELECT FOR UPDATE SKIP LOCKED` for atomic task claiming, and git worktrees for agent file isolation. The goal is to evaluate porting to Turso (libSQL/SQLite) for the database layer and AgentFS for file isolation, removing the PostgreSQL dependency.

## What Turso Offers

| Feature | Description | Useful for Minuano? |
|---------|-------------|---------------------|
| **MVCC (BEGIN CONCURRENT)** | Optimistic concurrent writes, conflict at commit | No - overkill for minuano's scale |
| **AgentFS** | CoW overlay filesystem per session, backed by SQLite | Yes - replaces git worktrees |
| **Database Branching** | CoW database snapshots | No - minuano needs shared state |
| **Memelord** | Persistent agent memory with weighted reinforcement learning, vector search via libSQL | Yes - could enhance task_context |

## Key Challenges

### 1. Atomic Task Claiming (Hardest Part)

**Current**: `SELECT FOR UPDATE SKIP LOCKED` in `AtomicClaim()` and `ClaimMergeEntry()` (`internal/db/queries.go`)

**Solution**: `BEGIN IMMEDIATE` + CAS pattern. Acquires database-level write lock (only one writer at a time). At minuano's scale (1-10 agents, single machine), the lock duration is microseconds - no bottleneck. Add a retry wrapper for `SQLITE_BUSY`.

This is simpler and more correct than BEGIN CONCURRENT for this use case. BEGIN CONCURRENT (optimistic) adds retry complexity without benefit at this scale.

### 2. SQL Syntax Changes (47 query functions in queries.go)

| PostgreSQL | SQLite/libSQL |
|------------|---------------|
| `BIGSERIAL` | `INTEGER PRIMARY KEY AUTOINCREMENT` |
| `TIMESTAMPTZ` | `TEXT` (ISO8601) |
| `JSONB` | `TEXT` + `json_extract()` |
| `TEXT[]` | `TEXT` (JSON array) |
| `$1, $2` params | `?` positional |
| `NOW()` | `datetime('now')` |
| `gen_random_uuid()` | `uuid.New()` in Go |
| `make_interval(mins=>$1)` | `datetime('now', '-' \|\| ? \|\| ' minutes')` |
| `pgxpool.Pool` | `*sql.DB` |
| `pgx.ErrNoRows` | `sql.ErrNoRows` |

### 3. PL/pgSQL Triggers

**`refresh_ready_tasks()`**: Port to SQLite `CREATE TRIGGER` on `AFTER UPDATE OF status ON tasks WHEN NEW.status = 'done'`. SQLite triggers are simpler but capable enough.

### 4. pg_notify (used by tramuntana)

**Solution**: Event table + polling. Go code inserts event rows after status transitions. Tramuntana polls `events` table with `id > last_seen_id`. The TUI already polls at 2s intervals.

### 5. Full-Text Search

**Current**: GIN index + `to_tsvector`/`plainto_tsquery`
**Solution**: FTS5 virtual table with sync triggers on `task_context`.

### 6. Bash Scripts (minuano-claim, minuano-done, etc.)

Currently use `psql` directly. **Replace with Go subcommands** (`minuano claim`, `minuano done`, etc.) to eliminate CLI dependency entirely.

## AgentFS Integration

**Replaces**: git worktrees for file isolation
**How**: Each agent gets a CoW overlay filesystem session via AgentFS (FUSE on Linux). Writes go to a SQLite-backed delta layer, base filesystem stays untouched.

**Hybrid approach**: Use AgentFS during work, but at task completion extract changed files, commit to git, and use existing merge queue. Preserves git history.

**Coexistence**: Support both `--worktrees` (current) and `--agentfs` (new) flags. AgentFS is opt-in initially.

**Key change**: `SpawnWithWorktree()` in `internal/agent/agent.go` gets a sibling `SpawnWithAgentFS()`.

## Memelord-Inspired Agent Memory

[Memelord](https://github.com/glommer/memelord) is Glommer's persistent memory system for coding agents, built on Turso/libSQL with native vector search (`vector32` column type + `vector_distance_cos()`). Key patterns applicable to minuano:

- **Weighted memories with reinforcement**: Memories gain weight when useful (rated by agent), decay over time when unused, get deleted when contradicted. Currently `task_context` treats all entries equally.
- **Semantic retrieval**: Vector embeddings enable "find context similar to this task" rather than just keyword FTS. Could improve inherited context quality when agents claim tasks.
- **Time decay**: Old context entries naturally lose relevance. A weight/decay system would surface fresh, high-value context first.

**Potential integration**: Add `weight` and `embedding` columns to `task_context`. Use libSQL's `vector32` type for semantic search when inheriting context from dependencies. Agents rate context usefulness via `minuano observe --useful` / `--not-useful`. This is a future enhancement, not part of the core migration.

## Database Branching

**Skip it.** Minuano's coordination database is inherently shared state - branching would break coordination. Only useful for testing (branch DB, run tests, discard).

## Phased Implementation

### Phase 0: Store Interface (prep, no behavior change)
Create `Store` interface in `internal/db/store.go` wrapping all 47 query functions. Current pgx code becomes `pgStore`. All commands switch to the interface. This decouples the migration from command-level changes.

### Phase 1: SQLite Schema + Query Port
- Write unified SQLite migration
- Implement `sqliteStore` (port all 47 functions)
- Helpers: `SQLiteTime` for timestamps, `JSONStringArray` for arrays, BUSY retry wrapper
- AtomicClaim/ClaimMergeEntry with BEGIN IMMEDIATE + CAS
- FTS5 for SearchContext
- SQLite cascade trigger
- Use `go-libsql` driver (Turso cloud from the start, enables embedded replicas, vector search)

### Phase 2: Script Replacement
Convert bash scripts to Go subcommands. Update agent bootstrap in `agent.go`.

### Phase 3: Notification System
Event table + Go-side emission + polling method for consumers. Update tramuntana.

### Phase 4: Remove PostgreSQL
Remove docker-compose, pgx dependency, `minuano up/down` commands. Update docs. `.env` changes from `DATABASE_URL=postgres://...` to `MINUANO_DB=./minuano.db`.

### Phase 5: AgentFS (parallel/optional)
Add `internal/agentfs/` package, `SpawnWithAgentFS`, `--agentfs` flag. Keep worktrees as default.

### Phase 6: Data Migration Tool
One-time `minuano migrate-from-pg` command for existing deployments.

## Risks

| Risk | Severity | Mitigation |
|------|----------|------------|
| BEGIN IMMEDIATE serialization | Low (microsecond writes, <10 agents) | Monitor; switch to BEGIN CONCURRENT if needed |
| AgentFS FUSE stability | Medium | Keep git worktrees as fallback |
| FTS5 vs tsvector differences | Low | Test with real data |
| Driver maturity (go-libsql) | Medium | go-libsql is Turso's official driver; fallback to modernc.org/sqlite if issues |
| Timestamp handling | Low | Consistent ISO8601 format |

## Critical Files

- `internal/db/queries.go` - All 47 query functions (largest change)
- `internal/db/db.go` - Connection layer + migrations
- `internal/db/migrations/001_initial.sql` - Schema reference
- `internal/agent/agent.go` - Worktree/AgentFS spawn logic
- `cmd/minuano/main.go` - Global pool variable
- `scripts/minuano-{claim,done,observe,handoff,pick}` - To be replaced
- `internal/git/git.go` - Worktree operations (kept for --worktrees mode)

## Verification

1. Port schema, run all existing tests against SQLite
2. Test AtomicClaim with concurrent agents (spawn 5 agents, verify no double-claims)
3. Test merge queue claiming under contention
4. Test FTS5 search against real task_context data
5. Test cascade trigger (mark task done, verify dependents become ready)
6. End-to-end: `minuano spawn` -> agent claims -> works -> `minuano done` -> merge
7. AgentFS: spawn agent with `--agentfs`, verify file isolation, verify changes extractable
