package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// ensureDefaultAgent creates a tmux window running the configured runner
// with AGENT_ID exported, unless:
//   - --no-agent was passed
//   - the agents table already has a live row for the resolved id
//   - a tmux window with the agent's name already exists in the session
//
// Precedence for the agent id: --agent flag > MAQUINISTA_DEFAULT_AGENT env
// > cfg.DefaultAgent (populated from env with fallback "maquinista").
func ensureDefaultAgent(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) error {
	if startNoAgent {
		return nil
	}

	agentID := strings.TrimSpace(startAgent)
	if agentID == "" {
		agentID = cfg.DefaultAgent
	}
	if agentID == "" {
		agentID = "maquinista"
	}

	cwd := strings.TrimSpace(startAgentCWD)
	if cwd == "" {
		cwd = cfg.DefaultAgentCWD
	}
	if cwd == "" {
		// Default to the dir `maquinista start` was invoked from — likely
		// a folder the user already trusts in Claude Code, avoiding the
		// workspace-trust prompt on first spawn. Fall back to $HOME only
		// if os.Getwd fails.
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		} else if h, herr := os.UserHomeDir(); herr == nil {
			cwd = h
		} else {
			return fmt.Errorf("resolving default cwd: getwd=%v home=%v", err, herr)
		}
	}

	// Skip if agent is already live in DB.
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var status string
	err := pool.QueryRow(qctx, `
		SELECT status FROM agents WHERE id=$1
	`, agentID).Scan(&status)
	if err == nil && isLiveStatus(status) {
		log.Printf("default agent: %s already registered (status=%s); skipping spawn", agentID, status)
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("checking existing agent: %w", err)
	}

	// Skip if a tmux window with that name already exists — the user
	// probably just started a Claude session manually and the hook is
	// about to upsert the row on its own.
	windows, err := tmux.ListWindows(cfg.TmuxSessionName)
	if err == nil {
		for _, w := range windows {
			if w.Name == agentID {
				log.Printf("default agent: tmux window %q (%s) already exists; skipping spawn", w.Name, w.ID)
				return nil
			}
		}
	}

	// Ensure the tmux session exists.
	if err := tmux.EnsureSession(cfg.TmuxSessionName); err != nil {
		return fmt.Errorf("ensuring tmux session: %w", err)
	}

	// Use the runner's LaunchCommand so the spawned pane gets the same
	// sandbox/skip-permissions flags the task runners do (e.g. claude
	// yields "IS_SANDBOX=1 claude --dangerously-skip-permissions"). Falls
	// back to $CLAUDE_COMMAND if the runner isn't registered.
	var runnerCmd string
	env := map[string]string{
		"AGENT_ID":    agentID,
		"RUNNER_TYPE": cfg.DefaultRunner,
	}
	if cfg.DatabaseURL != "" {
		env["DATABASE_URL"] = cfg.DatabaseURL
	}
	if r, err := runner.Get(cfg.DefaultRunner); err == nil {
		runnerCmd = r.LaunchCommand(runner.Config{WorkDir: cwd, Env: env})
		for k, v := range r.EnvOverrides() {
			env[k] = v
		}
	} else {
		runnerCmd = cfg.ClaudeCommand
		if runnerCmd == "" {
			runnerCmd = "claude"
		}
	}

	windowID, err := tmux.NewWindow(cfg.TmuxSessionName, agentID, cwd, runnerCmd, env)
	if err != nil {
		return fmt.Errorf("spawning default agent window: %w", err)
	}
	log.Printf("default agent: spawned %s at %s:%s (cwd=%s, runner=%s)",
		agentID, cfg.TmuxSessionName, windowID, cwd, runnerCmd)
	return nil
}

func isLiveStatus(s string) bool {
	switch s {
	case "running", "working", "idle":
		return true
	}
	return false
}
