// Freeze arms for the task scheduler (MAQ-31). The pipeline's dispatch
// watchdog (internal/pipeline/freeze.go) covers reviewer/fixer/merger
// panes; claimed tasks — the implementor phase — belong to this package,
// and only the scheduler's stale-claim reaper can requeue them. Two arms:
//
//   - RetireFrozenClaims runs every wake: a live agent row on a 'claimed'
//     task that is frozen (no outbox row AND no transcript growth past
//     IdleAfter, older than SpawnGrace — the shared pipeline.FreezeFilterSQL
//     predicate) is retired. The task row stays 'claimed'; the SAME wake's
//     ReapStaleClaims (Run orders the retire BEFORE the reaper for exactly
//     this) sees only non-live rows and requeues it to 'ready', where
//     DispatchOne mints a fresh -rN implementor. Retiring instead of
//     requeueing inline keeps one writer per transition (the reaper's
//     guarded UPDATE + its 🔄 note). The respawn budget is capped per task
//     (no rounds in the implementor phase): past pipeline's
//     FreezeRespawnCapFromEnv freeze retires, the next freeze parks the
//     task needs-human in the same retire tx instead of requeueing — the
//     circuit breaker against a systemic outage looping
//     freeze→requeue→claim→freeze forever.
//
//     Every retire note states whether a tmux pane existed for the retired
//     id (MAQ-38 AC 2): the probe is name-based (panes are created
//     -n <agentID> and ids are never reused), so "no" means the round
//     never had a pane — a spawn failure wearing a freeze costume — while
//     "yes" is a true silent worker. The distinction is what turned the
//     07/10 false-freeze churn (stale-id attribution starved every round's
//     freshness, pane present the whole time) from an undiagnosable loop
//     into a one-glance diagnosis in the 🆘.
//
//   - HealRestartCohort runs once per unit boot (MAQ-31 AC 3) — deferred
//     by Run until the monitor has had its first polls (the boot-relative
//     transcript veto is meaningless before the monitor has spoken). Live
//     task-scoped rows whose last_seen predates THIS process (crash
//     restart — a graceful `maquinista stop` deletes task agents outright)
//     and that never signaled post-boot — no outbox row ever AND no
//     transcript growth since the boot — are the frozen cohort the previous
//     process left behind; they are healed on the first pass instead of
//     waiting for a human to notice a stalled board. The transcript veto
//     (same MAQ-9 channel the continuous arms honor) keeps the boot-race
//     honest: a pane that SURVIVED the crash and is mid-turn re-binds and
//     streams tool events within moments, while a true ghost's transcript
//     froze with the old process. Rows younger than SpawnGrace are left to
//     the continuous arm: a newborn mid-boot at crash time is not provably
//     frozen (the 04/10 newborn-kill class — never murder on sight).
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
// paneExists probes "is there a pane for this agent id" (tmux.WindowNameExists
// in production; tests inject) and rides the retire note — MAQ-38 AC 2.
// Returns the number of rows THIS call retired (each one notified exactly
// once, inside pipeline.RetireFrozenAgent's guarded transition). Episodes
// whose respawn budget (respawnCap) is spent park needs-human atomically
// with the retire instead of requeueing.
func RetireFrozenClaims(ctx context.Context, pool *pgxpool.Pool, idle, spawn time.Duration, respawnCap int, sessionName string, killWindow func(session, windowID string) error, paneExists func(session, name string) bool) (int, error) {
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
		spent, err := pipeline.CountFreezeRetires(ctx, pool, f.taskID, "watchdog: implementor")
		if err != nil {
			return retired, err
		}
		pane := paneStateFor(sessionName, f.session, f.agentID, paneExists)
		note := fmt.Sprintf("watchdog: implementor %s frozen — no outbox activity for %s past the %s spawn grace; tmux pane for this id: %s; auto-retired, claim requeues to ready for a fresh attempt", f.agentID, idle, spawn, pane)
		var cleanup func(pgx.Tx) error
		if spent >= respawnCap {
			// Respawn budget spent — a fresh implementor would freeze the
			// same way. Park needs-human atomically with the retire (the
			// guarded retire is still the exactly-once dedup).
			note = fmt.Sprintf("watchdog: implementor %s frozen — no outbox activity for %s past the %s spawn grace; tmux pane for this id: %s; %d respawns already spent, parking needs-human", f.agentID, idle, spawn, pane, spent)
			cleanup = func(tx pgx.Tx) error {
				return pipeline.ParkEpisodeTx(ctx, tx, f.taskID, "claimed", note)
			}
		}
		applied, err := pipeline.RetireFrozenAgentCleanup(ctx, pool, f.agentID, f.taskID, note, cleanup)
		if err != nil {
			return retired, err
		}
		if applied {
			retired++
			if cleanup != nil {
				log.Printf("taskscheduler: watchdog retired frozen %s on %s — respawn cap (%d) reached, parked needs-human", f.agentID, f.taskID, respawnCap)
			} else {
				log.Printf("taskscheduler: watchdog retired frozen %s on %s — reaper requeues same wake", f.agentID, f.taskID)
			}
			killFrozenPane(sessionName, f.session, f.window, killWindow)
		}
	}
	return retired, nil
}

// restartCohortSQL: live task-scoped rows that predate `boot` ($1) and
// never signaled post-boot — no outbox row ever AND no transcript growth
// since the boot — the crash-restart cohort. Rows younger than the spawn
// grace ($2) are excluded: not provably frozen. The last_transcript_at
// clause is the MAQ-9 veto in boot-relative form: a pre-boot pane that
// survived the crash and is mid-turn streams tool events (never outbox
// text) as soon as the monitor re-binds it; killing it on a last_seen
// technicality would be the newborn-kill class in an older disguise. A
// true ghost's transcript froze with the old process — veto passes.
const restartCohortSQL = `
SELECT a.id, a.tmux_session, a.tmux_window, t.id, t.title
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE a.task_id IS NOT NULL
  AND a.status IN (` + liveAgentStatusSQL + `)
  AND a.last_seen < $1
  AND a.started_at < NOW() - make_interval(secs => $2)
  AND NOT EXISTS (
        SELECT 1 FROM agent_outbox o WHERE o.agent_id = a.id)
  AND (a.last_transcript_at IS NULL OR a.last_transcript_at < $1)`

// HealRestartCohort sweeps the crash-restart cohort once, per boot — Run
// calls it after the monitor has had its first polls (see the sweep grace
// in Run: before that the boot-relative transcript veto cannot observe any
// post-boot growth, and the sweep would murder live panes on sight).
// The per-role re-dispatch needs no special casing beyond the shared retire
// (which drops the agent's undriven prompts): 'claimed' tasks are requeued
// by ReapStaleClaims, 'review' tasks respawn via dispatchPass,
// 'changes_requested' episodes re-arm via fixerPass (a fix row the ghost
// held is released atomically with the retire), 'ready_to_merge' episodes
// re-arm via mergerSpawnPass on its next tick. Per-ghost failures do not
// abort the sweep — the remaining ghosts are logged and left to the
// continuous freeze arms.
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
	var errs []error
	var unswept []ghost
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
			// One bad ghost must not abort the sweep (it runs once per
			// boot) — log it, keep sweeping, and leave a survivor trail for
			// operators; the continuous arms cover anything unswept.
			log.Printf("taskscheduler: restart sweep: retire %s on %s failed: %v", g.agentID, g.taskID, err)
			errs = append(errs, err)
			unswept = append(unswept, g)
			continue
		}
		if applied {
			healed++
			log.Printf("taskscheduler: restart sweep retired pre-boot ghost %s on %s", g.agentID, g.taskID)
			killFrozenPane(sessionName, g.session, g.window, killWindow)
		}
	}
	for _, g := range unswept {
		log.Printf("taskscheduler: restart sweep: %s on %s NOT swept — the continuous freeze arms cover it on later passes", g.agentID, g.taskID)
	}
	return healed, errors.Join(errs...)
}

// paneStateFor answers "did a tmux pane exist for this agent id" for the
// freeze note (MAQ-38 AC 2). The probe is name-based — every maquinista
// pane is created with -n <agentID> and agent ids are never reused — so a
// "no" means the retired id never had a pane at all (a spawn failure, not
// a frozen worker), and a "yes" during an apparent freeze is the stale-id
// signature: work was streaming into a pane whose outbox rows were being
// attributed elsewhere. nil probe → "unknown" (tests, and any caller that
// cannot look at tmux); the note still ships.
func paneStateFor(sessionName, session, agentID string, paneExists func(session, name string) bool) string {
	if paneExists == nil {
		return "unknown"
	}
	s := session
	if s == "" {
		s = sessionName
	}
	if paneExists(s, agentID) {
		return "yes"
	}
	return "no"
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
