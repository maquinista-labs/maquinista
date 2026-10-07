package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// seedParkedTask inserts a pending_approval task carrying the residue of its
// failed episode: a stale claim and the worktree/branch a requeue must
// preserve.
func seedParkedTask(t *testing.T, pool *pgxpool.Pool, taskID, claimedBy, worktree string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO tasks (id, title, status, claimed_by, worktree_path)
		VALUES ($1, 'task ' || $1::text, 'pending_approval', NULLIF($2, ''), NULLIF($3, ''))
	`, taskID, claimedBy, worktree); err != nil {
		t.Fatalf("seed task %s: %v", taskID, err)
	}
}

// TestRequeueTask_FromPendingApproval: the sanctioned park escape flips
// pending_approval → ready, preserves worktree/branch so the next round
// updates the same PR, releases the stale claim, and stamps the journal
// (MAQ-40).
func TestRequeueTask_FromPendingApproval(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	seedParkedTask(t, pool, "rq1", "reviewer-rq1-r2", "/tmp/wt-rq1")

	if err := RequeueTask(pool, "rq1", "otavio"); err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}

	var status, worktree, claimedBy *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, worktree_path, claimed_by FROM tasks WHERE id='rq1'`).
		Scan(&status, &worktree, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if status == nil || *status != "ready" {
		t.Errorf("status = %v, want ready", status)
	}
	if worktree == nil || *worktree != "/tmp/wt-rq1" {
		t.Errorf("worktree_path = %v, want preserved (/tmp/wt-rq1)", worktree)
	}
	if claimedBy != nil {
		t.Errorf("claimed_by = %v, want NULL (claim released)", *claimedBy)
	}

	var agentID, content string
	if err := pool.QueryRow(context.Background(), `
		SELECT agent_id, content FROM task_context
		WHERE task_id='rq1' AND kind='observation'
		ORDER BY id DESC LIMIT 1
	`).Scan(&agentID, &content); err != nil {
		t.Fatalf("requeue journal row: %v", err)
	}
	if agentID != "otavio" {
		t.Errorf("journal agent_id = %q, want otavio (who)", agentID)
	}
	if !strings.Contains(content, "requeued to ready by otavio") {
		t.Errorf("journal content = %q, want it to name the requeue", content)
	}
}

// TestRequeueTask_RejectsOtherStatuses: requeue only undoes the park —
// every other status is an error, exactly like ApproveTask/RejectTask.
func TestRequeueTask_RejectsOtherStatuses(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	for _, status := range []string{"ready", "claimed", "review", "ready_to_merge", "done", "failed"} {
		taskID := "rq-" + status
		seedParkedTask(t, pool, taskID, "", "")
		if _, err := pool.Exec(context.Background(),
			`UPDATE tasks SET status=$1 WHERE id=$2`, status, taskID); err != nil {
			t.Fatalf("seed status %s: %v", status, err)
		}
		if err := RequeueTask(pool, taskID, "cli"); err == nil {
			t.Errorf("status %s: RequeueTask succeeded, want error", status)
		}
	}
}

// TestRequeueTask_ApproveFieldsUntouched: requeue is not an approval — it
// must not stamp approved_by (that would misreport who the human release
// authority was; only the approve verb does that).
func TestRequeueTask_ApproveFieldsUntouched(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	seedParkedTask(t, pool, "rq2", "", "")

	if err := RequeueTask(pool, "rq2", "cli"); err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	var approvedBy *string
	if err := pool.QueryRow(context.Background(),
		`SELECT approved_by FROM tasks WHERE id='rq2'`).Scan(&approvedBy); err != nil {
		t.Fatal(err)
	}
	if approvedBy != nil {
		t.Errorf("approved_by = %q, want NULL (requeue is not an approval)", *approvedBy)
	}
}
