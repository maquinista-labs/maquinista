// Package agentspawn provides primitives for spawning agent processes in tmux.
// It extracts ResolveRunnerCmd and WaitForReady from cmd/maquinista (package
// main) so other packages (e.g. the scheduler dispatcher) can reuse them
// without an import cycle. It also provides SpawnFresh for job-spawned agents.
package agentspawn

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/soul"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// AgentSpawner is implemented by sidecar.Manager; it starts the per-agent
// inbox goroutine for a newly-created agent.
type AgentSpawner interface {
	Spawn(agentID string)
}

// FreshParams holds the inputs for SpawnFresh.
type FreshParams struct {
	// AgentID is the pre-generated agent identifier (caller's responsibility).
	AgentID string
	// CWD is the working directory for the new agent's tmux pane.
	CWD string
	// SoulTemplateID is the soul template to clone for this agent.
	SoulTemplateID string
	// RunnerType overrides cfg.DefaultRunner when non-empty.
	RunnerType string
}

// SpawnFresh creates a fresh agent:
//  1. Inserts an agents row (status=stopped).
//  2. Clones the soul template.
//  3. Opens a new tmux window with the configured runner.
//  4. Waits for the runner to be ready (warns on timeout, does not fail).
//  5. Flips the agents row to status=running with the real tmux_window.
//  6. Calls spawner.Spawn(agentID) to start the inbox goroutine.
//
// The caller is responsible for enqueuing the first inbox message.
func SpawnFresh(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, p FreshParams, spawner AgentSpawner) (windowID string, err error) {
	runnerType := p.RunnerType
	if runnerType == "" {
		runnerType = cfg.DefaultRunner
	}

	// 1. Pre-register agents row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents
			(id, tmux_session, tmux_window, role, status, runner_type,
			 cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ($1, $2, '', 'user', 'stopped', $3, $4, $1, NOW(), NOW(), FALSE)
		ON CONFLICT (id) DO NOTHING
	`, p.AgentID, cfg.TmuxSessionName, runnerType, p.CWD); err != nil {
		return "", fmt.Errorf("insert agent row: %w", err)
	}

	// 2. Clone soul template.
	if err := soul.CreateFromTemplate(ctx, pool, p.AgentID, p.SoulTemplateID, soul.Overrides{}); err != nil {
		log.Printf("agentspawn: soul create for %s: %v (continuing)", p.AgentID, err)
	}

	// 3. Ensure tmux session.
	if err := tmux.EnsureSession(cfg.TmuxSessionName); err != nil {
		return "", fmt.Errorf("ensure tmux session: %w", err)
	}

	// 4. Build runner command.
	runnerCmd, env := ResolveRunnerCmd(cfg, p.AgentID, p.CWD, true, "")

	// 5. Open tmux window.
	windowID, err = tmux.NewWindow(cfg.TmuxSessionName, p.AgentID, p.CWD, runnerCmd, env)
	if err != nil {
		return "", fmt.Errorf("new tmux window: %w", err)
	}

	// 6. Wait for runner ready (warn-only).
	if werr := WaitForReady(cfg.TmuxSessionName, windowID, 15*time.Second); werr != nil {
		log.Printf("agentspawn: %s not ready within timeout: %v (first message may need manual Enter)", p.AgentID, werr)
	}

	// 7. Flip to running.
	if _, err := pool.Exec(ctx, `
		UPDATE agents SET tmux_window=$2, status='running', last_seen=NOW() WHERE id=$1
	`, p.AgentID, windowID); err != nil {
		return "", fmt.Errorf("finalize agent row: %w", err)
	}

	// 8. Start inbox goroutine.
	if spawner != nil {
		spawner.Spawn(p.AgentID)
	}

	return windowID, nil
}

// ResolveRunnerCmd picks the shell command line for the configured runner.
//   - hasSoul=true + empty resumeID → --system-prompt injection (claude family).
//   - resumeID set → --resume / --session flag; skips --system-prompt.
func ResolveRunnerCmd(cfg *config.Config, agentID, cwd string, hasSoul bool, resumeID string) (string, map[string]string) {
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

	if resumeID != "" {
		switch r.Name() {
		case "claude", "openclaude":
			return fmt.Sprintf("%s --resume %s", cmd, resumeID), env
		case "opencode":
			return fmt.Sprintf("%s --session %s", cmd, resumeID), env
		}
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

// WaitForReady polls the tmux pane until the runner TUI is ready or timeout.
func WaitForReady(session, windowID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text, err := tmux.CapturePane(session, windowID, false)
		if err == nil && runnerReady(text) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	text, _ := tmux.CapturePane(session, windowID, false)
	return fmt.Errorf("runner did not become ready in %s; last pane snippet: %q",
		timeout, lastLine(text))
}

func runnerReady(paneText string) bool {
	switch {
	case strings.Contains(paneText, "\n❯") || strings.HasPrefix(paneText, "❯"):
		return true
	case strings.Contains(paneText, "bypass permissions on"):
		return true
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

// SlugifyJobName converts a job name into a URL/ID-safe slug: lowercase,
// spaces and underscores become hyphens, non-alphanumeric chars are stripped,
// result truncated to 20 chars. Empty input returns "job".
func SlugifyJobName(name string) string {
	slug := strings.ToLower(name)
	slug = strings.NewReplacer(" ", "-", "_", "-").Replace(slug)
	var b strings.Builder
	for _, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > 20 {
		result = result[:20]
	}
	if result == "" {
		result = "job"
	}
	return result
}
