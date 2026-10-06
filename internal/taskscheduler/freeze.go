// Freeze arms for the task scheduler (MAQ-31). The pipeline's dispatch
// watchdog (internal/pipeline/freeze.go) covers reviewer/fixer/merger
// panes; claimed tasks — the implementor phase — belong to this package,
// and only the scheduler's stale-claim reaper can requeue them. Two arms:
//
//   - RetireFrozenClaims runs every wake: a live agent row on a 'claimed'
//     task that is frozen (no outbox row AND no transcript growth past
//     IdleAfter, older than SpawnGrace — the shared pipeline.FreezeFilterSQL
//     predicate) is retired. The task row stays 'claimed'; the next wake's
//     ReapStaleClaims sees only non-live rows and requeues it to 'ready',
//     where DispatchOne mints a fresh -rN implementor. Retiring instead of
//     requeueing inline keeps one writer per transition (the reaper's
//     guarded UPDATE + its 🔄 note).
//
//   - HealRestartCohort runs once at unit start (MAQ-31 AC 3): live
//     task-scoped rows whose last_seen predates THIS process (crash
//     restart — a graceful `maquinista stop` deletes task agents outright)
//     and that never streamed a single outbox row are the frozen cohort the
//     previous process left behind; they are healed on the first pass
//     instead of waiting for a human to notice a stalled board. Rows
//     younger than SpawnGrace are left to the continuous arm: a newborn
//     mid-boot at crash time is not provably frozen (the 04/10
//     newborn-kill class — never murder on sight).
//
// Freeze semantics are documented in pipeline/freeze.go; this file only
// applies them to the claimed/implementor phase.
package taskscheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
)

// frozenClaimsSQL: live agent rows on claimed pipeline/task tasks that
// meet the shared freeze predicate ($1 = idle secs, $2 = spawn secs).
const frozenClaimsSQL = `
SELECT a.id, a.tmux_session, a.tmux_window, t.id, t.title
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE t.status = 'claimed'
  AND a.status IN (` + liveAgentStatusSQL + `)` + pipeline.FreezeFilterSQL

// RetireFrozenClaims retires every frozen agent row on a claimed task.
// Returns the number of rows THIS call retired (each one notified exactly
// once, inside pipeline.RetireFrozenAgent's guarded transition).
func RetireFrozenClaims(ctx context.Context, pool *pgxpool.Pool, idle, spawn time.Duration, sessionName string, killWindow func(session, windowID string) error) (int, error) {
	rows, err := pool.Query(ctx, frozenClaimsSQL, idle.Seconds(), spawn.Seconds())
	if err != nil {
		return 0, err
	}
	type frozen struct{ agentID, session, window, taskID, title string }
	var victims []frozen
	for rows.Next() {
		var f frozen
		if err := rows.Scan(&f.agentID, &f.session, &f.window, &f.taskID, &f.title); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	retired := 0
	for _, f := range victims {
		note := fmt.Sprintf("watchdog: %s frozen — no outbox activity for %s past the %s spawn grace; auto-retired, claim requeues to ready for a fresh attempt", f.agentID, idle, spawn)
		applied, err := pipeline.RetireFrozenAgent(ctx, pool, f.agentID, f.taskID, note)
		if err != nil {
			return retired, err
		}
		if applied {
			retired++
			log.Printf("taskscheduler: watchdog retired frozen %s on %s — reaper requeues next wake", f.agentID, f.taskID)
			killFrozenPane(sessionName, f.session, f.window, killWindow)
		}
	}
	return retired, nil
}

// restartCohortSQL: live task-scoped rows that predate `boot` ($1) and
// never streamed a single outbox row — the crash-restart cohort. Rows
// younger than the spawn grace ($2) are excluded: not provably frozen.
const restartCohortSQL = `
SELECT a.id, a.tmux_session, a.tmux_window, t.id, t.title
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE a.task_id IS NOT NULL
  AND a.status IN (` + liveAgentStatusSQL + `)
  AND a.last_seen < $1
  AND a.started_at < NOW() - make_interval(secs => $2)
  AND NOT EXISTS (
        SELECT 1 FROM agent_outbox o WHERE o.agent_id = a.id)`

// HealRestartCohort sweeps the crash-restart cohort once, at unit start.
// The per-role re-dispatch needs no special casing beyond the shared retire
// (which drops the agent's undriven prompts): 'claimed' tasks are requeued
// by ReapStaleClaims, 'review' tasks respawn via dispatchPass,
// 'changes_requested' episodes re-arm via fixerPass (a fix row the ghost
// held is released atomically with the retire), 'ready_to_merge' episodes
// re-arm via mergerSpawnPass on its next tick.
func HealRestartCohort(ctx context.Context, pool *pgxpool.Pool, boot time.Time, spawnGrace time.Duration, sessionName string, killWindow func(session, windowID string) error) (int, error) {
	rows, err := pool.Query(ctx, restartCohortSQL, boot, spawnGrace.Seconds())
	if err != nil {
		return 0, err
	}
	type ghost struct{ agentID, session, window, taskID, title string }
	var ghosts []ghost
	for rows.Next() {
		var g ghost
		if err := rows.Scan(&g.agentID, &g.session, &g.window, &g.taskID, &g.title); err != nil {
			rows.Close()
			return 0, err
		}
		ghosts = append(ghosts, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	healed := 0
	for _, g := range ghosts {
		note := fmt.Sprintf("watchdog: restart cohort — %s predates this boot (last_seen before startup) and never streamed; auto-retired, task re-dispatches per its state", g.agentID)
		applied, err := pipeline.RetireFrozenAgentCleanup(ctx, pool, g.agentID, g.taskID, note, func(tx pgx.Tx) error {
			// Release a fix episode the ghost was working (same cleanup as
			// the fixer freeze arm) so fixerPass can re-arm it. No-op for
			// every other role/status.
			var round int
			if err := tx.QueryRow(ctx,
				`SELECT review_rounds FROM tasks WHERE id = $1 FOR UPDATE`, g.taskID).Scan(&round); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil // task row vanished mid-sweep — retire alone is the heal
				}
				return err
			}
			_, err := tx.Exec(ctx, `
				DELETE FROM task_context
				WHERE task_id = $1 AND kind = 'fix' AND content = $2
			`, g.taskID, fmt.Sprintf("round %d", round))
			return err
		})
		if err != nil {
			return healed, err
		}
		if applied {
			healed++
			log.Printf("taskscheduler: restart sweep retired pre-boot ghost %s on %s", g.agentID, g.taskID)
			killFrozenPane(sessionName, g.session, g.window, killWindow)
		}
	}
	return healed, nil
}

// killFrozenPane is the scheduler-side twin of pipeline's killReviewerPane:
// best-effort tmux cleanup, nil callback or empty window skips.
func killFrozenPane(sessionName, session, window string, killWindow func(session, windowID string) error) {
	if killWindow == nil || window == "" {
		return
	}
	s := session
	if s == "" {
		s = sessionName
	}
	if err := killWindow(s, window); err != nil {
		log.Printf("taskscheduler: freeze: kill window %s:%s: %v (continuing)", s, window, err)
	}
}
