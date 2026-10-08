package pipeline

// Orphan sweep (MAQ-41). A pipeline worker row can end up with task_id NULL:
// the completion route (scripts/maquinista-done / db.MarkDone) releases
// agents worker-pool style — `UPDATE agents SET task_id=NULL, status='idle'`
// — and any other writer that drops the binding (or a pre-existing NULL row
// resurrected by an ON CONFLICT DO NOTHING) lands the same way. A NULL
// binding is invisible to every completion leg (verdict, fixer, watchdog,
// merger passes all JOIN tasks ON agents.task_id), so the turn's end is
// unprocessable: the task never advances and the state machine wedges
// silently (MAQ-37: PR #44 sat CLEAN + approved for 16h while its fixer's
// finished round went nowhere).
//
// The sweep runs inside the existing dispatch tick and gives every
// NULL-task_id pipeline-role row exactly one of three dispositions — never
// silence:
//
//   - id embeds a task id (`<role>-<taskID>[-rN]`, the pipeline mint shape)
//     and the task exists:
//     - mid-flight rows (running/working) get their binding BACKFILLED, so
//       the verdict/fixer/watchdog legs see them again (the freeze bounds
//       still cap a genuinely dead pane);
//     - post-turn rows (idle/stopped) get the binding restored for the
//       record AND are retired dead — the turn is over, and a live row
//       with a binding would hold the task's unique-live slot forever.
//       If the task is still sitting in changes_requested with the current
//       round's fix row (the completed-fixer-whose-done-was-lost shape),
//       the episode is RE-ARMED in the same breath — the same transition
//       the frozen-fixer watchdog arm makes — so the next fixerPass mints
//       a fresh fixer instead of the task wedging behind a consumed
//       episode.
//   - anything else (no parseable task id, or the task row is gone): the
//     row is retired dead with one 🆘 on the pipeline topic.
//
// Exactly-once rides the same guarded-retire pattern as the freeze
// watchdog: only the single winner of the status→dead UPDATE notifies. A
// backfill is its own dedup (task_id IS NULL guard), so a repeat tick
// no-ops.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// orphanSweepNotePrefix stamps every sweep-generated task_context
// observation so dedup checks and humans can tell them apart from
// watchdog notes.
const orphanSweepNotePrefix = "orphan sweep"

// orphanAgentsSQL: live pipeline-role rows with no task binding — the
// sweep's only inputs. Dead rows are already inert (outside the unique-live
// index, invisible to the completion legs by the status filters); non-pipeline
// roles (user/executor/hook) are legitimately task-less.
const orphanAgentsSQL = `
SELECT id, role, status
FROM agents
WHERE task_id IS NULL
  AND status <> 'dead'
  AND role IN ('` + reviewerRole + `', '` + fixerRole + `', '` + mergerRole + `', '` + WorkerRole + `')`

// orphanTaskIDRe matches the pipeline mint shape: <role>-<taskID>[-rN]
// (mintAgentID) with the role anchored to the pipeline set. The task id
// segment is what the binding is restored from; non-mint ids (user topic
// agents, named agents) are rejected so the sweep never guesses.
var orphanTaskIDRe = regexp.MustCompile(`^(?:implementor|reviewer|fixer|merger)-(.+?)(?:-r\d+)?$`)

// taskIDFromOrphanAgentID extracts the task id embedded in a pipeline agent
// id ("<role>-<taskID>[-rN]"). ok=false when the shape doesn't match.
func taskIDFromOrphanAgentID(agentID string) (taskID string, ok bool) {
	m := orphanTaskIDRe.FindStringSubmatch(agentID)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return "", false
	}
	return m[1], true
}

// fixerEpisodeStuckSQL: is the task still parked in changes_requested with
// the CURRENT round's fix row in place — the state a fixer completes OUT of
// (its done flips the task to review) — i.e. the completed-fixer-whose-
// completion-was-never-processed shape.
const fixerEpisodeStuckSQL = `
SELECT EXISTS (
	SELECT 1 FROM tasks t
	WHERE t.id = $1 AND t.status = 'changes_requested'
	  AND EXISTS (
	        SELECT 1 FROM task_context f
	        WHERE f.task_id = t.id AND f.kind = 'fix'
	          AND f.content = 'round ' || t.review_rounds::text)
)`

// orphanAgent is one NULL-task_id pipeline row to reconcile.
type orphanAgent struct {
	id, role, status string
}

// orphanSweepPass reconciles every live NULL-task_id pipeline-role row (see
// the package comment). Errors from the row scan abort the pass; per-row
// failures are logged and retried next tick.
func orphanSweepPass(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, orphanAgentsSQL)
	if err != nil {
		return err
	}
	var orphans []orphanAgent
	for rows.Next() {
		var o orphanAgent
		if err := rows.Scan(&o.id, &o.role, &o.status); err != nil {
			rows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, o := range orphans {
		taskID, ok := taskIDFromOrphanAgentID(o.id)
		if !ok {
			retireUnrecoverableOrphan(ctx, pool, o)
			continue
		}
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`, taskID).Scan(&exists); err != nil {
			log.Printf("pipeline: orphan sweep: task lookup %s (from %s): %v", taskID, o.id, err)
			continue
		}
		if !exists {
			// Task gone (deleted/reaped): the row can never be processed
			// again — retire + one pipeline-topic alert, no task row to
			// attach an observation to.
			retireUnrecoverableOrphan(ctx, pool, o)
			continue
		}
		switch o.status {
		case "running", "working":
			backfillLiveOrphan(ctx, pool, o, taskID)
		default:
			retireCompletedOrphan(ctx, pool, o, taskID)
		}
	}
	return nil
}

// backfillLiveOrphan restores the binding of a mid-flight row so the
// completion legs can process its turn. Guarded against stealing the task's
// unique-live slot from another live row: on conflict the orphan is retired
// instead (the task already has a worker). The backfill UPDATE is its own
// exactly-once guard.
func backfillLiveOrphan(ctx context.Context, pool *pgxpool.Pool, o orphanAgent, taskID string) {
	tag, err := pool.Exec(ctx, `
		UPDATE agents SET task_id = $2, last_seen = NOW()
		WHERE id = $1 AND task_id IS NULL
		  AND NOT EXISTS (
		        SELECT 1 FROM agents x
		        WHERE x.task_id = $2 AND x.id <> $1 AND x.status <> 'dead')
	`, o.id, taskID)
	if err != nil {
		log.Printf("pipeline: orphan sweep: backfill %s → %s: %v", o.id, taskID, err)
		return
	}
	if tag.RowsAffected() == 0 {
		// Another live row holds the task's slot (or the row raced) — the
		// task is covered; the orphan is a duplicate pane at best. Retire
		// quietly: no wedge to alert about.
		retireOrphanRow(ctx, pool, o.id)
		log.Printf("pipeline: orphan sweep: %s unbound but task %s has a live worker — retired the orphan", o.id, taskID)
		return
	}
	note := fmt.Sprintf("%s: %s (%s) was live with a NULL task_id — binding to task %s restored; completion legs resume", orphanSweepNotePrefix, o.id, o.status, taskID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'observation', $3)
	`, taskID, o.id, note); err != nil {
		log.Printf("pipeline: orphan sweep: observation for %s: %v", o.id, err)
	}
	notifyTaskf(ctx, pool, taskID, "🩹 %s: %s — unbound %s row (%s) re-attached to the task; its turn is processable again.%s",
		TaskTitle(ctx, pool, taskID), note, o.status, o.id, prLinkSuffix(ctx, pool, taskID))
	log.Printf("pipeline: orphan sweep: backfilled task_id of live %s row %s → %s", o.role, o.id, taskID)
}

// retireCompletedOrphan reconciles a post-turn row (idle/stopped): the turn
// ended, so the row is retired dead — with its binding restored first, in
// the same statement, so the agents record stays auditable without holding
// the task's unique-live slot. When the shape says the completion was never
// processed (a fixer whose task still sits in changes_requested on the
// current episode), the episode is re-armed — the frozen-fixer watchdog
// arm's transition — and the 🆘 fires once; every other case is a benign
// remnant of the worker-pool release and only logs.
func retireCompletedOrphan(ctx context.Context, pool *pgxpool.Pool, o orphanAgent, taskID string) {
	// Exactly-once gate for the re-arm + alert below: only the single
	// winner of this retire proceeds.
	tag, err := pool.Exec(ctx, `
		UPDATE agents SET task_id = $2, status = 'dead', last_seen = NOW(), tmux_window = ''
		WHERE id = $1 AND status IN ('idle', 'stopped') AND task_id IS NULL
	`, o.id, taskID)
	if err != nil {
		log.Printf("pipeline: orphan sweep: retire %s: %v", o.id, err)
		return
	}
	if tag.RowsAffected() == 0 {
		return // raced to dead/bound elsewhere — nothing to reconcile
	}
	retireOrphanPrompts(ctx, pool, o.id)

	if o.role == fixerRole {
		var stuck bool
		if err := pool.QueryRow(ctx, fixerEpisodeStuckSQL, taskID).Scan(&stuck); err != nil {
			log.Printf("pipeline: orphan sweep: episode check %s: %v", o.id, err)
			return
		}
		if stuck {
			// The fixer's turn ended but the task never advanced — its done
			// was lost (unprocessed completion). Release the episode so the
			// next fixerPass mints a fresh fixer, exactly like the frozen-
			// fixer arm does, and say so ONCE (the retire above is the dedup).
			var round int
			if err := pool.QueryRow(ctx,
				`SELECT review_rounds FROM tasks WHERE id = $1`, taskID).Scan(&round); err != nil {
				log.Printf("pipeline: orphan sweep: round lookup %s: %v", taskID, err)
				return
			}
			if _, err := pool.Exec(ctx, `
				DELETE FROM task_context
				WHERE task_id = $1 AND kind = 'fix' AND content = $2
			`, taskID, fmt.Sprintf("round %d", round)); err != nil {
				log.Printf("pipeline: orphan sweep: episode release %s: %v", taskID, err)
				return
			}
			note := fmt.Sprintf("%s: fixer %s completed (status %s) while the task sat changes_requested on round %d — its completion was never processed (NULL task_id detached it); episode re-armed, a fresh fixer re-runs the round", orphanSweepNotePrefix, o.id, o.status, round)
			if _, err := pool.Exec(ctx, `
				INSERT INTO task_context (task_id, agent_id, kind, content)
				VALUES ($1, $2, 'observation', $3)
			`, taskID, o.id, note); err != nil {
				log.Printf("pipeline: orphan sweep: observation for %s: %v", o.id, err)
			}
			notifyTaskf(ctx, pool, taskID, "🆘 %s: %s — a completed fixer's round went unprocessed; the episode was re-armed and a fresh fixer respawns.%s",
				TaskTitle(ctx, pool, taskID), note, prLinkSuffix(ctx, pool, taskID))
			log.Printf("pipeline: orphan sweep: retired completed fixer %s (task %s still changes_requested) — episode re-armed", o.id, taskID)
			return
		}
	}
	// Benign remnant (the task advanced past the episode, or the role has
	// its own recovery legs): the binding is restored for the record, the
	// row is dead — log only, no alert noise.
	log.Printf("pipeline: orphan sweep: retired post-turn %s row %s (task %s) — binding restored for the record", o.role, o.id, taskID)
}

// retireUnrecoverableOrphan retires a row whose binding can't be recovered
// (no task id in the agent id, or the task row is gone) and fires exactly
// one pipeline-topic alert — the guarded retire is the dedup. Never silent:
// an unexplainable pipeline row means a spawn path bypassed the mint
// contract.
func retireUnrecoverableOrphan(ctx context.Context, pool *pgxpool.Pool, o orphanAgent) {
	tag, err := pool.Exec(ctx, `
		UPDATE agents SET status = 'dead', last_seen = NOW(), tmux_window = ''
		WHERE id = $1 AND status <> 'dead' AND task_id IS NULL
	`, o.id)
	if err != nil {
		log.Printf("pipeline: orphan sweep: retire %s: %v", o.id, err)
		return
	}
	if tag.RowsAffected() == 0 {
		return // raced — the winner alerted
	}
	retireOrphanPrompts(ctx, pool, o.id)
	notifyf(ctx, pool, "🆘 orphan sweep: pipeline row %s (role %s, status %s) had no task binding and no recoverable task id — retired dead. If this id was minted by the pipeline, the spawn path bypassed its task contract.", o.id, o.role, o.status)
	log.Printf("pipeline: orphan sweep: retired unrecoverable row %s (role %s)", o.id, o.role)
}

// retireOrphanPrompts drops a retired orphan's undriven task prompts — same
// rationale as the freeze watchdog's retire: a respawn re-enqueues the same
// dedup key and the orphan's stale row would shadow it forever.
func retireOrphanPrompts(ctx context.Context, pool *pgxpool.Pool, agentID string) {
	if _, err := pool.Exec(ctx, `
		DELETE FROM agent_inbox WHERE agent_id = $1 AND origin_channel = 'task'
	`, agentID); err != nil {
		log.Printf("pipeline: orphan sweep: prompt cleanup %s: %v", agentID, err)
	}
}

// retireOrphanRow is the minimal guarded retire used when no alert is due.
func retireOrphanRow(ctx context.Context, pool *pgxpool.Pool, agentID string) {
	if _, err := pool.Exec(ctx, `
		UPDATE agents SET status = 'dead', last_seen = NOW(), tmux_window = ''
		WHERE id = $1 AND status <> 'dead'
	`, agentID); err != nil {
		log.Printf("pipeline: orphan sweep: retire %s: %v", agentID, err)
	}
}
