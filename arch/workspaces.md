# Workspaces

## What a workspace is

A workspace is the filesystem context an agent operates in. Every agent
has exactly one active workspace at a time (`agents.active_workspace_id`).
The workspace determines the `cwd` passed to the tmux window.

## Scopes

Three scopes exist:

| Scope | Worktree | Use case |
|-------|----------|----------|
| `shared` | None — uses repo root | Single-agent or read-heavy tasks |
| `agent` | `<repo>/.worktrees/<agent-id>/` | Parallel agents on same repo without conflicts |
| `task` | `<repo>/.worktrees/<task-id>/` | Orchestrator-owned subtask agents |

`scope=shared` is the default for dashboard-spawned agents. Scope is set
at agent creation time and stored in `agents.workspace_scope`.

## Worktree lifecycle

For `scope=agent` or `scope=task`, `agent.ResolveLayout` computes the
worktree path and branch name. The branch is deterministic:
`agent/<agent-id>` or `task/<task-id>`.

Worktree creation is lazy: `reconcileAgentPanes` / `respawnAgent` calls
`git.WorktreeAdd` only if the directory does not yet exist. On restart
it reuses the existing worktree — in-flight work survives daemon
restarts.

## agent_workspaces table

Agents can hold multiple workspaces (e.g. one per feature branch) and
switch between them. `agents.active_workspace_id` points to the current
one; switching is a single UPDATE that a trigger mirrors into the
denormalized cache columns (`workspace_scope`, `workspace_repo_root`).

```
maquinista agent ws new <agent-id> <label> --scope=agent --repo=<path>
maquinista agent ws switch <agent-id> <label>
maquinista agent ws list <agent-id>
maquinista agent ws archive <agent-id> <label>
```

## Reconcile and workspace resolution

`respawnAgent` calls `ResolveLayout(scope, repoRoot, agentID, "")` to get
the window CWD. For `scope=shared` this is a no-op; for other scopes it
returns `WorktreeDir` (the git worktree path).

Legacy rows without `workspace_repo_root` fall back to inferring the repo
from `agents.cwd` via `git.RepoRoot`.

## TODO

- [ ] Full workspace-scopes.md plan implementation status
- [ ] Task-scoped workspace cleanup on task completion
- [ ] Worktree conflict detection (two agents, same branch)
- [ ] Dashboard workspace switcher UI
