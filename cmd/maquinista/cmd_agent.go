package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/soul"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

// Phase 3 of plans/active/multi-agent-registry.md — manage persistent
// agent rows (role='user') from the CLI. Task-scoped agents (task_id
// != NULL) are owned by the orchestrator; these subcommands don't
// touch them.

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage persistent agents (add / archive / kill)",
}

var (
	agentAddRunner       string
	agentAddRole         string
	agentAddCWD          string
	agentAddHandle       string
	agentAddSoulTemplate string
	agentAddSystemPrompt string
	agentAddPersona      string
	agentAddScope        string
	agentAddRepoRoot     string
)

var agentAddCmd = &cobra.Command{
	Use:   "add <agent-id>",
	Short: "Insert a new agent row + soul + default memory blocks",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentAdd(args[0])
	},
}

var agentArchiveCmd = &cobra.Command{
	Use:   "archive <agent-id>",
	Short: "Archive an agent (status='archived'); keeps history and bindings",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentArchive(args[0])
	},
}

var agentKillCmd = &cobra.Command{
	Use:   "kill <agent-id>",
	Short: "Signal the agent's runner to exit (stop_requested=TRUE)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentKill(args[0])
	},
}

var agentEditCmd = &cobra.Command{
	Use:   "edit <agent-id>",
	Short: "Update agent-level settings (persona / system prompt / runner / cwd)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentEdit(args[0])
	},
}

var agentSpawnCmd = &cobra.Command{
	Use:   "spawn <agent-id>",
	Short: "Force-respawn an agent: kill its tmux window (if any), clear tmux_window, run reconcile on this row",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentSpawn(args[0])
	},
}

// agentLogsLines + agentLogsCmd — pre-D.5 this was the top-level
// `maquinista logs <agent-id>` command; it now lives under
// `maquinista agent logs <agent-id>` since `logs` belongs to the
// daemon tailer (see cmd_logs.go).
var agentLogsLines int

var agentLogsCmd = &cobra.Command{
	Use:   "logs <agent-id>",
	Short: "Capture last N lines from an agent's tmux window",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentLogs(args[0])
	},
}

func runAgentLogs(agentID string) error {
	if err := connectDB(); err != nil {
		return err
	}
	a, err := db.GetAgent(pool, agentID)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("agent %q not found", agentID)
	}
	output, err := tmux.CapturePaneLines(a.TmuxSession, a.TmuxWindow, agentLogsLines)
	if err != nil {
		return fmt.Errorf("capturing pane: %w", err)
	}
	fmt.Println(output)
	return nil
}

func init() {
	agentAddCmd.Flags().StringVar(&agentAddRunner, "runner", "claude", "runner_type (claude, openclaude, opencode, custom)")
	agentAddCmd.Flags().StringVar(&agentAddRole, "role", "user", "agent role (user | executor)")
	agentAddCmd.Flags().StringVar(&agentAddCWD, "cwd", "", "agent working directory")
	agentAddCmd.Flags().StringVar(&agentAddHandle, "handle", "", "friendly @-handle (unique, lowercase a-z0-9_-)")
	agentAddCmd.Flags().StringVar(&agentAddSoulTemplate, "soul-template", "", "soul template id (default: 'default')")
	agentAddCmd.Flags().StringVar(&agentAddSystemPrompt, "system-prompt", "", "file to load into agent_settings.system_prompt")
	agentAddCmd.Flags().StringVar(&agentAddPersona, "persona", "", "agent_settings.persona text")
	agentAddCmd.Flags().StringVar(&agentAddScope, "scope", "shared", "workspace scope: shared | agent | task (see plans/active/workspace-scopes.md)")
	agentAddCmd.Flags().StringVar(&agentAddRepoRoot, "repo", "", "project git repo root (required when --scope=agent; defaults to --cwd if that's a git repo)")

	agentEditCmd.Flags().StringVar(&agentAddRunner, "runner", "", "new runner_type")
	agentEditCmd.Flags().StringVar(&agentAddCWD, "cwd", "", "new agent working directory")
	agentEditCmd.Flags().StringVar(&agentAddSystemPrompt, "system-prompt", "", "new system prompt file")
	agentEditCmd.Flags().StringVar(&agentAddPersona, "persona", "", "new persona text")
	agentEditCmd.Flags().StringVar(&agentAddScope, "scope", "", "new workspace scope (shared | agent | task); empty = unchanged")
	agentEditCmd.Flags().StringVar(&agentAddRepoRoot, "repo", "", "new project git repo root; empty = unchanged")

	agentLogsCmd.Flags().IntVar(&agentLogsLines, "lines", 50, "number of lines to capture")

	agentCmd.AddCommand(agentAddCmd, agentArchiveCmd, agentKillCmd, agentEditCmd, agentSpawnCmd, agentLogsCmd)
	rootCmd.AddCommand(agentCmd)
}

// runAgentSpawn: force-kill the existing tmux window (if any), clear
// agents.tmux_window + mark status='stopped', then the next routing
// ladder tick (or explicit message) spawns it fresh. For users who
// want to bounce an agent without restarting the whole daemon.
func runAgentSpawn(id string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var existingWindow string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(tmux_window,'') FROM agents WHERE id=$1
	`, id).Scan(&existingWindow); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no such agent: %s", id)
		}
		return err
	}

	if existingWindow != "" {
		if err := tmux.KillWindow(cfg.TmuxSessionName, existingWindow); err != nil {
			// "can't find window" is fine — already gone.
			if !tmux.IsWindowDead(err) {
				return fmt.Errorf("kill tmux window: %w", err)
			}
		}
	}

	if _, err := pool.Exec(ctx, `
		UPDATE agents
		SET tmux_window = '', status = 'stopped', last_seen = NOW()
		WHERE id = $1
	`, id); err != nil {
		return fmt.Errorf("clear tmux_window: %w", err)
	}
	fmt.Printf("Respawn pending for %s — next message in its topic will re-create the pane via tier-3 spawn.\n", id)
	return nil
}

func runAgentAdd(id string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cwd := agentAddCWD
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}

	// Resolve + validate the workspace scope. For scope='agent' we need
	// a repo root: honor --repo, otherwise infer from --cwd if that's a
	// git repo. Persisting workspace_repo_root now (rather than deriving
	// at reconcile time) keeps the layout deterministic across daemon
	// restarts.
	scope := agent.WorkspaceScope(agentAddScope)
	if scope == "" {
		scope = agent.ScopeShared
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	repoRoot := agentAddRepoRoot
	if scope == agent.ScopeAgent && repoRoot == "" {
		repoRoot = cwd
	}
	if scope == agent.ScopeAgent {
		if repoRoot == "" {
			return fmt.Errorf("--scope=agent requires --repo or a --cwd that's a git repo")
		}
		// Trial resolve so the operator sees typos early.
		if _, err := agent.ResolveLayout(scope, repoRoot, id, ""); err != nil {
			return fmt.Errorf("validate workspace layout: %w", err)
		}
		// Confirm the repo_root is actually a git repo — catches /tmp,
		// typos, and non-git directories before spawn time, where the
		// error would surface as an opaque "git worktree add" failure.
		if _, err := git.RepoRoot(repoRoot); err != nil {
			return fmt.Errorf("--repo %q is not a git repository: %w", repoRoot, err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Insert the agents row (status='stopped' until the start daemon or
	// tier-3 routing spawns a runner).
	if _, err := tx.Exec(ctx, `
		INSERT INTO agents
			(id, tmux_session, tmux_window, role, status, runner_type,
			 cwd, window_name, started_at, last_seen, stop_requested,
			 workspace_scope, workspace_repo_root)
		VALUES ($1, 'maquinista', '', $2, 'stopped', $3, $4, $1, NOW(), NOW(), FALSE, $5, NULLIF($6, ''))
	`, id, agentAddRole, agentAddRunner, cwd, string(scope), repoRoot); err != nil {
		return fmt.Errorf("insert agent %s: %w", id, err)
	}

	// Handle (optional; validated by the DB partial-unique index).
	if agentAddHandle != "" {
		normalized := strings.ToLower(agentAddHandle)
		if _, err := tx.Exec(ctx, `UPDATE agents SET handle=$1 WHERE id=$2`, normalized, id); err != nil {
			return fmt.Errorf("set handle: %w", err)
		}
	}

	// agent_settings — optional persona / system_prompt.
	sysPrompt := ""
	if agentAddSystemPrompt != "" {
		b, err := os.ReadFile(agentAddSystemPrompt)
		if err != nil {
			return fmt.Errorf("read --system-prompt file: %w", err)
		}
		sysPrompt = string(b)
	}
	if sysPrompt != "" || agentAddPersona != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_settings (agent_id, persona, system_prompt)
			VALUES ($1, NULLIF($2, ''), NULLIF($3, ''))
			ON CONFLICT (agent_id) DO UPDATE SET
				persona       = COALESCE(EXCLUDED.persona, agent_settings.persona),
				system_prompt = COALESCE(EXCLUDED.system_prompt, agent_settings.system_prompt),
				updated_at    = NOW()
		`, id, agentAddPersona, sysPrompt); err != nil {
			return fmt.Errorf("agent_settings upsert: %w", err)
		}
	}

	// Soul: clone from template. Overrides from --persona if supplied (the
	// operator typically means "this is the agent's self-description").
	var overrides soul.Overrides
	if agentAddPersona != "" {
		overrides.CoreTruths = &agentAddPersona
	}
	if err := soul.CreateFromTemplate(ctx, tx, id, agentAddSoulTemplate, overrides); err != nil {
		return fmt.Errorf("soul create: %w", err)
	}

	// Phase 6 of plans/active/workspace-scopes.md: every agent gets a
	// default workspace row at creation so the "workspaces as children"
	// invariant holds for new agents too (migration 028 backfilled
	// pre-existing rows). For scope=shared this is just a cwd pointer;
	// for scope=agent it carries the resolved worktree_dir + branch.
	wsID := id + "@default"
	if err := insertWorkspaceRow(ctx, tx, wsID, id, scope, repoRoot); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET active_workspace_id=$1 WHERE id=$2`, wsID, id); err != nil {
		return fmt.Errorf("activate default workspace: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("Added agent %s (runner=%s, cwd=%s, scope=%s, workspace=%s)\n",
		id, agentAddRunner, cwd, scope, wsID)
	return nil
}

// insertWorkspaceRow inserts a canonical agent_workspaces row given a
// resolved scope + repo root. For scope=shared, worktree_dir and branch
// are left NULL. For scope=agent/task, the worktree path and branch
// name come from ResolveLayout so the SQL backfill in migration 028
// and the runtime formula stay in lockstep.
func insertWorkspaceRow(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, scope agent.WorkspaceScope, repoRoot string) error {
	var worktreeDir, branch *string
	if scope != agent.ScopeShared {
		layout, err := agent.ResolveLayout(scope, repoRoot, agentID, "")
		if err != nil {
			return fmt.Errorf("resolve layout for workspace: %w", err)
		}
		worktreeDir = &layout.WorktreeDir
		branch = &layout.Branch
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root, worktree_dir, branch)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, workspaceID, agentID, string(scope), repoRoot, worktreeDir, branch); err != nil {
		return fmt.Errorf("insert agent_workspaces: %w", err)
	}
	return nil
}

func runAgentArchive(id string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tag, err := pool.Exec(ctx, `
		UPDATE agents SET status='archived', stop_requested=TRUE, last_seen=NOW()
		WHERE id=$1
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no such agent: %s", id)
	}
	fmt.Printf("Archived %s\n", id)
	return nil
}

func runAgentKill(id string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tag, err := pool.Exec(ctx, `
		UPDATE agents SET stop_requested=TRUE, last_seen=NOW()
		WHERE id=$1
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no such agent: %s", id)
	}
	fmt.Printf("Kill requested for %s — the agent_stop NOTIFY will signal the sidecar\n", id)
	return nil
}

func runAgentEdit(id string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Verify the agent exists first.
	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM agents WHERE id=$1`, id).Scan(&role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no such agent: %s", id)
		}
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if agentAddRunner != "" {
		if _, err := tx.Exec(ctx, `UPDATE agents SET runner_type=$1 WHERE id=$2`, agentAddRunner, id); err != nil {
			return fmt.Errorf("set runner_type: %w", err)
		}
	}
	if agentAddCWD != "" {
		if _, err := tx.Exec(ctx, `UPDATE agents SET cwd=$1 WHERE id=$2`, agentAddCWD, id); err != nil {
			return fmt.Errorf("set cwd: %w", err)
		}
	}
	if agentAddScope != "" {
		scope := agent.WorkspaceScope(agentAddScope)
		if err := scope.Validate(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agents SET workspace_scope=$1 WHERE id=$2`, string(scope), id); err != nil {
			return fmt.Errorf("set workspace_scope: %w", err)
		}
	}
	if agentAddRepoRoot != "" {
		if _, err := tx.Exec(ctx, `UPDATE agents SET workspace_repo_root=$1 WHERE id=$2`, agentAddRepoRoot, id); err != nil {
			return fmt.Errorf("set workspace_repo_root: %w", err)
		}
	}
	if agentAddSystemPrompt != "" {
		b, err := os.ReadFile(agentAddSystemPrompt)
		if err != nil {
			return fmt.Errorf("read --system-prompt file: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_settings (agent_id, system_prompt)
			VALUES ($1, $2)
			ON CONFLICT (agent_id) DO UPDATE SET
				system_prompt = EXCLUDED.system_prompt,
				updated_at    = NOW()
		`, id, string(b)); err != nil {
			return fmt.Errorf("agent_settings.system_prompt: %w", err)
		}
	}
	if agentAddPersona != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_settings (agent_id, persona)
			VALUES ($1, $2)
			ON CONFLICT (agent_id) DO UPDATE SET
				persona    = EXCLUDED.persona,
				updated_at = NOW()
		`, id, agentAddPersona); err != nil {
			return fmt.Errorf("agent_settings.persona: %w", err)
		}
		// Mirror into soul.core_truths so the rendered system prompt
		// actually reflects the new persona.
		if s, err := soul.Load(ctx, tx, id); err == nil {
			s.CoreTruths = agentAddPersona
			if err := soul.Upsert(ctx, tx, *s); err != nil {
				return fmt.Errorf("soul.Upsert: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("Edited %s\n", id)
	return nil
}
