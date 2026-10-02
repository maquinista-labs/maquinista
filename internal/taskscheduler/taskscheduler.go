// Package taskscheduler implements §D.4: drains the ready-tasks queue,
// ensures a per-task implementor agent exists, enqueues its /work-on-task
// inbox row, and flips the task to 'claimed'. Designed for multiple
// replicas — FOR UPDATE SKIP LOCKED + uq_agents_task_live keep each task
// dispatched exactly once.
package taskscheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
	"github.com/maquinista-labs/maquinista/internal/orchestrator"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
)

// EnsureAgentFn is a thin adapter so the scheduler can be tested without
// importing the real orchestrator spawner chain. Production injects
// orchestrator.EnsureAgent via a closure that also supplies a Spawner.
type EnsureAgentFn func(ctx context.Context, role, taskID string) (agentID string, err error)

// parkNoWorktreeNote is the task_context verdict row recorded when a task is
// parked for having no worktree_path (DispatchOne claim-time park and the
// ParkUnspawnable backstop share it).
const parkNoWorktreeNote = "no worktree_path — implementor can never spawn (EnsureAgent refuses worktree-less tasks by design); parked needs-human"

// Config bundles scheduler knobs.
type Config struct {
	PollInterval time.Duration
	EnsureAgent  EnsureAgentFn
	// ParkGrace is how long a claimed task with no worktree_path and no
	// live agent sits before ParkUnspawnable parks it needs-human
	// (MAQ-13 backstop). <= 0 → default 10m, overridable via
	// MAQUINISTA_WORKTREE_GRACE.
	ParkGrace time.Duration
}

// Run drives the task-scheduler loop until ctx is cancelled.
//
// Wake triggers: LISTEN task_events (from migration 004) with a
// PollInterval ticker fallback. Each wake drains every eligible task.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if cfg.EnsureAgent == nil {
		return errors.New("taskscheduler: EnsureAgent required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.ParkGrace <= 0 {
		cfg.ParkGrace = DefaultParkGrace
		if v := os.Getenv(ParkGraceEnv); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				cfg.ParkGrace = d
			}
		}
	}

	listener, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN task_events"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	for {
		if err := drain(ctx, pool, cfg); err != nil {
			log.Printf("taskscheduler: %v", err)
		}
		// MAQ-13 backstop: claimed tasks with no worktree_path and no live
		// agent are unspawnable forever (the scheduler only claims 'ready'
		// rows, so nothing revisits them). Park past grace, exactly once.
		if parked, perr := ParkUnspawnable(ctx, pool, cfg.ParkGrace); perr != nil {
			log.Printf("taskscheduler: park unspawnable: %v", perr)
		} else if parked > 0 {
			log.Printf("taskscheduler: parked %d unspawnable claimed task(s) needs-human", parked)
		}
		// Self-heal: a claimed task whose agent has no inbox row (spawn
		// raced the enqueue, mailbox dropped it, crash mid-dispatch) would
		// otherwise sit idle forever. Re-enqueue on each wake.
		healed, herr := HealMissingInbox(ctx, pool)
		if herr != nil {
			log.Printf("taskscheduler: heal missing inbox: %v", herr)
		} else if healed > 0 {
			log.Printf("taskscheduler: healed %d task(s) with missing inbox prompt", healed)
		}
		waitCtx, cancel := context.WithTimeout(ctx, cfg.PollInterval)
		_, nerr := listener.Conn().WaitForNotification(waitCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if nerr != nil && !errors.Is(nerr, context.DeadlineExceeded) {
			log.Printf("taskscheduler: notify: %v", nerr)
		}
	}
}

func drain(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	for {
		dispatched, err := DispatchOne(ctx, pool, cfg)
		if err != nil {
			return err
		}
		if !dispatched {
			return nil
		}
	}
}

// DispatchOne claims one ready task that has no live agent and routes it.
// Returns (true, nil) on dispatch, (false, nil) when nothing is ready.
func DispatchOne(ctx context.Context, pool *pgxpool.Pool, cfg Config) (bool, error) {
	var taskID string
	var roleFromMeta *string
	var worktree *string

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		SELECT id, metadata->>'role', worktree_path
		FROM tasks t
		WHERE status = 'ready'
		  AND NOT EXISTS (
		        SELECT 1 FROM agents a
		        WHERE a.task_id = t.id AND a.status <> 'dead'
		      )
		ORDER BY priority DESC, created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&taskID, &roleFromMeta, &worktree)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim task: %w", err)
	}
	// Structural dead end (MAQ-13): no worktree_path means EnsureAgent can
	// never spawn (it refuses worktree-less tasks by design), so claiming
	// would only start a ready→claimed→ready log loop. Park needs-human
	// inside the claim tx instead — the guarded transition makes it exactly
	// once — and ping the Pipeline topic once.
	if worktree == nil || *worktree == "" {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status='pending_approval' WHERE id=$1`, taskID); err != nil {
			return false, fmt.Errorf("park no-worktree: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_context (task_id, kind, content) VALUES ($1, 'verdict', $2)`,
			taskID, parkNoWorktreeNote); err != nil {
			return false, fmt.Errorf("park no-worktree note: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit park: %w", err)
		}
		log.Printf("taskscheduler: task %s parked needs-human: no worktree_path (implementor can never spawn)", taskID)
		pipeline.Notifyf(ctx, pool, "🆘 %s: no worktree_path — cannot spawn an implementor. Parked needs-human. Provision the worktree, set tasks.worktree_path, flip the task back to 'ready'.",
			pipeline.TaskTitle(ctx, pool, taskID))
		return true, nil
	}

	role := "implementor"
	if roleFromMeta != nil && *roleFromMeta != "" {
		role = *roleFromMeta
	}

	// Flip the task state BEFORE committing the claim TX so concurrent
	// schedulers see it as 'claimed' immediately. EnsureAgent runs after
	// the commit — if it fails, a reaper (future work) flips back to
	// 'ready', but for now the failure surfaces and the partial unique
	// index releases the dead row naturally on the next tick.
	if _, err := tx.Exec(ctx, `
		UPDATE tasks
		SET status = 'claimed', claimed_at = NOW()
		WHERE id = $1
	`, taskID); err != nil {
		return false, fmt.Errorf("flip claimed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit claim: %w", err)
	}

	agentID, err := cfg.EnsureAgent(ctx, role, taskID)
	if err != nil && !errors.Is(err, orchestrator.ErrAgentAlreadyLive) {
		// Attempt to revert the task so it can be retried.
		_, _ = pool.Exec(ctx, `UPDATE tasks SET status='ready' WHERE id=$1 AND status='claimed'`, taskID)
		return true, fmt.Errorf("ensure_agent %s: %w", taskID, err)
	}

	// Enqueue the implementor's starting prompt + mark task.claimed_by.
	if err := enqueueWorkOnTask(ctx, pool, agentID, taskID); err != nil {
		return true, fmt.Errorf("enqueue inbox: %w", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET claimed_by=$2 WHERE id=$1`, taskID, "@"+agentID); err != nil {
		return true, fmt.Errorf("set claimed_by: %w", err)
	}
	return true, nil
}

// HealMissingInbox is the heal path from the 1.4-style "crash mid-dispatch"
// case: the task was already 'claimed' and the agent row exists, but no
// inbox row is present. A follow-up tick enqueues the missing message so
// nothing gets stuck.
func HealMissingInbox(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, a.id AS agent_id
		FROM tasks t
		JOIN agents a ON a.task_id = t.id AND a.status <> 'dead'
		WHERE t.status = 'claimed'
		  AND NOT EXISTS (
		        SELECT 1 FROM agent_inbox i
		        WHERE i.agent_id = a.id
		          AND i.origin_channel = 'task'
		          AND i.external_msg_id = 'task:' || t.id
		      )
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	healed := 0
	type pair struct{ taskID, agentID string }
	var pending []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.taskID, &p.agentID); err != nil {
			return healed, err
		}
		pending = append(pending, p)
	}
	for _, p := range pending {
		if err := enqueueWorkOnTask(ctx, pool, p.agentID, p.taskID); err != nil {
			return healed, err
		}
		healed++
	}
	return healed, nil
}

func enqueueWorkOnTask(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string) error {
	content, _ := json.Marshal(map[string]any{
		"type":    "task",
		"task_id": taskID,
		"prompt":  fmt.Sprintf("/work-on-task %s", taskID),
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Upsert, not enqueue: the (origin_channel, external_msg_id) key is
	// stable per task, so a previous attempt's processed row would make a
	// plain insert collapse to a no-op (ON CONFLICT DO NOTHING) and the new
	// agent would never receive its prompt. Repoint + reset for redelivery.
	if _, err := mailbox.UpsertInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "task-scheduler",
		OriginChannel: "task",
		ExternalMsgID: "task:" + taskID,
		Content:       content,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
