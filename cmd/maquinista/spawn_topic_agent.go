package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// resolveStartCWD picks the working directory new topic agents inherit.
// Precedence: --agent-cwd flag, cfg.DefaultAgentCWD, process working
// directory (os.Getwd), user home. Mirrors the retired ensureDefaultAgent
// heuristic so Claude's workspace-trust prompt doesn't fire on first spawn.
func resolveStartCWD(cfg *config.Config) (string, error) {
	if cwd := strings.TrimSpace(startAgentCWD); cwd != "" {
		return cwd, nil
	}
	if cfg.DefaultAgentCWD != "" {
		return cfg.DefaultAgentCWD, nil
	}
	if wd, err := os.Getwd(); err == nil {
		return wd, nil
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h, nil
	}
	return "", errors.New("could not resolve a working directory for spawn_topic_agent")
}

// newTopicAgentSpawner returns a routing.SpawnFunc closure that creates a
// fresh per-topic agent: new agents row (id = t-<chat>-<thread>), new tmux
// window, new runner process. Wired into the bot's routing ladder tier 3
// per plans/per-topic-agent-pivot.md.
//
// Safe to call concurrently: the upsert on agents.id collapses same-topic
// races; tmux window creation is idempotent via EnsureSession + unique
// window name.
//
// cwd: defaults to the directory from which `maquinista start` was invoked
// (same heuristic the retired ensureDefaultAgent used). Avoids Claude's
// workspace-trust prompt on first spawn.
func newTopicAgentSpawner(cfg *config.Config, pool *pgxpool.Pool, botState *state.State, defaultCWD string) func(ctx context.Context, userID, threadID string, chatID *int64) (string, error) {
	return func(ctx context.Context, userID, threadID string, chatID *int64) (string, error) {
		if chatID == nil {
			return "", errors.New("spawn_topic_agent: chat_id is required for t-<chat>-<thread> id")
		}
		agentID := fmt.Sprintf("t-%d-%s", *chatID, threadID)

		// If the row exists and its tmux window is live, reuse it — same
		// topic, same agent. Matches "resume on re-send" semantics.
		var status, existingWindow string
		err := pool.QueryRow(ctx, `
			SELECT status, COALESCE(tmux_window,'') FROM agents WHERE id=$1
		`, agentID).Scan(&status, &existingWindow)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("checking existing agent: %w", err)
		}
		if err == nil && isLiveStatus(status) && tmuxWindowExists(cfg.TmuxSessionName, existingWindow) {
			log.Printf("spawn_topic_agent: %s already live at %s:%s; reusing",
				agentID, cfg.TmuxSessionName, existingWindow)
			// Keep state.json in sync with the live row.
			if botState != nil {
				botState.SetWindowRunner(existingWindow, cfg.DefaultRunner)
				botState.SetWindowDisplayName(existingWindow, agentID)
				_ = botState.Save(filepath.Join(cfg.MaquinistaDir, "state.json"))
			}
			return agentID, nil
		}
		if err == nil {
			log.Printf("spawn_topic_agent: %s row says %s; respawning pane", agentID, status)
			if _, uerr := pool.Exec(ctx, `
				UPDATE agents SET status='stopped', stop_requested=TRUE, last_seen=NOW()
				WHERE id=$1
			`, agentID); uerr != nil {
				log.Printf("spawn_topic_agent: mark-stopped: %v", uerr)
			}
		}

		cwd := defaultCWD
		if cwd == "" {
			return "", errors.New("spawn_topic_agent: no defaultCWD resolved")
		}

		if err := tmux.EnsureSession(cfg.TmuxSessionName); err != nil {
			return "", fmt.Errorf("ensuring tmux session: %w", err)
		}

		runnerCmd, env := resolveRunnerCommand(cfg, agentID, cwd)

		windowID, err := tmux.NewWindow(cfg.TmuxSessionName, agentID, cwd, runnerCmd, env)
		if err != nil {
			return "", fmt.Errorf("spawning topic agent window: %w", err)
		}
		log.Printf("spawn_topic_agent: spawned %s at %s:%s (cwd=%s, runner=%s)",
			agentID, cfg.TmuxSessionName, windowID, cwd, runnerCmd)

		if _, err := pool.Exec(ctx, `
			INSERT INTO agents
				(id, tmux_session, tmux_window, role, status, runner_type,
				 started_at, last_seen, stop_requested)
			VALUES ($1, $2, $3, 'user', 'running', $4, NOW(), NOW(), FALSE)
			ON CONFLICT (id) DO UPDATE SET
				tmux_session   = EXCLUDED.tmux_session,
				tmux_window    = EXCLUDED.tmux_window,
				status         = 'running',
				runner_type    = EXCLUDED.runner_type,
				last_seen      = NOW(),
				stop_requested = FALSE
		`, agentID, cfg.TmuxSessionName, windowID, cfg.DefaultRunner); err != nil {
			return "", fmt.Errorf("registering topic agent row: %w", err)
		}

		if botState != nil {
			botState.SetWindowRunner(windowID, cfg.DefaultRunner)
			botState.SetWindowDisplayName(windowID, agentID)
			if serr := botState.Save(filepath.Join(cfg.MaquinistaDir, "state.json")); serr != nil {
				log.Printf("spawn_topic_agent: state.Save: %v", serr)
			}
		}
		return agentID, nil
	}
}

// isLiveStatus matches an agent status against the "pane should be up"
// set. Lifted from the retired ensureDefaultAgent.
func isLiveStatus(s string) bool {
	switch s {
	case "running", "working", "idle":
		return true
	}
	return false
}

// tmuxWindowExists returns true when session:windowID resolves to a real
// tmux window. Empty windowID short-circuits to false.
func tmuxWindowExists(session, windowID string) bool {
	if windowID == "" {
		return false
	}
	windows, err := tmux.ListWindows(session)
	if err != nil {
		return false
	}
	for _, w := range windows {
		if w.ID == windowID {
			return true
		}
	}
	return false
}

// resolveRunnerCommand picks the shell command line for the configured
// runner. Factored out so spawn_topic_agent and any future spawn callers
// stay consistent on sandbox / skip-permissions flags.
func resolveRunnerCommand(cfg *config.Config, agentID, cwd string) (string, map[string]string) {
	env := map[string]string{
		"AGENT_ID":    agentID,
		"RUNNER_TYPE": cfg.DefaultRunner,
	}
	if cfg.DatabaseURL != "" {
		env["DATABASE_URL"] = cfg.DatabaseURL
	}
	if r, err := runner.Get(cfg.DefaultRunner); err == nil {
		for k, v := range r.EnvOverrides() {
			env[k] = v
		}
		return r.LaunchCommand(runner.Config{WorkDir: cwd, Env: env}), env
	}
	cmd := cfg.ClaudeCommand
	if cmd == "" {
		cmd = "claude"
	}
	return cmd, env
}
