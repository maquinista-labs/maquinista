package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/memory"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/soul"
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
// per plans/archive/per-topic-agent-pivot.md.
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
		// Race guard: another goroutine is already spawning this agent
		// (status='spawning', tmux_window still empty). Return the existing
		// agentID so the message gets enqueued — the in-progress spawn will
		// assign the window. Without this, a second rapid message would
		// create a second tmux pane and the two spawns race to write
		// tmux_window, leaving state.json pointing at the wrong window.
		if err == nil && status == "spawning" {
			log.Printf("spawn_topic_agent: %s is already spawning; reusing", agentID)
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

		// Pre-register an agents row with status='spawning' so any concurrent
		// spawn (rapid second message before W1 is ready) sees the marker and
		// returns early instead of creating a second tmux pane. We flip to
		// 'running' with the real tmux_window after the pane is up.
		if _, err := pool.Exec(ctx, `
			INSERT INTO agents
				(id, tmux_session, tmux_window, role, status, runner_type,
				 session_id, cwd, window_name,
				 started_at, last_seen, stop_requested)
			VALUES ($1, $2, '', 'user', 'spawning', $3, NULL, $4, $1, NOW(), NOW(), FALSE)
			ON CONFLICT (id) DO UPDATE SET
				tmux_session   = EXCLUDED.tmux_session,
				runner_type    = EXCLUDED.runner_type,
				cwd            = EXCLUDED.cwd,
				window_name    = EXCLUDED.window_name,
				status         = 'spawning',
				stop_requested = FALSE
		`, agentID, cfg.TmuxSessionName, cfg.DefaultRunner, cwd); err != nil {
			return "", fmt.Errorf("pre-registering topic agent row: %w", err)
		}

		// Create soul (default template), seed memory blocks. Both live in
		// Postgres only — no prompt file on disk. The runner launch
		// command pulls the rendered soul via `maquinista soul render <id>`
		// at spawn time via shell substitution ($()), so the DB stays the
		// single source of truth per §0 of maquinista-v2.md.
		if err := soul.CreateFromTemplate(ctx, pool, agentID, "", soul.Overrides{}); err != nil {
			log.Printf("spawn_topic_agent: soul create for %s: %v (continuing)", agentID, err)
		}
		var seedPersona string
		hasSoul := false
		if s, lerr := soul.Load(ctx, pool, agentID); lerr == nil && s != nil {
			seedPersona = s.CoreTruths
			hasSoul = true
		}
		if err := memory.SeedDefaultBlocks(ctx, pool, agentID, seedPersona); err != nil {
			log.Printf("spawn_topic_agent: seed memory blocks for %s: %v (continuing)", agentID, err)
		}

		// If an existing agents row carries a runner session_id, we're
		// respawning the same topic's pane after a stop/start cycle —
		// hand it to the runner as the resume token so context carries
		// across restarts.
		var resumeSessionID string
		_ = pool.QueryRow(ctx,
			`SELECT COALESCE(session_id,'') FROM agents WHERE id=$1`,
			agentID,
		).Scan(&resumeSessionID)

		runnerCmd, env := resolveRunnerCommand(cfg, agentID, cwd, hasSoul, resumeSessionID)

		windowID, err := tmux.NewWindow(cfg.TmuxSessionName, agentID, cwd, runnerCmd, env)
		if err != nil {
			return "", fmt.Errorf("spawning topic agent window: %w", err)
		}
		log.Printf("spawn_topic_agent: spawned %s at %s:%s (cwd=%s, runner=%s, soul=%v)",
			agentID, cfg.TmuxSessionName, windowID, cwd, runnerCmd, hasSoul)

		// Block until the runner's TUI is ready to accept keystrokes.
		if err := waitForRunnerReady(cfg.TmuxSessionName, windowID, 15*time.Second); err != nil {
			log.Printf("spawn_topic_agent: %s not ready within timeout: %v (first message may need manual Enter)",
				agentID, err)
		}

		// Flip the agents row to running and publish the real tmux_window.
		if _, err := pool.Exec(ctx, `
			UPDATE agents
			SET tmux_window = $2, status = 'running', last_seen = NOW()
			WHERE id = $1
		`, agentID, windowID); err != nil {
			return "", fmt.Errorf("finalizing topic agent row: %w", err)
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

// waitForRunnerReady polls the tmux pane until the interactive runner is
// ready to accept input, or the timeout elapses. Detects the Claude TUI
// prompt (`❯` chevron) and OpenCode's build-status bar; falls back to a
// short settle delay for unknown runners. Tuned for the first-message
// race where send-keys arrives before the TUI has drawn its prompt.
func waitForRunnerReady(session, windowID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text, err := tmux.CapturePane(session, windowID, false)
		if err == nil && runnerReady(text) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// One last capture for the error message.
	text, _ := tmux.CapturePane(session, windowID, false)
	return fmt.Errorf("runner did not become ready in %s; last pane snippet: %q",
		timeout, lastLine(text))
}

// runnerReady returns true when the pane text carries a known
// "accepting input" marker. Keep the matches loose so minor TUI version
// bumps don't break startup — readiness detection is best-effort; the
// caller logs and continues on timeout anyway.
func runnerReady(paneText string) bool {
	switch {
	// Claude Code prompt — "❯" chevron at the start of a line, plus the
	// permissions footer that only renders once the TUI is live.
	case strings.Contains(paneText, "\n❯") || strings.HasPrefix(paneText, "❯"):
		return true
	case strings.Contains(paneText, "bypass permissions on"):
		return true
	// OpenCode — the "Build" status bar appears once the TUI is up.
	case strings.Contains(paneText, "Build "):
		return true
	}
	return false
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
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
// runner.
//
//  - hasSoul=true and fresh start (resumeSessionID=="") → append
//    `--system-prompt "$(maquinista soul render <id>)"` for Claude-family
//    runners. DB is source of truth for identity.
//  - resumeSessionID != "" → append the runner's "resume this session"
//    flag (claude/openclaude: --resume <sid>; opencode: --session <sid>)
//    and SKIP --system-prompt because the resumed session already has
//    the original system prompt in its history.
func resolveRunnerCommand(cfg *config.Config, agentID, cwd string, hasSoul bool, resumeSessionID string) (string, map[string]string) {
	env := map[string]string{
		"AGENT_ID":    agentID,
		"RUNNER_TYPE": cfg.DefaultRunner,
	}
	if cfg.DatabaseURL != "" {
		env["DATABASE_URL"] = cfg.DatabaseURL
	}
	r, rerr := runner.Get(cfg.DefaultRunner)
	if rerr != nil {
		cmd := cfg.ClaudeCommand
		if cmd == "" {
			cmd = "claude"
		}
		return cmd, env
	}
	for k, v := range r.EnvOverrides() {
		env[k] = v
	}

	cmd := r.LaunchCommand(runner.Config{WorkDir: cwd, Env: env})

	// Resume path: runner-native flag, no soul injection (the stored
	// session already has it).
	if resumeSessionID != "" {
		switch r.Name() {
		case "claude", "openclaude":
			return fmt.Sprintf("%s --resume %s", cmd, resumeSessionID), env
		case "opencode":
			return fmt.Sprintf("%s --session %s", cmd, resumeSessionID), env
		}
		// Unknown runner — fall through to fresh-launch path.
	}

	if !hasSoul {
		return cmd, env
	}
	switch r.Name() {
	case "claude", "openclaude":
		maquinistaBin := cfg.MaquinistaBin
		if maquinistaBin == "" {
			maquinistaBin = "maquinista"
		}
		cmd = fmt.Sprintf(`%s --system-prompt "$(%s soul render %s)"`, cmd, maquinistaBin, agentID)
	}
	return cmd, env
}
