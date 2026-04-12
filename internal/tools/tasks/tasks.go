// Package tasks is the typed Go surface for task DAG mutations from
// Appendix D. Agents reach it through the maquinista CLI wrapper or MCP;
// the DB remains authoritative, so no caching here.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Task is the insert payload.
type Task struct {
	ID           string
	Title        string
	Body         string
	Priority     int
	ProjectID    *string
	Role         string // 'executor' | 'implementor' | 'planner' | …
	WorktreePath *string
}

// CreateTask inserts a task row. Role='implementor' requires WorktreePath
// since the implementor spawn depends on it (§D.3). Title cannot be empty.
func CreateTask(ctx context.Context, pool *pgxpool.Pool, t Task) error {
	if strings.TrimSpace(t.Title) == "" {
		return errors.New("title required")
	}
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("id required")
	}
	if t.Role == "implementor" && (t.WorktreePath == nil || *t.WorktreePath == "") {
		return errors.New("worktree_path required when role=implementor")
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO tasks (id, title, body, priority, project_id, worktree_path, metadata)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4, 0), 5), $5, $6, jsonb_build_object('role', $7::text))
	`, t.ID, t.Title, t.Body, t.Priority, t.ProjectID, t.WorktreePath, t.Role)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

// AddDep adds a dependency edge: `taskID` depends on `dependsOn`. The
// caller should invoke ValidateDAG after a batch of AddDep calls to catch
// cycles — we don't do it here because intermediate states during a large
// plan write may be temporarily acyclic-invalid.
func AddDep(ctx context.Context, pool *pgxpool.Pool, taskID, dependsOn string) error {
	if taskID == dependsOn {
		return errors.New("self-dependency")
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO task_deps (task_id, depends_on) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, taskID, dependsOn)
	return err
}

// ValidateDAG walks task_deps with WITH RECURSIVE and fails if any cycle
// exists. Returns nil on a clean DAG.
func ValidateDAG(ctx context.Context, pool *pgxpool.Pool) error {
	var cycle []string
	err := pool.QueryRow(ctx, `
		WITH RECURSIVE walk(id, path, cycle) AS (
			SELECT task_id, ARRAY[task_id, depends_on], task_id = depends_on
			  FROM task_deps
			UNION ALL
			SELECT d.task_id, w.path || d.depends_on,
			       d.depends_on = ANY(w.path)
			FROM task_deps d
			JOIN walk w ON w.path[array_upper(w.path,1)] = d.task_id
			WHERE NOT w.cycle
		)
		SELECT path FROM walk WHERE cycle LIMIT 1
	`).Scan(&cycle)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("validate dag: %w", err)
	}
	return fmt.Errorf("cycle detected: %s", strings.Join(cycle, " → "))
}

// SetPRUrl records the opened-PR URL and flips status/pr_state to 'review'/'open'.
func SetPRUrl(ctx context.Context, pool *pgxpool.Pool, taskID, url string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE tasks
		SET pr_url = $2, pr_state = 'open', status = 'review'
		WHERE id = $1
	`, taskID, url)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no task %q", taskID)
	}
	return nil
}

// MarkReview is a manual transition for tasks that should stop auto-claim
// (e.g., awaiting human review outside a PR).
func MarkReview(ctx context.Context, pool *pgxpool.Pool, taskID string) error {
	_, err := pool.Exec(ctx, `UPDATE tasks SET status='review' WHERE id=$1`, taskID)
	return err
}

// MarkMerged flips pr_state='merged', status='done'. The existing
// refresh_ready_tasks trigger (migration 001/003) fires on
// status='done' and promotes eligible dependents to 'ready'.
func MarkMerged(ctx context.Context, pool *pgxpool.Pool, taskID string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE tasks
		SET pr_state='merged', status='done', done_at=$2
		WHERE id=$1
	`, taskID, time.Now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no task %q", taskID)
	}
	return nil
}

// MarkClosed flips pr_state='closed', status='failed' — the PR was
// rejected/abandoned. Dependents stay 'pending'.
func MarkClosed(ctx context.Context, pool *pgxpool.Pool, taskID string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE tasks SET pr_state='closed', status='failed' WHERE id=$1
	`, taskID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no task %q", taskID)
	}
	return nil
}

