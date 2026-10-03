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

// Config bundles scheduler knobs.
type Config struct {
	PollInterval time.Duration
	EnsureAgent  EnsureAgentFn
}

// liveAgentStatusSQL is the set of agents.status values that mean "a pane
// is actually working this task". `stopped` is deliberately excluded
// (MAQ-18): SpawnFresh pre-registers the row as 'stopped' and only flips
// it to 'running' once the tmux window exists (agentspawn.SpawnFresh),
// the sidecar marks rows 'stopped' when a window vanishes mid-drive
// (sidecar.Manager), and `maquinista stop` parks rows that way — in all
// three the pane is gone, so the row must never wedge a task. The
// dashboard "stopped + empty tmux_window = needs provisioning" state is
// reachable only for role='user' + task_id IS NULL rows
// (reconcileAgentPanes), so it cannot collide with the task-scoped rows
// this package reasons about.
const liveAgentStatusSQL = `'running','idle','working','spawning'`

// staleClaimBoundSQL is how old a 'claimed' task must be — with no live
// task-scoped agent row — before ReapStaleClaims releases it back to
// 'ready'. The bound is what keeps the reaper away from fresh claims:
// DispatchOne commits 'claimed' BEFORE EnsureAgent inserts the agent row,
// and that row itself starts life as 'stopped' for up to ~15s
// (SpawnFresh's ready-wait). Both windows are far shorter than this.
const staleClaimBoundSQL = `INTERVAL '5 minutes'`

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
		// Self-heal: a claimed task whose agent has no inbox row (spawn
		// raced the enqueue, mailbox dropped it, crash mid-dispatch) would
		// otherwise sit idle forever. Re-enqueue on each wake.
		healed, herr := HealMissingInbox(ctx, pool)
		if herr != nil {
			log.Printf("taskscheduler: heal missing inbox: %v", herr)
		} else if healed > 0 {
			log.Printf("taskscheduler: healed %d task(s) with missing inbox prompt", healed)
		}
		// Reaper: a claimed task whose agent row died mid-flight (pane
		// vanished, daemon restart raced the spawn) is released back to
		// 'ready' once the claim is stale — otherwise nothing will ever
		// pick it up again.
		reaped, rerr := ReapStaleClaims(ctx, pool)
		if rerr != nil {
			log.Printf("taskscheduler: reap stale claims: %v", rerr)
		} else if reaped > 0 {
			log.Printf("taskscheduler: reaped %d stale claim(s) back to ready", reaped)
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
			// Nothing claimable this pass. If ready tasks were skipped
			// because a live agent still holds them, journal why (MAQ-18:
			// no silent skips).
			if _, err := LogBlockedReadyTasks(ctx, pool); err != nil {
				log.Printf("taskscheduler: blocked-task report: %v", err)
			}
			return nil
		}
	}
}

// LogBlockedReadyTasks journals every 'ready' task that is being skipped
// because a live agent row still holds it, one log line per blocking
// agent. Returns the number of blocked tasks reported. This is the
// visible counterpart of the claim filter: before MAQ-18 a 'stopped'
// agent row silenced a task with zero trace in the logs.
func LogBlockedReadyTasks(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, a.id, a.status
		FROM tasks t
		JOIN agents a
		  ON a.task_id = t.id
		 AND a.status IN (`+liveAgentStatusSQL+`)
		WHERE t.status = 'ready'
		ORDER BY t.priority DESC, t.created_at, a.id
	`)
	if err != nil {
		return 0, fmt.Errorf("query blocked ready tasks: %w", err)
	}
	defer rows.Close()
	type blocked struct{ taskID, agentID, status string }
	var found []blocked
	for rows.Next() {
		var b blocked
		if err := rows.Scan(&b.taskID, &b.agentID, &b.status); err != nil {
			return 0, err
		}
		found = append(found, b)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, b := range found {
		log.Printf("taskscheduler: ready task %s skipped: held by live agent %s (status=%s)",
			b.taskID, b.agentID, b.status)
	}
	return len(found), nil
}

// ReapStaleClaims releases 'claimed' tasks whose task-scoped agent rows
// are ALL non-live (stopped/archived/dead) and whose claim is older than
// staleClaimBoundSQL — the mid-flight death case: implementor pane
// vanishes, the sidecar parks the row at 'stopped', and the task would
// otherwise sit 'claimed' forever with nobody working it. The claimed_at
// bound keeps the reaper away from fresh claims, whose agent row does not
// exist yet (claimed is committed before EnsureAgent) or is still in
// SpawnFresh's 'stopped' pre-registration phase. Agent rows themselves
// are left alone: DispatchOne's claim-TX release handles them when the
// task is re-claimed.
func ReapStaleClaims(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx, `
		UPDATE tasks t
		SET status = 'ready', claimed_by = NULL, claimed_at = NULL
		WHERE t.status = 'claimed'
		  AND t.claimed_at < NOW() - `+staleClaimBoundSQL+`
		  AND NOT EXISTS (
		        SELECT 1 FROM agents a
		        WHERE a.task_id = t.id AND a.status IN (`+liveAgentStatusSQL+`)
		      )
		RETURNING t.id
	`)
	if err != nil {
		return 0, fmt.Errorf("reap stale claims: %w", err)
	}
	defer rows.Close()
	var reaped []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		reaped = append(reaped, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range reaped {
		// MAQ-22: the guarded UPDATE above flips the row exactly once per
		// release, so the requeue-after-heal one-liner is exactly-once too.
		pipeline.Notifyf(ctx, pool, "🔄 %s: requeued to ready — stale claim healed (agent died mid-flight).",
			pipeline.TaskTitle(ctx, pool, id))
	}
	return len(reaped), nil
}

// DispatchOne claims one ready task that has no live agent and routes it.
// A task whose only agent rows are 'stopped'/'archived'/'dead' IS
// claimable — those rows are released (flipped to 'dead') atomically with
// the claim so the uq_agents_task_live slot is free for the fresh spawn.
// Returns (true, nil) on dispatch, (false, nil) when nothing is ready.
func DispatchOne(ctx context.Context, pool *pgxpool.Pool, cfg Config) (bool, error) {
	var taskID string
	var roleFromMeta *string

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		SELECT id, metadata->>'role'
		FROM tasks t
		WHERE status = 'ready'
		  AND NOT EXISTS (
		        SELECT 1 FROM agents a
		        WHERE a.task_id = t.id AND a.status IN (` + liveAgentStatusSQL + `)
		      )
		ORDER BY priority DESC, created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&taskID, &roleFromMeta)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim task: %w", err)
	}

	// Release stale agent rows from previous attempts. A 'stopped' or
	// 'archived' row still occupies the uq_agents_task_live slot, so
	// without this EnsureAgent's fresh insert would bounce off the index
	// and the prompt would be enqueued to a pane that no longer exists.
	// Running inside the claim TX keeps release + claim atomic.
	if _, err := tx.Exec(ctx, `
		UPDATE agents SET status = 'dead', last_seen = NOW()
		WHERE task_id = $1 AND status IN ('stopped','archived')
	`, taskID); err != nil {
		return false, fmt.Errorf("release stale agents: %w", err)
	}
	role := "implementor"
	if roleFromMeta != nil && *roleFromMeta != "" {
		role = *roleFromMeta
	}

	// Flip the task state BEFORE committing the claim TX so concurrent
	// schedulers see it as 'claimed' immediately. EnsureAgent runs after
	// the commit — if it fails, the task is reverted to 'ready' below,
	// and ReapStaleClaims covers the crash-in-between case.
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
	// MAQ-22: the claim announces itself. The exactly-once guard is the
	// guarded claim above (FOR UPDATE SKIP LOCKED + 'ready'→'claimed'
	// flip): one winner per claim, one note. EnsureAgent returns a
	// non-empty id on both outcomes — a freshly minted pane, or the
	// already-live pane (ErrAgentAlreadyLive) that a racing spawn created
	// and this claim then routes a fresh /work-on-task to — so both paths
	// are genuine claims and both announce; the empty check below is
	// defensiveness against a future EnsureAgent change, not the guard.
	if agentID != "" {
		pipeline.Notifyf(ctx, pool, "📋 %s: claimed by @%s (%s) — /work-on-task dispatched.",
			pipeline.TaskTitle(ctx, pool, taskID), agentID, role)
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
