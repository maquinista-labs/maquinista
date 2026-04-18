package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// reconcileAgentPanes walks every persistent agent (role='user',
// task_id IS NULL, status live) and ensures each has a running tmux
// window. After a `./maquinista stop` / `start` cycle the tmux session
// itself is gone; this function brings the panes back.
//
// When agents.session_id is set (populated by the Claude SessionStart
// hook or the OpenCode source's discovery pass), the runner is
// launched with its native "resume this session" flag so the
// conversation history survives the restart. Fresh rows with empty
// session_id get the normal soul-injected boot.
//
// Runs in the foreground — the caller waits for all known agents to
// come up before the bot starts accepting messages. Each respawn
// blocks up to 15s waiting for the TUI to be ready; errors are logged
// and the next agent proceeds.
func reconcileAgentPanes(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, botState *state.State, defaultCWD string) (int, error) {
	if pool == nil {
		return 0, nil
	}
	// Respawn persistent user agents whose pane is absent. stop_requested
	// filters out agents the operator explicitly `agent kill`-ed — those
	// stay parked. Status is widened to 'stopped' because `maquinista
	// stop` parks rows that way (see agent.KillAll). status='archived'
	// and 'dead' are permanent terminal states — skipped.
	rows, err := pool.Query(ctx, `
		SELECT id, COALESCE(cwd,''), COALESCE(tmux_window,''), COALESCE(runner_type,''),
		       COALESCE(session_id,''),
		       COALESCE(workspace_scope,'shared'),
		       COALESCE(workspace_repo_root,'')
		FROM agents
		WHERE role = 'user'
		  AND task_id IS NULL
		  AND stop_requested = FALSE
		  AND status IN ('running','idle','working','stopped')
		ORDER BY started_at ASC
	`)
	if err != nil {
		return 0, fmt.Errorf("list live agents: %w", err)
	}
	defer rows.Close()

	type agentRow struct {
		ID, CWD, TmuxWindow, RunnerType, SessionID, WorkspaceScope, RepoRoot string
	}
	var agents []agentRow
	for rows.Next() {
		var a agentRow
		if err := rows.Scan(&a.ID, &a.CWD, &a.TmuxWindow, &a.RunnerType, &a.SessionID, &a.WorkspaceScope, &a.RepoRoot); err != nil {
			return 0, err
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	respawned := 0
	for _, a := range agents {
		// Already live? The DB-tracked tmux_window may be stale after a
		// daemon restart — the tmux session was torn down, so the id we
		// remembered won't resolve. tmuxWindowExists guards against that.
		if a.TmuxWindow != "" && tmuxWindowExists(cfg.TmuxSessionName, a.TmuxWindow) {
			continue
		}
		if err := respawnAgent(ctx, cfg, pool, botState, defaultCWD, a.ID, a.CWD, a.RunnerType, a.SessionID, a.WorkspaceScope, a.RepoRoot); err != nil {
			log.Printf("reconcile: respawn %s: %v", a.ID, err)
			continue
		}
		respawned++
	}
	return respawned, nil
}

// respawnAgent recreates a tmux window for a known agent row. Reuses
// the agents row's cwd / runner_type / session_id when set, falling
// back to the daemon default for missing values.
//
// When workspaceScope is "agent" or "task" and workspaceRepoRoot is
// populated, the tmux window opens in the resolved worktree dir and
// the worktree is created on demand if missing (restart path).
func respawnAgent(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, botState *state.State, defaultCWD, agentID, cwd, runnerType, sessionID, workspaceScope, workspaceRepoRoot string) error {
	if cwd == "" {
		cwd = defaultCWD
	}
	if cwd == "" {
		return errors.New("no cwd and no defaultCWD")
	}

	// Resolve the workspace layout. For scope=shared this is a no-op;
	// for scope=agent we create (or reuse) a worktree under the
	// project's repo root. scope=task agents don't go through this
	// reconcile path — they're orchestrator-owned — but we handle the
	// case defensively.
	scope := agent.WorkspaceScope(workspaceScope)
	if scope == "" {
		scope = agent.ScopeShared
	}
	if scope != agent.ScopeShared {
		repoRoot := workspaceRepoRoot
		if repoRoot == "" {
			// Legacy rows without workspace_repo_root: derive from cwd
			// best-effort. Log once so the operator sees the deprecation.
			if root, rerr := git.RepoRoot(cwd); rerr == nil {
				repoRoot = root
				log.Printf("reconcile: %s has scope=%s but no workspace_repo_root; inferred %s from cwd", agentID, scope, repoRoot)
			} else {
				return fmt.Errorf("scope=%s requires workspace_repo_root (row has cwd=%s, not a git repo: %v)", scope, cwd, rerr)
			}
		}
		layout, err := agent.ResolveLayout(scope, repoRoot, agentID, "")
		if err != nil {
			return fmt.Errorf("resolving workspace layout: %w", err)
		}
		// Create the worktree on first start. If the directory already
		// exists and is a worktree, reuse it — preserves in-flight work
		// across daemon restarts.
		if layout.WorktreeDir != "" {
			if _, statErr := os.Stat(layout.WorktreeDir); os.IsNotExist(statErr) {
				if werr := git.WorktreeAdd(layout.RepoRoot, layout.WorktreeDir, layout.Branch); werr != nil {
					return fmt.Errorf("creating worktree %s: %w", layout.WorktreeDir, werr)
				}
				log.Printf("reconcile: %s created worktree %s on branch %s", agentID, layout.WorktreeDir, layout.Branch)
			} else if statErr != nil {
				return fmt.Errorf("stat worktree %s: %w", layout.WorktreeDir, statErr)
			}
		}
		cwd = layout.WindowCWD()
	}

	// Temporarily override the daemon's default runner so
	// resolveRunnerCommand / EnvOverrides / SpawnFunc all pick this
	// agent's runner. Restored when we're done.
	savedRunner := cfg.DefaultRunner
	if runnerType != "" {
		cfg.DefaultRunner = runnerType
	}
	defer func() { cfg.DefaultRunner = savedRunner }()

	// Ensure the tmux session exists, then open the window with the
	// resume-aware command line.
	if err := tmux.EnsureSession(cfg.TmuxSessionName); err != nil {
		return fmt.Errorf("ensure tmux session: %w", err)
	}

	// hasSoul controls --system-prompt injection; we hit the soul table
	// to know whether the agent has identity. On resume (sessionID set),
	// resolveRunnerCommand skips --system-prompt anyway, so this only
	// matters for fresh-boot fallback when sessionID is empty.
	hasSoul := false
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1 FROM agent_souls WHERE agent_id=$1`, agentID).Scan(&one); err == nil {
		hasSoul = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("reconcile: soul check for %s: %v (continuing)", agentID, err)
	}

	runnerCmd, env := resolveRunnerCommand(cfg, agentID, cwd, hasSoul, sessionID)
	windowID, err := tmux.NewWindow(cfg.TmuxSessionName, agentID, cwd, runnerCmd, env)
	if err != nil {
		return fmt.Errorf("new tmux window: %w", err)
	}
	label := "fresh"
	if sessionID != "" {
		label = "resume=" + sessionID
	}
	log.Printf("reconcile: respawned %s at %s:%s (cwd=%s, runner=%s, %s)",
		agentID, cfg.TmuxSessionName, windowID, cwd, runnerCmd, label)

	// Wait for the runner TUI so the inbox consumer's first send-keys
	// lands on a live prompt. Same timeout we use in tier-3 spawn.
	if err := waitForRunnerReady(cfg.TmuxSessionName, windowID, 15*time.Second); err != nil {
		log.Printf("reconcile: %s not ready within timeout: %v", agentID, err)
	}

	// Publish the new tmux_window. Database controls status; only update
	// tmux_window (derived state). Never override stop_requested or status.
	upCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := pool.Exec(upCtx, `
		UPDATE agents
		SET tmux_window = $2, last_seen = NOW()
		WHERE id = $1
	`, agentID, windowID); err != nil {
		return fmt.Errorf("update agents: %w", err)
	}

	if botState != nil {
		botState.SetWindowRunner(windowID, cfg.DefaultRunner)
		botState.SetWindowDisplayName(windowID, agentID)
		_ = botState.Save(filepath.Join(cfg.MaquinistaDir, "state.json"))
	}
	return nil
}
