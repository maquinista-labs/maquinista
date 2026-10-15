// Dispatch-liveness belt (MAQ-54). The freeze watchdog retires silent
// agents and the ADR-0008 arms re-dispatch their work through state
// transitions — but every one of those transitions ENDS at a spawn leg
// owned by a different pass (fixerPass mints fixers, dispatchPass mints
// reviewers, the task scheduler claims 'ready' rows). A spawn leg that
// silently fails — a worktree that evaporated between transitions, a
// candidate query that no longer matches the task's shape, a scheduler
// that is down — leaves the task stranded in a state that EXPECTS a next
// agent row, with no live agent and nothing spawning, forever. That is the
// a00c5d33/MAQ-46 shape: 28 minutes of dead board until a human noticed.
//
// The belt is the alarm for exactly that: a pipeline task sitting in a
// spawn-expecting state ('ready', 'claimed', 'review', 'changes_requested')
// with NO live agent row and no spawn attempt since the last piece of leg
// activity, past the liveness bound, fires ONE needs-human note on the
// Pipeline topic. It is deliberately notify-only — the auto-recovery is the
// transitions' job (MAQ-54 made the silent-success re-arm one); the belt
// exists so a dropped dispatch is loud, not so the belt becomes a second
// dispatcher. 'ready_to_merge' and the terminal/parked states are excluded:
// waiting for an approve verb there is the designed resting state.
//
// Exactly-once per drop episode rides the same anchor-and-marker pattern as
// the park fan-out (claimParkFanout): the anchor is the newest non-liveness
// activity on the task (last agent-row start, last task_context write, or
// task creation — whichever is newest), the single-statement claim inserts
// a 'liveness' marker row only when no newer marker exists, and any fresh
// activity (a retire, a verdict, a re-arm — all write context rows; any
// spawn moves the agent anchor) re-arms the alarm for the NEXT drop. A
// repeat fire therefore requires the drop to recur after new activity,
// which is exactly the "once per dropped dispatch" contract.
//
// The bound is the freeze spawn grace (MAQUINISTA_WATCHDOG_SPAWN, 10m
// default) — the same window a newborn agent may legitimately take to boot.
// Demanding liveness faster than a spawn is allowed to be silent would
// alarm on healthy cold boots.
package pipeline

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// livenessKind is the task_context kind stamping liveness alert markers.
const livenessKind = "liveness"

// livenessScanSQL finds spawn-expecting pipeline tasks whose dispatch leg
// dropped, and claims the alert in the same statement (the marker INSERT is
// the exactly-once guard — only the winner returns the row). $1 = liveness
// bound in seconds, $2 = the marker kind. The anchor is the newest of: the
// task's last agent-row start, its last non-marker task_context write, and
// its creation — a dropped dispatch begins when activity stops.
const livenessScanSQL = `
WITH cand AS (
    SELECT t.id, t.status,
           GREATEST(
             COALESCE((SELECT MAX(a.started_at) FROM agents a
                       WHERE a.task_id = t.id), t.created_at),
             COALESCE((SELECT MAX(c.created_at) FROM task_context c
                       WHERE c.task_id = t.id AND c.kind <> $2), t.created_at)
           ) AS anchor
    FROM tasks t
    WHERE t.metadata->>'ticket_issue_id' IS NOT NULL
      AND t.status IN ('ready', 'claimed', 'review', 'changes_requested')
      AND NOT EXISTS (
            SELECT 1 FROM agents a
            WHERE a.task_id = t.id AND a.status <> 'dead')
), claim AS (
    INSERT INTO task_context (task_id, kind, content)
    SELECT c.id, $2,
           'dispatch-liveness: state ' || c.status || ' with no live agent and no spawn attempt since ' ||
           c.anchor::text || ' (liveness bound ' || make_interval(secs => $1)::text || ') — the expected dispatch leg never materialized'
    FROM cand c
    WHERE c.anchor < NOW() - make_interval(secs => $1)
      AND NOT EXISTS (
            SELECT 1 FROM task_context m
            WHERE m.task_id = c.id AND m.kind = $2 AND m.created_at > c.anchor)
    RETURNING task_id
)
SELECT cl.task_id, t.status
FROM claim cl JOIN tasks t ON t.id = cl.task_id`

// dispatchLivenessPass fires the belt: one needs-human note per dropped
// dispatch, exactly once per drop episode (the claim inside livenessScanSQL
// dedups; a raced second pass computes the same anchor and inserts
// nothing). Runs every dispatch tick, after the watchdog pass — the retire
// arms' transitions land first, so the belt only speaks about drops they
// could not heal.
func dispatchLivenessPass(ctx context.Context, pool *pgxpool.Pool, bound time.Duration) error {
	rows, err := pool.Query(ctx, livenessScanSQL, bound.Seconds(), livenessKind)
	if err != nil {
		return err
	}
	type dropped struct{ taskID, status string }
	var drops []dropped
	for rows.Next() {
		var d dropped
		if err := rows.Scan(&d.taskID, &d.status); err != nil {
			rows.Close()
			return err
		}
		drops = append(drops, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range drops {
		log.Printf("pipeline: dispatch: liveness: %s stuck in %q with no live agent past the %s bound — needs-human note sent", d.taskID, d.status, bound)
		notifyTaskf(ctx, pool, d.taskID,
			"🆘 %s: a dispatch was expected but never materialized — the task sits %q with no live agent and no spawn attempt for over %s. The machine should have re-queued or spawned a leg by now; check the orchestrator logs, or requeue the task by hand.",
			TaskTitle(ctx, pool, d.taskID), d.status, DurHuman(bound))
	}
	return nil
}
