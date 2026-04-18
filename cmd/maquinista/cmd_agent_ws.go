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
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/spf13/cobra"
)

// Phase 6 of plans/active/workspace-scopes.md: "maquinista agent ws"
// subcommands let an operator hold N workspaces per agent and switch
// between them. Each workspace is a first-class row in
// agent_workspaces keyed by "<agent>@<label>" — switching is just a
// one-statement update of agents.active_workspace_id, which the
// migration-028 trigger mirrors into the denormalized cache columns.

var agentWsCmd = &cobra.Command{
	Use:   "ws",
	Short: "Manage per-agent workspaces (list / new / switch / archive)",
}

var agentWsListCmd = &cobra.Command{
	Use:   "list <agent-id>",
	Short: "List an agent's workspaces with the active one starred",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentWsList(args[0])
	},
}

var (
	wsNewScope string
	wsNewRepo  string
)

var agentWsNewCmd = &cobra.Command{
	Use:   "new <agent-id> <label>",
	Short: "Create a new workspace and activate it",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentWsNew(args[0], args[1])
	},
}

var agentWsSwitchCmd = &cobra.Command{
	Use:   "switch <agent-id> <label>",
	Short: "Activate an existing workspace",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentWsSwitch(args[0], args[1])
	},
}

var agentWsArchiveCmd = &cobra.Command{
	Use:   "archive <agent-id> <label>",
	Short: "Archive a workspace (soft delete; worktree stays on disk)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentWsArchive(args[0], args[1])
	},
}

func init() {
	agentWsNewCmd.Flags().StringVar(&wsNewScope, "scope", "agent", "workspace scope: shared | agent | task")
	agentWsNewCmd.Flags().StringVar(&wsNewRepo, "repo", "", "project git repo root (required for --scope=agent or --scope=task)")

	agentWsCmd.AddCommand(agentWsListCmd, agentWsNewCmd, agentWsSwitchCmd, agentWsArchiveCmd)
	agentCmd.AddCommand(agentWsCmd)
}

func runAgentWsList(agentID string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var active *string
	if err := pool.QueryRow(ctx, `SELECT active_workspace_id FROM agents WHERE id=$1`, agentID).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no such agent: %s", agentID)
		}
		return err
	}

	rows, err := pool.Query(ctx, `
		SELECT id, scope, repo_root, COALESCE(worktree_dir,''), COALESCE(branch,''), created_at, archived_at
		FROM agent_workspaces
		WHERE agent_id=$1 AND archived_at IS NULL
		ORDER BY created_at ASC
	`, agentID)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Fprintf(os.Stdout, "%-3s %-30s %-8s %-40s %s\n", "", "WORKSPACE", "SCOPE", "REPO", "BRANCH")
	for rows.Next() {
		var id, scope, repo, worktree, branch string
		var createdAt time.Time
		var archivedAt *time.Time
		if err := rows.Scan(&id, &scope, &repo, &worktree, &branch, &createdAt, &archivedAt); err != nil {
			return err
		}
		marker := "  "
		if active != nil && *active == id {
			marker = "★ "
		}
		fmt.Fprintf(os.Stdout, "%-3s %-30s %-8s %-40s %s\n", marker, id, scope, repo, branch)
	}
	return rows.Err()
}

func runAgentWsNew(agentID, label string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Agent must exist.
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE id=$1)`, agentID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no such agent: %s", agentID)
	}

	scope := agent.WorkspaceScope(wsNewScope)
	if err := scope.Validate(); err != nil {
		return err
	}
	repo := wsNewRepo
	if scope != agent.ScopeShared {
		if repo == "" {
			return fmt.Errorf("--scope=%s requires --repo", scope)
		}
		if _, err := git.RepoRoot(repo); err != nil {
			return fmt.Errorf("--repo %q is not a git repository: %w", repo, err)
		}
	} else if repo == "" {
		if cwd, err := os.Getwd(); err == nil {
			repo = cwd
		}
	}

	// Build the workspace row. For scope=agent/task the path + branch
	// math lives in ResolveLayout (matches the SQL backfill in
	// migration 028 and the runtime formula used by reconcile).
	wsID := agentID + "@" + label
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := insertWorkspaceRow(ctx, tx, wsID, agentID, scope, repo); err != nil {
		// Surface a friendlier error when the label collides.
		if err != nil && errorIsUniqueViolation(err) {
			return fmt.Errorf("workspace %q already exists for %s", wsID, agentID)
		}
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET active_workspace_id=$1 WHERE id=$2`, wsID, agentID); err != nil {
		return fmt.Errorf("activate new workspace: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	fmt.Printf("Created + activated workspace %s (scope=%s, repo=%s)\n", wsID, scope, repo)
	return nil
}

func runAgentWsSwitch(agentID, label string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsID := agentID + "@" + label

	// Target workspace must exist, belong to this agent, and not be archived.
	var ownerID string
	var archivedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT agent_id, archived_at FROM agent_workspaces WHERE id=$1
	`, wsID).Scan(&ownerID, &archivedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no such workspace: %s", wsID)
		}
		return err
	}
	if ownerID != agentID {
		return fmt.Errorf("workspace %s belongs to %s, not %s", wsID, ownerID, agentID)
	}
	if archivedAt != nil {
		return fmt.Errorf("workspace %s is archived; unarchive it first", wsID)
	}

	tag, err := pool.Exec(ctx, `UPDATE agents SET active_workspace_id=$1 WHERE id=$2`, wsID, agentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no such agent: %s", agentID)
	}
	fmt.Printf("Switched %s to workspace %s\n", agentID, wsID)
	fmt.Println("  (restart the agent's pane or wait for the next reconcile tick to see the new cwd)")
	return nil
}

func runAgentWsArchive(agentID, label string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsID := agentID + "@" + label

	// Refuse to archive the currently active workspace — the operator
	// should switch to another one first, otherwise the agent's reconcile
	// loop would have no valid layout to resolve.
	var active *string
	if err := pool.QueryRow(ctx, `SELECT active_workspace_id FROM agents WHERE id=$1`, agentID).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no such agent: %s", agentID)
		}
		return err
	}
	if active != nil && *active == wsID {
		return fmt.Errorf("cannot archive %s: it's the active workspace — switch first", wsID)
	}

	tag, err := pool.Exec(ctx, `
		UPDATE agent_workspaces
		SET archived_at = NOW()
		WHERE id=$1 AND agent_id=$2 AND archived_at IS NULL
	`, wsID, agentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no such (or already-archived) workspace: %s", wsID)
	}
	fmt.Printf("Archived workspace %s (worktree left on disk)\n", wsID)
	return nil
}

// errorIsUniqueViolation matches the duplicate-key / unique-constraint
// messages Postgres surfaces when a workspace label collides. Narrow
// by design — only used to turn a PK collision on agent_workspaces
// into a friendly error string.
func errorIsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint")
}
