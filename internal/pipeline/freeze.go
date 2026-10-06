// Freeze watchdog (MAQ-31): retires live pipeline agents whose agent_outbox
// went silent — the ground-truth activity signal — and re-dispatches the
// work per role instead of parking the task needs-human.
//
// The failure this replaces (2026-10-06 night): two agents froze silently —
// an implementor pre-PR and a reviewer mid-review — and sat `running` for
// 60+ minutes with live tmux panes and ZERO outbox rows while the old stall
// watchdog never fired. Two defects:
//
//  1. the newborn exemption was `started_at < NOW() - timeout` — the FULL
//     stall bound (2h) — so any agent under two hours old was untouchable;
//  2. implementors on `claimed` tasks had no watchdog arm at all (the only
//     self-heal, retireStuckImplementor, fires lazily on a reviewer-spawn
//     uq_agents_task_live failure — which never happens pre-PR, because the
//     task never reaches review).
//
// The freeze rule (both bounds tunable, see FreezeBoundsFromEnv):
//
//	frozen := started_at < NOW() - SpawnGrace          // newborn grace, 10m
//	       AND no agent_outbox row within IdleAfter    // silence, 30m
//	       AND no transcript growth within IdleAfter   // MAQ-9 channel
//
// Transcript growth stays a liveness veto (MAQ-9, verified live): a healthy
// agent mid-command streams tool events into the JSONL but writes no outbox
// text — killing on outbox silence alone would murder long legitimate turns.
// A frozen agent is silent on BOTH channels; tonight's victims were.
// Pane existence and agents.last_seen are explicitly NOT signals.
//
// On freeze: retire the row (the guarded UPDATE is the exactly-once 🆘
// dedup), kill the pane, re-dispatch per role — reviewer → fresh -rN
// reviewer in-round (task stays 'review'; dispatchPass mints), fixer →
// episode re-armed (fix row released; fixerPass mints), implementor →
// claim requeued to 'ready' (taskscheduler.RetireFrozenClaims + the
// stale-claim reaper). Mergers keep their needs-human park: the money path
// keeps a human gate. Every auto-retire notifies; a heal is never silent.
package pipeline

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Freeze bounds (MAQ-31).
const (
	// DefaultFreezeIdle is the silence bound: a live pipeline agent with no
	// outbox row AND no transcript growth for this long — past SpawnGrace —
	// is frozen and auto-retired (MAQUINISTA_WATCHDOG_IDLE).
	DefaultFreezeIdle = 30 * time.Minute
	// DefaultFreezeSpawn is the newborn grace: agents younger than this are
	// never frozen (prompt delivery races pi cold boot — a fresh spawn has
	// no signal on either channel yet). MAQUINISTA_WATCHDOG_SPAWN.
	DefaultFreezeSpawn = 10 * time.Minute
)

// FreezeBoundsFromEnv resolves the shared freeze bounds from
// MAQUINISTA_WATCHDOG_IDLE / MAQUINISTA_WATCHDOG_SPAWN, falling back to the
// documented defaults on absent or malformed values. Both the dispatch
// watchdog and the task scheduler's claim arm read the same pair, so one
// env pair tunes every freeze arm.
func FreezeBoundsFromEnv() (idle, spawn time.Duration) {
	idle, spawn = DefaultFreezeIdle, DefaultFreezeSpawn
	if v := strings.TrimSpace(os.Getenv("MAQUINISTA_WATCHDOG_IDLE")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			idle = d
		} else {
			log.Printf("pipeline: freeze: invalid MAQUINISTA_WATCHDOG_IDLE %q, using %s", v, idle)
		}
	}
	if v := strings.TrimSpace(os.Getenv("MAQUINISTA_WATCHDOG_SPAWN")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			spawn = d
		} else {
			log.Printf("pipeline: freeze: invalid MAQUINISTA_WATCHDOG_SPAWN %q, using %s", v, spawn)
		}
	}
	return idle, spawn
}

// FreezeFilterSQL is the shared freeze predicate, appended to a query whose
// FROM row is `agents a` (joined to tasks as needed). Params: $1 = idle
// bound in seconds, $2 = spawn grace in seconds. An agent row matching this
// filter is frozen: past the newborn grace, silent on the outbox channel
// (no row within idle) AND on the transcript channel (no growth within
// idle, MAQ-9) — never a fresh signal on either.
const FreezeFilterSQL = `
  AND a.started_at < NOW() - make_interval(secs => $2)
  AND NOT EXISTS (
        SELECT 1 FROM agent_outbox o
        WHERE o.agent_id = a.id AND o.created_at > NOW() - make_interval(secs => $1))
  AND (a.last_transcript_at IS NULL
       OR a.last_transcript_at < NOW() - make_interval(secs => $1))`

// watchdogPass retires frozen reviewers and fixers. Unlike the pre-MAQ-31
// stall watchdog it does NOT park the task: a frozen reviewer respawns
// in-round (task stays 'review'), a frozen fixer's episode is re-armed
// (task stays 'changes_requested'). The re-dispatch happens through the
// existing passes on their next tick — no new spawn code.
func watchdogPass(ctx context.Context, pool *pgxpool.Pool, idle, spawn time.Duration, sessionName string, killWindow func(session, windowID string) error) error {
	args := []any{idle.Seconds(), spawn.Seconds()}

	var reviewers []liveReviewer
	if err := scanReviewers(ctx, pool, liveReviewersSQL+FreezeFilterSQL, args, &reviewers); err != nil {
		return err
	}
	for _, r := range reviewers {
		note := fmt.Sprintf("watchdog: reviewer frozen — no outbox activity for %s past the %s spawn grace; auto-retired, fresh reviewer respawns in-round", idle, spawn)
		applied, err := RetireFrozenAgent(ctx, pool, r.agentID, r.taskID, note)
		if err != nil {
			return err
		}
		if applied {
			log.Printf("pipeline: dispatch: watchdog retired frozen reviewer %s on %s — respawning", r.agentID, r.taskID)
			killReviewerPane(sessionName, r.session, r.window, killWindow)
		}
	}

	var fixers []liveReviewer
	if err := scanReviewers(ctx, pool, liveFixersSQL+FreezeFilterSQL, args, &fixers); err != nil {
		return err
	}
	for _, r := range fixers {
		note := fmt.Sprintf("watchdog: fixer frozen — no outbox activity for %s past the %s spawn grace; auto-retired, fix episode re-armed", idle, spawn)
		applied, err := retireFrozenAgentTx(ctx, pool, r.agentID, r.taskID, note, func(tx pgx.Tx) error {
			// Release the episode: fixerCandidatesSQL keys on the round's
			// fix row, so deleting it lets the next fixerPass mint a fresh
			// fixer (-rN) for the SAME round. The frozen agent's own fix
			// prompt row is already gone (RetireFrozenAgent drops its
			// undriven prompts), so the re-armed episode re-enqueues clean.
			var round int
			if err := tx.QueryRow(ctx,
				`SELECT review_rounds FROM tasks WHERE id = $1 FOR UPDATE`, r.taskID).Scan(&round); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				DELETE FROM task_context
				WHERE task_id = $1 AND kind = 'fix' AND content = $2
			`, r.taskID, fmt.Sprintf("round %d", round))
			return err
		})
		if err != nil {
			return err
		}
		if applied {
			log.Printf("pipeline: dispatch: watchdog retired frozen fixer %s on %s — episode re-armed", r.agentID, r.taskID)
			killReviewerPane(sessionName, r.session, r.window, killWindow)
		}
	}
	return nil
}

// RetireFrozenAgent retires one frozen agent row: guarded status→dead (the
// exactly-once 🆘 dedup — only the transition's winner notifies), a
// task_context observation for downstream attempts, and deletion of the
// agent's undriven task prompts (the rationale lives on
// retireFrozenAgentTx). applied=false when the row raced to dead elsewhere
// (no notification).
func RetireFrozenAgent(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note string) (bool, error) {
	return retireFrozenAgentTx(ctx, pool, agentID, taskID, note, nil)
}

// RetireFrozenAgentCleanup is RetireFrozenAgent with a same-tx cleanup hook,
// for retires whose re-dispatch needs an episode-row release (the fixer
// arms delete the round's fix row with it) — atomic with the guarded
// retire, so a raced no-op never releases an episode it didn't win.
func RetireFrozenAgentCleanup(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note string, cleanup func(pgx.Tx) error) (bool, error) {
	return retireFrozenAgentTx(ctx, pool, agentID, taskID, note, cleanup)
}

// retireFrozenAgentTx is the shared retire: one tx — guarded status→dead,
// an 'observation' row for downstream attempts, deletion of the agent's
// undriven task prompts, then the optional cleanup hook. The 🆘 fires only
// after commit, so the guarded retire's single-winner UPDATE is the
// exactly-once dedup.
//
// The prompt deletion is what makes in-place re-dispatch safe for
// episode-keyed roles: a respawned fixer/merger re-enqueues the SAME
// dedup key (fix:<task>:<round>, merger:<task>:<entry>:<attempt>) and the
// frozen agent's stale row would otherwise shadow it forever (EnqueueInbox
// ON CONFLICT DO NOTHING points at the dead pane). A frozen agent by
// definition never acted on its prompt — zero/stale outbox is the freeze
// premise — so the rows are undriven and safe to drop. Implementors don't
// need it (the scheduler's Upsert repoints) and reviewers grow a fresh
// round key, but uniformity costs nothing and un-wedges every shape.
func retireFrozenAgentTx(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note string, extra func(pgx.Tx) error) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE agents SET status='dead', last_seen=NOW()
		WHERE id=$1 AND status <> 'dead'
	`, agentID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil // raced to dead elsewhere — no notification
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'observation', $3)
	`, taskID, agentID, note); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM agent_inbox WHERE agent_id = $1 AND origin_channel = 'task'
	`, agentID); err != nil {
		return false, err
	}
	if extra != nil {
		if err := extra(tx); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	// MAQ-22/MAQ-31: the guarded retire above fired exactly once, so the
	// 🆘 does too — inside the applied branch, never on a raced no-op.
	notifyTaskf(ctx, pool, taskID, "🆘 %s: %s%s", TaskTitle(ctx, pool, taskID), note, prLinkSuffix(ctx, pool, taskID))
	return true, nil
}
