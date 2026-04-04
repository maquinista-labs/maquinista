package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/otaviocarvalho/volta/internal/git"
	"github.com/otaviocarvalho/volta/internal/runner"
	"github.com/otaviocarvalho/volta/internal/state"
	"github.com/otaviocarvalho/volta/internal/tmux"
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

// Spawn registers an agent in the DB, creates a tmux window, and sends the bootstrap command.
// role should be "executor" (default) or "planner".
func Spawn(pool *pgxpool.Pool, tmuxSession, agentID, claudeMDPath string, env map[string]string, r runner.AgentRunner, role string) (*Agent, error) {
	runnerName := "claude"
	if r != nil {
		runnerName = r.Name()
	}
	if role == "" {
		role = "executor"
	}

	if err := db.RegisterAgent(pool, agentID, tmuxSession, agentID, nil, nil, runnerName, nil, role); err != nil {
		return nil, fmt.Errorf("registering agent: %w", err)
	}

	// Merge runner env overrides into the tmux window env.
	mergedEnv := mergeEnv(env, r)

	if err := tmux.NewWindowWithDir(tmuxSession, agentID, ".", mergedEnv); err != nil {
		db.DeleteAgent(pool, agentID)
		return nil, fmt.Errorf("creating tmux window: %w", err)
	}

	sendBootstrap(tmuxSession, agentID, claudeMDPath, env, nil, nil, r)

	if r != nil && !r.HasSessionHook() {
		workDir, _ := filepath.Abs(".")
		writeSessionMapFallback(tmuxSession, agentID, workDir)
	}

	now := time.Now()
	return &Agent{
		ID:          agentID,
		TmuxSession: tmuxSession,
		TmuxWindow:  agentID,
		Status:      "idle",
		StartedAt:   now,
		LastSeen:    &now,
	}, nil
}

// SpawnWithWorktree registers an agent with an isolated git worktree.
// role should be "executor" (default) or "planner".
func SpawnWithWorktree(pool *pgxpool.Pool, tmuxSession, agentID, claudeMDPath string, env map[string]string, r runner.AgentRunner, role string) (*Agent, error) {
	repoRoot, err := git.RepoRoot(".")
	if err != nil {
		return nil, fmt.Errorf("finding repo root: %w", err)
	}

	worktreeDir := filepath.Join(repoRoot, ".volta", "worktrees", agentID)
	branch := "volta/" + agentID

	if err := git.WorktreeAdd(repoRoot, worktreeDir, branch); err != nil {
		return nil, fmt.Errorf("creating worktree: %w", err)
	}

	runnerName := "claude"
	if r != nil {
		runnerName = r.Name()
	}

	if role == "" {
		role = "executor"
	}

	if err := db.RegisterAgent(pool, agentID, tmuxSession, agentID, &worktreeDir, &branch, runnerName, nil, role); err != nil {
		git.WorktreeRemove(repoRoot, worktreeDir)
		return nil, fmt.Errorf("registering agent: %w", err)
	}

	// Merge runner env overrides into the tmux window env.
	mergedEnv := mergeEnv(env, r)

	if err := tmux.NewWindowWithDir(tmuxSession, agentID, worktreeDir, mergedEnv); err != nil {
		db.DeleteAgent(pool, agentID)
		git.WorktreeRemove(repoRoot, worktreeDir)
		return nil, fmt.Errorf("creating tmux window: %w", err)
	}

	sendBootstrap(tmuxSession, agentID, claudeMDPath, env, &worktreeDir, &branch, r)

	if r != nil && !r.HasSessionHook() {
		writeSessionMapFallback(tmuxSession, agentID, worktreeDir)
	}

	now := time.Now()
	return &Agent{
		ID:          agentID,
		TmuxSession: tmuxSession,
		TmuxWindow:  agentID,
		Status:      "idle",
		StartedAt:   now,
		LastSeen:    &now,
		WorktreeDir: &worktreeDir,
		Branch:      &branch,
	}, nil
}

// writeSessionMapFallback writes a preliminary session_map.json entry for runners
// that have no SessionStart hook (e.g. OpenCode). It uses the agent ID as a stable
// proxy for the session ID so bot handlers can route Telegram messages correctly.
func writeSessionMapFallback(tmuxSession, agentID, workDir string) {
	windowID, err := tmux.GetWindowID(tmuxSession, agentID)
	if err != nil || windowID == "" {
		return // tmux not available or window not found; skip silently
	}

	dir := os.Getenv("VOLTA_DIR")
	if dir == "" {
		dir = "~/.volta"
	}
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			dir = filepath.Join(home, dir[2:])
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	key := tmuxSession + ":" + windowID
	sessionMapPath := filepath.Join(dir, "session_map.json")
	_ = state.ReadModifyWriteSessionMap(sessionMapPath, func(data map[string]state.SessionMapEntry) {
		data[key] = state.SessionMapEntry{
			SessionID:       "",
			CWD:             workDir,
			WindowName:      agentID,
			WindowCreatedAt: time.Now().UnixMilli(),
		}
	})
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

// KillAll terminates all registered agents.
func KillAll(pool *pgxpool.Pool, tmuxSession string) error {
	agents, err := db.ListAgents(pool)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	for _, a := range agents {
		if err := Kill(pool, tmuxSession, a.ID); err != nil {
			fmt.Printf("warning: failed to kill agent %s: %v\n", a.ID, err)
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
