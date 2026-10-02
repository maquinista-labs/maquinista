package taskscheduler

// MAQ-13 backstop: tasks that are 'claimed' with no worktree_path and no
// live agent can never spawn — the scheduler only claims 'ready' rows, so
// such a task would sit wedged silently forever (it no longer even loops:
// DispatchOne parks ready tasks like that at claim time). This pass catches
// the survivors — legacy wedged rows from before the fix, and tasks that
// lost their agent between claim and ensure — by parking them needs-human
// past a grace period, exactly once, with a single Pipeline-topic ping.

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
)

// ParkGraceEnv is the operator knob for how long an unspawnable claimed task
// may sit before it is parked needs-human.
const ParkGraceEnv = "MAQUINISTA_WORKTREE_GRACE"

// DefaultParkGrace matches the incident shape: a task that stays unspawnable
// for 10 minutes is not about to fix itself.
const DefaultParkGrace = 10 * time.Minute

// ParkUnspawnable parks every claimed task that has no worktree_path, no
// live agent, and has been claimed longer than grace. Returns the number of
// tasks parked. The guarded transition (status='claimed' → 'pending_approval'
// in one tx with the note) makes each park exactly-once.
func ParkUnspawnable(ctx context.Context, pool *pgxpool.Pool, grace time.Duration) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id
		FROM tasks t
		WHERE t.status = 'claimed'
		  AND (t.worktree_path IS NULL OR t.worktree_path = '')
		  AND t.claimed_at IS NOT NULL
		  AND t.claimed_at < NOW() - $1::interval
		  AND NOT EXISTS (
		        SELECT 1 FROM agents a
		        WHERE a.task_id = t.id AND a.status <> 'dead'
		      )
	`, grace)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		taskIDs = append(taskIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	parked := 0
	for _, taskID := range taskIDs {
		applied, err := parkNoWorktree(ctx, pool, taskID)
		if err != nil {
			log.Printf("taskscheduler: park %s: %v", taskID, err)
			continue
		}
		if !applied {
			continue // raced to another status — leave it alone
		}
		log.Printf("taskscheduler: task %s parked needs-human: claimed >%s with no worktree_path and no live agent", taskID, grace)
		pipeline.Notifyf(ctx, pool, "🆘 %s: claimed %s ago with no worktree_path and no live agent — unspawnable, parked needs-human. Provision the worktree, set tasks.worktree_path, flip the task back to 'ready'.",
			pipeline.TaskTitle(ctx, pool, taskID), grace)
		parked++
	}
	return parked, nil
}

// parkNoWorktree flips the task to pending_approval (guarded on its current
// status) and records the park note — one tx. applied=false when the row
// raced to another status.
func parkNoWorktree(ctx context.Context, pool *pgxpool.Pool, taskID string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE tasks SET status='pending_approval' WHERE id=$1 AND status='claimed'`, taskID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_context (task_id, kind, content) VALUES ($1, 'verdict', $2)`,
		taskID, parkNoWorktreeNote); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
