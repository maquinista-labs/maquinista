package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// Agent represents a running agent instance.
type Agent struct {
	ID          string
	TmuxSession string
	TmuxWindow  string
	TaskID      *string
	Status      string
	StartedAt   time.Time
	LastSeen    *time.Time
	WorktreeDir *string
	Branch      *string
}

// Spawn registers an agent in the DB, creates a tmux window, and sends
// the bootstrap command. role should be "executor" (default) or "planner".
//
// Thin wrapper around SpawnWithLayout: ScopeShared, repoRoot = abs(".").
// Prefer SpawnWithLayout for new callers so the repo root is explicit.
func Spawn(pool *pgxpool.Pool, tmuxSession, agentID, claudeMDPath string, env map[string]string, r runner.AgentRunner, role string) (*Agent, error) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		return nil, fmt.Errorf("resolving cwd: %w", err)
	}
	layout, err := ResolveLayout(ScopeShared, cwd, agentID, "")
	if err != nil {
		return nil, err
	}
	return SpawnWithLayout(pool, tmuxSession, agentID, claudeMDPath, env, r, role, layout)
}

// SpawnWithWorktree registers an agent with an isolated git worktree.
// role should be "executor" (default) or "planner".
//
// Legacy wrapper around SpawnWithLayout. Resolves the repo root from
// the process cwd via git.RepoRoot(".") — callers that can't guarantee
// the binary was launched from inside the target repo should use
// SpawnWithLayout + ResolveLayout(ScopeAgent, ...) directly.
func SpawnWithWorktree(pool *pgxpool.Pool, tmuxSession, agentID, claudeMDPath string, env map[string]string, r runner.AgentRunner, role string) (*Agent, error) {
	repoRoot, err := git.RepoRoot(".")
	if err != nil {
		return nil, fmt.Errorf("finding repo root: %w", err)
	}
	layout, err := ResolveLayout(ScopeAgent, repoRoot, agentID, "")
	if err != nil {
		return nil, err
	}
	return SpawnWithLayout(pool, tmuxSession, agentID, claudeMDPath, env, r, role, layout)
}

// SpawnWithLayout is the unified spawn entry point parameterized by a
// resolved workspace Layout. For ScopeShared it behaves like the old
// Spawn(); for ScopeAgent / ScopeTask it creates (or reuses) the
// worktree at layout.WorktreeDir on layout.Branch.
//
// Reuse semantics: if layout.WorktreeDir already exists and is a git
// worktree, the existing directory is reused in place (no new branch,
// no WorktreeAdd). Intended for persistent ScopeAgent panes that
// restart across daemon restarts.
func SpawnWithLayout(
	pool *pgxpool.Pool,
	tmuxSession, agentID, claudeMDPath string,
	env map[string]string,
	r runner.AgentRunner,
	role string,
	layout Layout,
) (*Agent, error) {
	if err := layout.Scope.Validate(); err != nil {
		return nil, err
	}
	if layout.RepoRoot == "" {
		return nil, fmt.Errorf("SpawnWithLayout: layout.RepoRoot is empty")
	}

	runnerName := "claude"
	if r != nil {
		runnerName = r.Name()
	}
	if role == "" {
		role = "executor"
	}

	// Create the worktree for scoped layouts. Reuse if the directory
	// already exists (restart case).
	createdWorktree := false
	if layout.WorktreeDir != "" {
		switch existing, err := os.Stat(layout.WorktreeDir); {
		case err == nil && existing.IsDir():
			// Reuse in place. Sanity-check that the dir actually belongs
			// to the expected repo — a stale directory from a different
			// project would silently bind the agent to the wrong code.
			if actualRoot, rerr := git.RepoRoot(layout.WorktreeDir); rerr != nil {
				return nil, fmt.Errorf("reusing worktree %s: %w", layout.WorktreeDir, rerr)
			} else if filepath.Clean(actualRoot) != filepath.Clean(layout.WorktreeDir) && filepath.Clean(actualRoot) != filepath.Clean(layout.RepoRoot) {
				// actualRoot being the worktree itself is fine (git
				// reports the worktree as its own root). We only fail
				// when it's clearly a different repo.
				return nil, fmt.Errorf("worktree %s belongs to %s, not %s", layout.WorktreeDir, actualRoot, layout.RepoRoot)
			}
		case err == nil && !existing.IsDir():
			return nil, fmt.Errorf("worktree path %s exists and is not a directory", layout.WorktreeDir)
		case os.IsNotExist(err):
			if err := git.WorktreeAdd(layout.RepoRoot, layout.WorktreeDir, layout.Branch); err != nil {
				return nil, fmt.Errorf("creating worktree: %w", err)
			}
			createdWorktree = true
		default:
			return nil, fmt.Errorf("stat worktree %s: %w", layout.WorktreeDir, err)
		}
	}

	// DB registration.
	var worktreeDirPtr, branchPtr *string
	if layout.WorktreeDir != "" {
		worktreeDirPtr = &layout.WorktreeDir
		branchPtr = &layout.Branch
	}
	if err := db.RegisterAgent(pool, agentID, tmuxSession, agentID, worktreeDirPtr, branchPtr, runnerName, nil, role); err != nil {
		if createdWorktree {
			git.WorktreeRemove(layout.RepoRoot, layout.WorktreeDir)
		}
		return nil, fmt.Errorf("registering agent: %w", err)
	}

	// Tmux window.
	mergedEnv := mergeEnv(env, r)
	if err := tmux.NewWindowWithDir(tmuxSession, agentID, layout.WindowCWD(), mergedEnv); err != nil {
		db.DeleteAgent(pool, agentID)
		if createdWorktree {
			git.WorktreeRemove(layout.RepoRoot, layout.WorktreeDir)
		}
		return nil, fmt.Errorf("creating tmux window: %w", err)
	}

	sendBootstrap(tmuxSession, agentID, claudeMDPath, env, worktreeDirPtr, branchPtr, r)

	if r != nil && !r.HasSessionHook() {
		upsertHooklessAgentCWD(pool, agentID, layout.WindowCWD())
	}

	now := time.Now()
	a := &Agent{
		ID:          agentID,
		TmuxSession: tmuxSession,
		TmuxWindow:  agentID,
		Status:      "idle",
		StartedAt:   now,
		LastSeen:    &now,
	}
	if layout.WorktreeDir != "" {
		a.WorktreeDir = &layout.WorktreeDir
		a.Branch = &layout.Branch
	}
	return a, nil
}

// upsertHooklessAgentCWD persists the working directory on the agents
// row for runners with no SessionStart hook (OpenCode). session_id is
// left NULL; the monitor source discovers it later from the runner's
// own DB. Replaces the retired session_map.json fallback — all state
// now lives in Postgres per §0 of maquinista-v2.md.
func upsertHooklessAgentCWD(pool *pgxpool.Pool, agentID, workDir string) {
	if pool == nil {
		return
	}
	windowID, err := tmux.GetWindowID(os.Getenv("MAQUINISTA_TMUX_SESSION"), agentID)
	_ = windowID // not strictly required; the row already carries tmux_window from RegisterAgent
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		UPDATE agents
		SET cwd = COALESCE(NULLIF($2,''), cwd),
		    window_name = COALESCE(NULLIF($1,''), window_name),
		    last_seen = NOW()
		WHERE id = $1
	`, agentID, workDir); err != nil {
		// Fail open — the monitor will still discover the cwd from
		// whichever runner-side source it already reads.
		_ = err
	}
}

// mergeEnv merges runner env overrides into the base env map.
func mergeEnv(base map[string]string, r runner.AgentRunner) map[string]string {
	merged := make(map[string]string, len(base))
	for k, v := range base {
		merged[k] = v
	}
	if r != nil {
		for k, v := range r.EnvOverrides() {
			merged[k] = v
		}
	}
	return merged
}

func sendBootstrap(tmuxSession, agentID, claudeMDPath string, env map[string]string, worktreeDir, branch *string, r runner.AgentRunner) {
	scriptsDir := filepath.Join(filepath.Dir(claudeMDPath), "..", "scripts")
	absScripts, _ := filepath.Abs(scriptsDir)

	bootstrap := []string{
		fmt.Sprintf("export AGENT_ID=%q", agentID),
		fmt.Sprintf("export DATABASE_URL=%q", env["DATABASE_URL"]),
		fmt.Sprintf("export PATH=\"$PATH:%s\"", absScripts),
	}

	if worktreeDir != nil {
		bootstrap = append(bootstrap, fmt.Sprintf("export WORKTREE_DIR=%q", *worktreeDir))
	}
	if branch != nil {
		bootstrap = append(bootstrap, fmt.Sprintf("export BRANCH=%q", *branch))
	}

	claudeMDArg := claudeMDPath
	if worktreeDir != nil {
		wtClaudeMD := filepath.Join(*worktreeDir, "claude", "agent-loop.md")
		if _, err := os.Stat(wtClaudeMD); err == nil {
			claudeMDArg = wtClaudeMD
		}
	}

	prompt := fmt.Sprintf("$(cat %s)", claudeMDArg)
	if r != nil {
		cfg := runner.Config{Env: env}
		if worktreeDir != nil {
			cfg.WorkDir = *worktreeDir
		}
		bootstrap = append(bootstrap, r.InteractiveCommand(prompt, cfg))
	} else {
		// Fallback to hardcoded claude command.
		bootstrap = append(bootstrap, fmt.Sprintf("claude --dangerously-skip-permissions -p \"%s\"", prompt))
	}

	for _, cmd := range bootstrap {
		tmux.SendKeysWithDelay(tmuxSession, agentID, cmd, 100)
	}
}

// Kill terminates an agent: kills the tmux window, releases claimed tasks, removes from DB.
func Kill(pool *pgxpool.Pool, tmuxSession, agentID string) error {
	a, err := db.GetAgent(pool, agentID)
	if err != nil {
		return fmt.Errorf("getting agent: %w", err)
	}

	tmux.KillWindow(tmuxSession, agentID)

	if a != nil && a.WorktreeDir != nil && a.Branch != nil {
		repoRoot, rootErr := git.RepoRoot(".")
		if rootErr == nil {
			unmerged, err := git.HasUnmergedChanges(repoRoot, *a.Branch, "main")
			if err != nil {
				fmt.Printf("warning: could not check unmerged changes for %s: %v\n", agentID, err)
			} else if unmerged {
				fmt.Printf("warning: preserving worktree %s — branch %s has unmerged changes\n", *a.WorktreeDir, *a.Branch)
			} else {
				if err := git.WorktreeRemove(repoRoot, *a.WorktreeDir); err != nil {
					fmt.Printf("warning: failed to remove worktree %s: %v\n", *a.WorktreeDir, err)
				}
			}
		}
	}

	if err := db.DeleteAgent(pool, agentID); err != nil {
		return fmt.Errorf("deleting agent from DB: %w", err)
	}

	return nil
}

// KillAll terminates task-scoped agents (task_id IS NOT NULL) and
// parks persistent user agents. Persistent rows keep their session_id
// + soul so the next `./maquinista start` can respawn the panes with
// --resume <sid> and preserve context. Called by `maquinista stop`.
func KillAll(pool *pgxpool.Pool, tmuxSession string) error {
	agents, err := db.ListAgents(pool)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	for _, a := range agents {
		// Task-scoped ephemeral agents are deleted outright — they have
		// no cross-restart meaning. Same for agents explicitly marked
		// as executors (non-user roles).
		if a.TaskID != nil && *a.TaskID != "" {
			if err := Kill(pool, tmuxSession, a.ID); err != nil {
				fmt.Printf("warning: failed to kill agent %s: %v\n", a.ID, err)
			}
			continue
		}
		// Persistent user agents: preserve the row. The tmux window is
		// about to die with the session; mark status='stopped' and
		// clear tmux_window so the startup reconcile respawns cleanly.
		if _, err := pool.Exec(context.Background(), `
			UPDATE agents
			SET status = 'stopped',
			    tmux_window = '',
			    last_seen = NOW(),
			    stop_requested = FALSE
			WHERE id = $1
		`, a.ID); err != nil {
			fmt.Printf("warning: park persistent agent %s: %v\n", a.ID, err)
		}
	}
	return nil
}

// Heartbeat updates an agent's last_seen and status.
func Heartbeat(pool *pgxpool.Pool, agentID, status string) error {
	return db.UpdateAgentStatus(pool, agentID, status)
}

// List returns all registered agents with their task assignments.
func List(pool *pgxpool.Pool) ([]*db.Agent, error) {
	return db.ListAgents(pool)
}
