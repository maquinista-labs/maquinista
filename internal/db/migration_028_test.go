package db

import (
	"context"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// TestMigration028_WorkspaceSwitchTrigger verifies the trigger from
// migration 028 mirrors agent_workspaces.{scope,repo_root} into
// agents.{workspace_scope,workspace_repo_root} whenever
// active_workspace_id changes. This is the hot-path contract the
// reconcile loop relies on to avoid joining on every pass.
func TestMigration028_WorkspaceSwitchTrigger(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	// Seed an agent + two workspaces.
	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window, status) VALUES ('alice','s','w','stopped')`)
	mustExec(t, pool, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root)
		VALUES ('alice@a','alice','shared','/a')
	`)
	mustExec(t, pool, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root, worktree_dir, branch)
		VALUES ('alice@b','alice','agent','/b','/b/.maquinista/worktrees/agent/alice','maquinista/agent/alice')
	`)

	// Activate A — trigger should mirror shared+/a into the cache columns.
	mustExec(t, pool, `UPDATE agents SET active_workspace_id='alice@a' WHERE id='alice'`)
	var scope, repo string
	if err := pool.QueryRow(ctx,
		`SELECT workspace_scope, COALESCE(workspace_repo_root,'') FROM agents WHERE id='alice'`,
	).Scan(&scope, &repo); err != nil {
		t.Fatal(err)
	}
	if scope != "shared" || repo != "/a" {
		t.Errorf("after activating alice@a: scope=%q repo=%q; want shared / /a", scope, repo)
	}

	// Switch to B — cache must follow.
	mustExec(t, pool, `UPDATE agents SET active_workspace_id='alice@b' WHERE id='alice'`)
	if err := pool.QueryRow(ctx,
		`SELECT workspace_scope, COALESCE(workspace_repo_root,'') FROM agents WHERE id='alice'`,
	).Scan(&scope, &repo); err != nil {
		t.Fatal(err)
	}
	if scope != "agent" || repo != "/b" {
		t.Errorf("after switching to alice@b: scope=%q repo=%q; want agent / /b", scope, repo)
	}
}

// TestMigration028_ConstraintsRejectInvalidShapes enforces the two
// CHECK constraints: scope must be in the enum; scope=shared must
// leave worktree_dir / branch NULL, scope=agent/task must populate them.
func TestMigration028_ConstraintsRejectInvalidShapes(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window, status) VALUES ('alice','s','w','stopped')`)

	// Bad scope → rejected.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root) VALUES ('x','alice','weird','/x')
	`); err == nil {
		t.Error("bad scope should be rejected")
	}

	// scope=shared with worktree → rejected.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root, worktree_dir, branch)
		VALUES ('x','alice','shared','/x','/x/wt','m/x')
	`); err == nil {
		t.Error("scope=shared with worktree_dir should be rejected")
	}

	// scope=agent without worktree → rejected.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root) VALUES ('x','alice','agent','/x')
	`); err == nil {
		t.Error("scope=agent without worktree_dir should be rejected")
	}

	// scope=agent with worktree → accepted.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root, worktree_dir, branch)
		VALUES ('ok','alice','agent','/x','/x/wt','m/x')
	`); err != nil {
		t.Errorf("scope=agent with worktree_dir should be accepted: %v", err)
	}
}

// TestMigration028_CascadeDelete: deleting an agent cascades to its
// workspaces. FK is ON DELETE CASCADE on agent_workspaces.agent_id.
func TestMigration028_CascadeDelete(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window, status) VALUES ('alice','s','w','stopped')`)
	mustExec(t, pool, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root) VALUES ('alice@a','alice','shared','/a')
	`)
	// active_workspace_id references the workspace; must clear the FK
	// from the agent side before deleting the agent (unless we drop the
	// constraint). Here we just let DELETE cascade via the workspace
	// side.
	mustExec(t, pool, `UPDATE agents SET active_workspace_id='alice@a' WHERE id='alice'`)

	// Deleting the agent cascades to agent_workspaces; active FK is
	// declared non-cascading so the delete has to null it first, which
	// the referential-integrity check on the active pointer enforces.
	// Simulate the operator's flow: clear active, then delete.
	mustExec(t, pool, `UPDATE agents SET active_workspace_id=NULL WHERE id='alice'`)
	mustExec(t, pool, `DELETE FROM agents WHERE id='alice'`)

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_workspaces WHERE agent_id='alice'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("cascade delete left %d workspaces for alice", n)
	}
}
