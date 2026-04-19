## Context

Today maquinista has three divergent spawn paths that each hard-code
one assumption about filesystem isolation:

- `internal/agent/agent.go:32` `Spawn` — no isolation, tmux window
  at `.` (whatever the daemon's cwd was). Used by the reconcile loop
  for persistent user agents (`multi-agent-registry.md` Phase 1) and
  by the topic-agent spawner (`cmd/maquinista/spawn_topic_agent.go`).
- `internal/agent/agent.go:73` `SpawnWithWorktree` — creates
  `.maquinista/worktrees/<agentID>` on branch `maquinista/<agentID>`.
  Only reached via `maquinista spawn/run --worktrees`. Derives the
  repo root from **`git.RepoRoot(".")`** — the daemon's cwd, not the
  agent's — so if an operator launches `./maquinista` from anywhere
  other than the project they want worktreed, the worktree is cut
  against the wrong repo.
- `internal/bot/worktree_commands.go:32` `executePickwTask` — creates
  `.minuano/worktrees/<project>-<taskID>` on branch
  `minuano/<project>-<taskID>`, keyed by `thread_id` in
  `window_worktrees` (migration 021). Different path, different
  branch prefix, different base-repo resolution
  (`getRepoRoot` reads the window's CWD from state, not process cwd).

Plus one external consumer: `internal/orchestrator/ensure_agent.go:64`
reads `tasks.worktree_path` directly and refuses to spawn if empty —
whoever *creates* the worktree is upstream of this code.

Result: "two persistent user agents on the same project" is **not
isolated at all** (both go through `Spawn`, both share whatever cwd
the daemon was launched from), while "two task agents on the same
project" *is* isolated but through two incompatible code paths. No
active plan closes this gap — `checkpoint-rollback.md` Phase 1
assumes the worktree already exists; `multi-agent-registry.md` Phase 1
explicitly doesn't touch isolation.

## Scope

Seven phases. Phases 1 + 2 are the minimum viable fix (abstraction +
bug fix). Phase 3 unblocks persistent-agent isolation behind schema
columns. Phase 4 migrates CLI call sites. Phase 5 is lifecycle
cleanup. Phase 6 promotes workspaces to first-class child rows (so
an agent can hold N workspaces and switch between them). Phase 7
surfaces workspaces on the dashboard and Telegram.

The goal is **one** spawn API parameterized by a `WorkspaceScope`,
with `git worktree` as the only backend in this plan. Other backends
(Docker container, Turso AgentFS CoW, Cloudflare-style snapshots)
stay out of scope — they slot into the same API later.

### Phase 1 — `WorkspaceScope` + `Layout` in `internal/agent/`

New file `internal/agent/workspace.go`:

```go
type WorkspaceScope string

const (
    // ScopeShared: agent runs in the configured cwd directly.
    // No worktree, no branch. Matches today's Spawn() behavior.
    ScopeShared WorkspaceScope = "shared"

    // ScopeAgent: one worktree per (project, agent), persistent
    // across restarts. For long-lived user agents that share a
    // project but must not step on each other's edits.
    ScopeAgent  WorkspaceScope = "agent"

    // ScopeTask: one worktree per (project, task), ephemeral.
    // Matches /t_pickw and orchestrator behavior.
    ScopeTask   WorkspaceScope = "task"
)

// Layout is the resolved filesystem plan for a spawn. Worktree
// backends populate WorktreeDir + Branch; the Shared scope leaves
// both empty and RepoRoot holds the directory the tmux window opens
// in.
type Layout struct {
    Scope       WorkspaceScope
    RepoRoot    string  // always set — the project's git root OR the cwd for Shared
    WorktreeDir string  // empty for Shared
    Branch      string  // empty for Shared
}
```

Resolver:

```go
// ResolveLayout returns the Layout for a given scope + identity.
// repoRoot must be the project's git root for ScopeAgent / ScopeTask;
// for ScopeShared it's the cwd the window should open in.
// agentID is required for ScopeAgent; taskID for ScopeTask.
func ResolveLayout(scope WorkspaceScope, repoRoot, agentID, taskID string) (Layout, error) {
    ...
}
```

Path + branch conventions (stable; callers don't compute them):

| Scope  | Worktree dir                                      | Branch                    |
|--------|---------------------------------------------------|---------------------------|
| shared | — (cwd = repoRoot)                                | —                         |
| agent  | `<repoRoot>/.maquinista/worktrees/agent/<id>`     | `maquinista/agent/<id>`   |
| task   | `<repoRoot>/.maquinista/worktrees/task/<id>`      | `maquinista/task/<id>`    |

Tests: one per scope including error cases (empty id for agent/task,
empty repoRoot, etc.).

### Phase 2 — `SpawnWithLayout`, drop process-cwd dependency

New single entry point in `internal/agent/agent.go`:

```go
func SpawnWithLayout(
    pool *pgxpool.Pool,
    tmuxSession, agentID, claudeMDPath string,
    env map[string]string,
    r runner.AgentRunner,
    role string,
    layout Layout,
) (*Agent, error)
```

Behavior:

1. If `layout.WorktreeDir != ""` and it doesn't exist, call
   `git.WorktreeAdd(layout.RepoRoot, layout.WorktreeDir, layout.Branch)`.
2. If `layout.WorktreeDir != ""` and it does exist (e.g. persistent
   agent restarting), skip the add — reuse in place. Validate the
   directory is actually a worktree of `layout.RepoRoot` via
   `git.RepoRoot(layout.WorktreeDir)`.
3. `RegisterAgent` with `worktree_dir = layout.WorktreeDir or nil`,
   `branch = layout.Branch or nil`.
4. `tmux.NewWindowWithDir(tmuxSession, agentID, windowCWD, env)` —
   `windowCWD` is `layout.WorktreeDir` when set, else `layout.RepoRoot`.
5. Send bootstrap with `WORKTREE_DIR` / `BRANCH` env vars only when
   the scope has them.

Existing `Spawn` and `SpawnWithWorktree` become three-line wrappers:

```go
func Spawn(...) (*Agent, error) {
    cwd, _ := filepath.Abs(".")
    return SpawnWithLayout(..., Layout{Scope: ScopeShared, RepoRoot: cwd})
}

func SpawnWithWorktree(...) (*Agent, error) {
    // NOTE: legacy path. Still calls git.RepoRoot(".") for repo
    // resolution; new callers should use SpawnWithLayout directly.
    repoRoot, err := git.RepoRoot(".")
    if err != nil { return nil, err }
    layout, err := ResolveLayout(ScopeAgent, repoRoot, agentID, "")
    if err != nil { return nil, err }
    return SpawnWithLayout(..., layout)
}
```

This keeps the existing CLI flags green while all new code goes
through `SpawnWithLayout`.

### Phase 3 — Schema + reconcile loop scope awareness

Migration `027_agents_workspace_scope.sql`:

```sql
ALTER TABLE agents
  ADD COLUMN IF NOT EXISTS workspace_scope TEXT NOT NULL DEFAULT 'shared',
  ADD COLUMN IF NOT EXISTS workspace_repo_root TEXT;
-- workspace_scope: 'shared' | 'agent' | 'task'
-- workspace_repo_root: the git repo the agent's worktree (if any) is
-- cut from. Stable across restarts so the reconcile loop doesn't
-- have to re-derive it from cwd.
```

Reconcile loop changes (`cmd/maquinista/cmd_start_reconcile.go` —
created by `multi-agent-registry.md` Phase 1, to be extended here):

For each persistent user agent row:

1. Read `workspace_scope`, `workspace_repo_root`, `cwd`.
2. If `workspace_repo_root` is empty, fall back to `cwd`, then to the
   process cwd (legacy). Log a one-time deprecation warning.
3. `layout, err := agent.ResolveLayout(scope, repoRoot, agentID, "")`.
4. `agent.SpawnWithLayout(..., layout)`.

On agent edit (`maquinista agent edit <id> --scope agent`), flipping
from `shared` to `agent` does **not** move files — it just changes
the intent. The next reconcile creates the worktree. Flipping in the
other direction leaves the worktree on disk; operator removes it by
hand if they want.

CLI:

- `maquinista agent add <id> --scope {shared,agent,task} --repo <path>`
- `maquinista agent edit <id> --scope … --repo …`

`--repo` defaults to the agent's `cwd` when it's a git repo.

### Phase 4 — Migrate existing call sites

1. `cmd_spawn.go` / `cmd_run.go` — replace `--worktrees` boolean with
   `--scope {shared,agent}` (default `shared` to match today's
   behavior). Keep `--worktrees` as a deprecated alias for
   `--scope agent` with a one-line stderr warning. Repo root comes
   from `--repo` flag or the process cwd (explicit, not magical).
2. `internal/bot/worktree_commands.go` — `/t_pickw` continues to use
   its own `window_worktrees` table (thread-keyed, different scope
   semantics) but now calls `ResolveLayout(ScopeTask, repoRoot,
   "", taskID)` so the path + branch naming matches the rest of the
   tree. Old `.minuano/` paths stay reachable (already-created
   worktrees keep working) but new `/t_pickw` uses
   `.maquinista/worktrees/task/<id>`.
3. `internal/orchestrator/ensure_agent.go` — untouched. The
   orchestrator's `tasks.worktree_path` contract predates this plan;
   whoever populates that field should use `ResolveLayout` but the
   consumer code doesn't need to change.

### Phase 6 — Workspaces as children of agents

Phases 1–5 treat `workspace_scope` + `workspace_repo_root` as
immutable-per-agent columns. Real usage is multi-project: alice
works on project A today, project B tomorrow, and expects both
worktrees to survive. Promote workspaces to first-class rows:

```sql
CREATE TABLE agent_workspaces (
    id            TEXT PRIMARY KEY,          -- e.g. "alice@project-a"
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    scope         TEXT NOT NULL
        CHECK (scope IN ('shared', 'agent', 'task')),
    repo_root     TEXT NOT NULL,
    worktree_dir  TEXT,                       -- NULL for scope='shared'
    branch        TEXT,                       -- NULL for scope='shared'
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    archived_at   TIMESTAMPTZ
);

ALTER TABLE agents
    ADD COLUMN active_workspace_id TEXT REFERENCES agent_workspaces(id);
```

`workspace_scope` + `workspace_repo_root` on `agents` stay as
denormalized cache columns (avoid joining on every reconcile pass);
a BEFORE-UPDATE trigger on `agents.active_workspace_id` keeps them
synced with the active workspace row.

Migration `028_agent_workspaces.sql` also **backfills**: for every
existing agent row, insert a workspace `<agent_id>@default` derived
from the row's current `workspace_scope` + `workspace_repo_root` (or
`cwd` for the scope=shared default), then set
`active_workspace_id = '<agent_id>@default'`. Post-migration invariant:
every non-archived agent has at least one workspace.

Operations:

- **Create** — `INSERT INTO agent_workspaces` + `ResolveLayout` for
  `worktree_dir` + `branch`. Does not automatically activate.
- **Activate** — `UPDATE agents SET active_workspace_id = $ws`. Next
  reconcile pass opens the tmux window at the new workspace's
  `WindowCWD`. Worktree is created on demand if missing; if present,
  reused in place.
- **Archive** — `UPDATE agent_workspaces SET archived_at = NOW()`.
  Soft-delete only; the worktree on disk is **not** removed by
  default (operator cleans up if they want). Blocked when the
  workspace is currently active.
- **List** — `SELECT … FROM agent_workspaces WHERE agent_id = $1
  AND archived_at IS NULL ORDER BY created_at`.

Blocker decisions encoded here:

- **#1 merge flow** — agents merge directly via `git merge` in the
  worktree OR create a PR via `gh`. No `maquinista agent merge`
  subsystem; `scope=agent` branches are named `maquinista/agent/<id>`
  and push cleanly to any remote.
- **#2 operator-deleted worktree** — reconcile recreates the
  worktree only when `stop_requested=FALSE` AND `status NOT IN
  ('archived','dead')`. If the operator nukes the dir AND parks the
  row, reconcile leaves it alone.
- **#3 agent-moves-projects** — not a direct column update anymore;
  create a new workspace + activate. Old workspace rows survive as
  history. Matches Hermes' "identity immutable, workspace rotates"
  model but with human-chosen labels instead of timestamps.
- **#4 default scope** — `scope=shared` stays the default for the
  auto-spawned agent; opting into `scope=agent` requires `--scope
  agent --repo <path>` at `agent add` time.

### Phase 7 — Dashboard + Telegram surfaces

**Dashboard** (Next.js, `internal/dashboard/web/`):

- `GET /api/agents/[id]/workspaces` — list active workspaces.
- `POST /api/agents/[id]/workspaces` — create + activate a new
  workspace from `{id, scope, repo_root}`.
- `PATCH /api/agents/[id]/workspaces/[wsId]` — activate an existing
  workspace.
- `DELETE /api/agents/[id]/workspaces/[wsId]` — archive.
- New Workspaces card in the agent detail tabs: workspace list with
  active marker (`★`), one-click **Switch**, **Archive** (disabled
  when active), and a "New workspace" form.

**Telegram** (`internal/bot/`):

- `/ws` — list workspaces for the topic's bound agent, mark the
  active one.
- `/ws new <label> <repo_root>` — create + activate.
- `/ws switch <label>` — activate.
- `/ws archive <label>` — soft-delete (refuses if active).
- Fails closed when the topic isn't bound to an agent (`/agent_default`
  first).

Both surfaces share the Go helpers in `internal/agent/workspace.go`
and a new `internal/db/queries_workspaces.go` (CRUD). The Next.js
API calls these via a narrow HTTP → DB layer, not via running
`maquinista` binaries.

### Phase 5 — Lifecycle parity

Today `agent.Kill` removes the worktree if the branch is merged and
preserves it if not (`agent.go:222-235`). That's correct for
`ScopeTask`, wrong for `ScopeAgent`:

- `ScopeShared` → nothing to remove.
- `ScopeAgent` → **preserve** on kill. Persistent agents restart into
  the same worktree; removing it on every `stop` defeats the point.
  Instead, mark `stop_requested=TRUE` and leave files.
- `ScopeTask` → today's behavior: remove if merged, preserve if not.

Refactor `agent.Kill` to branch on `workspace_scope`.

## Schema changes

Single migration `027_agents_workspace_scope.sql` (shown in Phase 3).
No new tables.

## Files to modify

### Phase 1

- `internal/agent/workspace.go` (new) — `WorkspaceScope`, `Layout`,
  `ResolveLayout`.
- `internal/agent/workspace_test.go` (new) — one case per scope plus
  error cases.

### Phase 2

- `internal/agent/agent.go` — add `SpawnWithLayout`, reduce `Spawn`
  and `SpawnWithWorktree` to wrappers. Do not delete either; the
  call sites migrate in Phase 4.
- `internal/agent/agent_test.go` (new or existing) — integration
  test under `//go:build integration` that actually creates a
  worktree and confirms the tmux window opens in it.

### Phase 3

- `internal/db/migrations/027_agents_workspace_scope.sql` (new).
- `internal/db/queries.go` — extend `RegisterAgent` / `GetAgent` /
  `ListAgents` to read + write the two new columns. Add
  `UpdateAgentScope(agentID, scope, repoRoot)`.
- `cmd/maquinista/cmd_start_reconcile.go` — scope-aware spawn (exact
  location depends on how `multi-agent-registry.md` Phase 1 lands;
  if it hasn't, the relevant code lives in
  `cmd/maquinista/cmd_start.go:ensureDefaultAgent`).
- `cmd/maquinista/cmd_agent.go` — `--scope` / `--repo` flags on
  `agent add` / `agent edit`.

### Phase 4

- `cmd/maquinista/cmd_spawn.go`, `cmd/maquinista/cmd_run.go` —
  `--scope` flag; `--worktrees` deprecation alias.
- `internal/bot/worktree_commands.go` — call `ResolveLayout` for the
  task scope; keep `window_worktrees` writes as-is.

### Phase 5

- `internal/agent/agent.go` `Kill` — scope-aware cleanup.

## Interaction with other active plans

- **`multi-agent-registry.md`** — Phase 3 here depends on its Phase 1
  (the reconcile loop needs to exist first). If `multi-agent-registry`
  Phase 1 is unshipped when this plan starts, do Phase 3 against the
  current `ensureDefaultAgent` and update the touch point when
  reconcile lands.
- **`checkpoint-rollback.md`** — already builds on the agent's
  worktree. Its Phase 1 per-tool commit logic works unchanged against
  `ScopeAgent` worktrees. Phase 4 "branches" becomes cleaner when
  this plan is in place: "fork agent" = new `ScopeAgent` row pointing
  at the same `workspace_repo_root`.
- **`per-agent-sidecar.md`** — orthogonal. The sidecar consumes the
  `Layout` via the existing `agents.worktree_dir` column; no change
  needed.
- **`json-state-migration.md`** — Phase B3 already moved
  `WorktreeBindings` to `window_worktrees`; this plan leaves that
  table alone (it's thread-scoped, not agent-scoped).

## Verification per phase

- **Phase 1** — `go test ./internal/agent/` passes with new cases:
  `TestResolveLayout_Shared`, `TestResolveLayout_Agent`,
  `TestResolveLayout_Task`, `TestResolveLayout_MissingID`.
- **Phase 2** — `go test ./internal/agent/` still green;
  `maquinista spawn --worktrees foo` still produces the same
  worktree dir and branch as before the refactor (smoke test
  against the pre-refactor output in a scratch repo).
- **Phase 3** — `INSERT INTO agents (..., workspace_scope='agent',
  workspace_repo_root='/path/to/project')` + `./maquinista start` →
  tmux pane opens in `<project>/.maquinista/worktrees/agent/<id>`;
  `git -C <pane cwd> rev-parse --abbrev-ref HEAD` prints
  `maquinista/agent/<id>`. Restart daemon: same pane respawns in the
  same dir.
- **Phase 4** — `maquinista spawn foo --scope agent --repo ~/code/proj`
  works; `maquinista spawn foo --worktrees` still works with a
  deprecation warning.
- **Phase 5** — `maquinista kill <scope=agent>` leaves the worktree
  on disk; `maquinista kill <scope=task>` removes it if merged.

## Open questions

1. **Bootstrap default for the default agent.** Today the
   auto-spawned `MAQUINISTA_DEFAULT_AGENT` lands at `$HOME` with
   `scope=shared`. Should `./maquinista start` from inside a git repo
   default the agent to `scope=agent` with `repo=$PWD`? It's a
   sensible DWIM but would change existing behavior for users who
   run the daemon from their own `~/code/maquinista` dir.
2. **Multiple projects per daemon.** `workspace_repo_root` is per
   agent — two agents on different projects are fine. What's missing
   is a first-class `projects` table (repo_root + default scope +
   merge strategy). Today `topic_projects.project_id` is a bare
   string with no join. Defer to its own plan if the pain becomes
   real; `workspace_repo_root` is the escape hatch until then.
3. **`/t_pickw` branch-prefix migration.** Phase 4 switches the new
   `/t_pickw` worktrees to `.maquinista/worktrees/task/` +
   `maquinista/task/` but leaves older `.minuano/` worktrees alone.
   If this is ugly, add a one-shot `maquinista worktrees migrate`
   CLI to rename them. Not worth it until a user complains.
4. **Non-git backends.** Openclaw (Docker per scope) and Cloudflare
   (snapshot+fork) map onto the same `Layout` shape — `WorktreeDir`
   becomes "isolated directory path" and `Branch` becomes "snapshot
   id". Out of scope for now; noting so the type names don't lock us
   to git semantics forever.
