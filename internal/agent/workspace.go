package agent

import (
	"fmt"
	"path/filepath"
)

// WorkspaceScope controls how an agent's filesystem is isolated at
// spawn time. See plans/active/workspace-scopes.md for the design.
type WorkspaceScope string

const (
	// ScopeShared: agent runs in the configured cwd directly. No
	// worktree, no branch. Matches the legacy Spawn() behavior.
	ScopeShared WorkspaceScope = "shared"

	// ScopeAgent: one worktree per (project, agent), persistent across
	// restarts. For long-lived user agents that share a project but
	// must not step on each other's edits.
	ScopeAgent WorkspaceScope = "agent"

	// ScopeTask: one worktree per (project, task), typically ephemeral.
	// Matches /t_pickw and orchestrator behavior.
	ScopeTask WorkspaceScope = "task"
)

// Validate returns nil if s is a known scope, otherwise an error.
func (s WorkspaceScope) Validate() error {
	switch s {
	case ScopeShared, ScopeAgent, ScopeTask:
		return nil
	case "":
		return fmt.Errorf("workspace scope: empty")
	default:
		return fmt.Errorf("workspace scope: unknown value %q", string(s))
	}
}

// Layout is the resolved filesystem plan for a spawn. Worktree-based
// backends populate WorktreeDir + Branch; ScopeShared leaves both
// empty and RepoRoot holds the directory the tmux window opens in.
type Layout struct {
	Scope       WorkspaceScope
	RepoRoot    string // always set
	WorktreeDir string // empty for ScopeShared
	Branch      string // empty for ScopeShared
}

// WindowCWD returns the directory the agent's tmux window should open
// in. WorktreeDir takes precedence when set; otherwise RepoRoot.
func (l Layout) WindowCWD() string {
	if l.WorktreeDir != "" {
		return l.WorktreeDir
	}
	return l.RepoRoot
}

// ResolveLayout returns the Layout for a given scope + identity.
//
// repoRoot must be the project's git root for ScopeAgent / ScopeTask;
// for ScopeShared it's the cwd the window should open in. agentID is
// required for ScopeAgent; taskID for ScopeTask.
//
// Path + branch conventions:
//
//	shared → window cwd = repoRoot, no worktree
//	agent  → <repoRoot>/.maquinista/worktrees/agent/<id> on maquinista/agent/<id>
//	task   → <repoRoot>/.maquinista/worktrees/task/<id>  on maquinista/task/<id>
func ResolveLayout(scope WorkspaceScope, repoRoot, agentID, taskID string) (Layout, error) {
	if err := scope.Validate(); err != nil {
		return Layout{}, err
	}
	if repoRoot == "" {
		return Layout{}, fmt.Errorf("workspace layout: repoRoot is required")
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return Layout{}, fmt.Errorf("workspace layout: abs(%q): %w", repoRoot, err)
	}

	switch scope {
	case ScopeShared:
		return Layout{Scope: ScopeShared, RepoRoot: absRoot}, nil

	case ScopeAgent:
		if agentID == "" {
			return Layout{}, fmt.Errorf("workspace layout: agentID is required for scope %q", scope)
		}
		return Layout{
			Scope:       ScopeAgent,
			RepoRoot:    absRoot,
			WorktreeDir: filepath.Join(absRoot, ".maquinista", "worktrees", "agent", agentID),
			Branch:      "maquinista/agent/" + agentID,
		}, nil

	case ScopeTask:
		if taskID == "" {
			return Layout{}, fmt.Errorf("workspace layout: taskID is required for scope %q", scope)
		}
		return Layout{
			Scope:       ScopeTask,
			RepoRoot:    absRoot,
			WorktreeDir: filepath.Join(absRoot, ".maquinista", "worktrees", "task", taskID),
			Branch:      "maquinista/task/" + taskID,
		}, nil
	}

	return Layout{}, fmt.Errorf("workspace layout: unhandled scope %q", scope)
}
