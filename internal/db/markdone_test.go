package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// seedClaimedTask inserts a claimed task (with optional pipeline metadata)
// and its claiming agent — the precondition MarkDone's WHERE guards on.
func seedClaimedTask(t *testing.T, pool *pgxpool.Pool, taskID, agentID, metadataJSON string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO tasks (id, title, status, claimed_by, metadata)
		VALUES ($1, $4, 'claimed', $2, $3::jsonb)
	`, taskID, agentID, metadataJSON, "task "+taskID); err != nil {
		t.Fatalf("seed task %s: %v", taskID, err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO agents (id, tmux_session, tmux_window, status, role, runner_type)
		VALUES ($1, 'sess', $1, 'working', 'implementor', 'pi')
	`, agentID); err != nil {
		t.Fatalf("seed agent %s: %v", agentID, err)
	}
}

func taskStatus(t *testing.T, pool *pgxpool.Pool, taskID string) (status string, doneAt any, claimedBy any) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT status, done_at, claimed_by FROM tasks WHERE id = $1
	`, taskID).Scan(&status, &doneAt, &claimedBy)
	if err != nil {
		t.Fatalf("task row %s: %v", taskID, err)
	}
	return status, doneAt, claimedBy
}

// TestMarkDone_PipelineTaskGoesToReview pins the EX-03 done-path branch
// (ADR-0005): a pipeline task (metadata ticket_issue_id) completes into
// 'review', not 'done' — the review-dispatch loop takes over from there.
func TestMarkDone_PipelineTaskGoesToReview(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	seedClaimedTask(t, pool, "pipe-1", "agent-1", `{"ticket_issue_id":"uuid-77"}`)

	if err := MarkDone(pool, "pipe-1", "agent-1", "did the thing"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	status, doneAt, claimedBy := taskStatus(t, pool, "pipe-1")
	if status != "review" {
		t.Fatalf("pipeline task status = %q, want review", status)
	}
	if doneAt == nil {
		t.Fatal("done_at not stamped")
	}
	if claimedBy != nil {
		t.Fatalf("claimed_by = %v, want NULL", claimedBy)
	}

	// The result context row (dispatch's zero-author input) is present.
	var agentID string
	if err := pool.QueryRow(context.Background(), `
		SELECT agent_id FROM task_context
		WHERE task_id = 'pipe-1' AND kind = 'result'
		ORDER BY created_at DESC LIMIT 1
	`).Scan(&agentID); err != nil {
		t.Fatalf("result row: %v", err)
	}
	if agentID != "agent-1" {
		t.Fatalf("result author = %q, want agent-1", agentID)
	}
}

// TestMarkDone_PlainTaskStillDone proves the branch is metadata-gated:
// non-pipeline tasks complete exactly as before (EX-03 regression guard).
func TestMarkDone_PlainTaskStillDone(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	seedClaimedTask(t, pool, "plain-1", "agent-2", `{"other":"stuff"}`)

	if err := MarkDone(pool, "plain-1", "agent-2", "did the thing"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	status, _, _ := taskStatus(t, pool, "plain-1")
	if status != "done" {
		t.Fatalf("plain task status = %q, want done", status)
	}
}
