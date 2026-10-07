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
//
// The in-round re-dispatch is bounded (FreezeRespawnCapFromEnv, default 3):
// the freeze observation rows each guarded retire writes are the budget
// ledger, and once an episode has spent it, the next freeze parks the task
// needs-human instead of respawning — the old 2h stall watchdog was an
// accidental circuit breaker against systemic outages (model API down →
// freeze→respawn→freeze forever, one 🆘 every ~31m); this is the
// deliberate one.
package pipeline

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
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

// FreezeRespawnCapEnv tunes the freeze→respawn circuit breaker
// (MAQUINISTA_WATCHDOG_RESPAWN_CAP).
const FreezeRespawnCapEnv = "MAQUINISTA_WATCHDOG_RESPAWN_CAP"

// DefaultFreezeRespawnCap bounds the in-round re-dispatch: after this many
// watchdog retires of the same episode (task+round for reviewer/fixer,
// task for the implementor arm) the NEXT freeze parks the task needs-human
// instead of respawning. At the default bounds 3 respawns + the final park
// spans ~2h — the old stall watchdog's accidental circuit-breaker window.
const DefaultFreezeRespawnCap = 3

// FreezeRespawnCapFromEnv resolves the respawn cap from
// MAQUINISTA_WATCHDOG_RESPAWN_CAP, falling back to the documented default
// on absent or malformed values. Every freeze arm reads the same cap, so
// one var tunes the whole circuit breaker.
func FreezeRespawnCapFromEnv() int {
	respawnCap := DefaultFreezeRespawnCap
	if v := strings.TrimSpace(os.Getenv(FreezeRespawnCapEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			respawnCap = n
		} else {
			log.Printf("pipeline: freeze: invalid %s %q, using %d", FreezeRespawnCapEnv, v, respawnCap)
		}
	}
	return respawnCap
}

// CountFreezeRetires counts one arm's prior freeze retirements for a task —
// the observation rows each guarded retire writes are the respawn budget's
// ledger. The prefix is arm- and round-scoped ('watchdog: reviewer frozen
// (round 2)'), so each new round starts with a fresh budget.
func CountFreezeRetires(ctx context.Context, pool *pgxpool.Pool, taskID, prefix string) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_context
		WHERE task_id = $1 AND kind = 'observation' AND content LIKE $2 || '%'
	`, taskID, prefix).Scan(&n)
	return n, err
}

// ParkEpisodeTx parks a task needs-human inside a freeze retire's tx —
// guarded on the episode status so a raced transition writes nothing — and
// records the verdict row naming the park reason. The caller's guarded
// retire is the exactly-once gate: only its single winner runs this hook,
// so the park (like the retire's 🆘) fires exactly once.
func ParkEpisodeTx(ctx context.Context, tx pgx.Tx, taskID, fromStatus, why string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'pending_approval' WHERE id = $1 AND status = $2`, taskID, fromStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // raced out of the episode status — nothing to park
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO task_context (task_id, kind, content) VALUES ($1, 'verdict', $2)`, taskID, why)
	return err
}

// watchdogPass retires frozen reviewers and fixers. Unlike the pre-MAQ-31
// stall watchdog it does NOT park the task: a frozen reviewer respawns
// in-round (task stays 'review'), a frozen fixer's episode is re-armed
// (task stays 'changes_requested'). The re-dispatch happens through the
// existing passes on their next tick — no new spawn code. The respawn
// budget is capped per task+round (respawnCap): once spent, the next
// freeze parks needs-human instead — the circuit breaker against a
// systemic outage looping freeze→respawn forever.
func watchdogPass(ctx context.Context, pool *pgxpool.Pool, idle, spawn time.Duration, respawnCap int, sessionName string, killWindow func(session, windowID string) error) error {
	args := []any{idle.Seconds(), spawn.Seconds()}

	var reviewers []liveReviewer
	if err := scanReviewers(ctx, pool, liveReviewersSQL+FreezeFilterSQL, args, &reviewers); err != nil {
		return err
	}
	for _, r := range reviewers {
		spent, err := CountFreezeRetires(ctx, pool, r.taskID, fmt.Sprintf("watchdog: reviewer frozen (round %d)", r.round))
		if err != nil {
			return err
		}
		if spent >= respawnCap {
			// Respawn budget spent: a fresh reviewer would freeze the same
			// way (systemic outage, undeliverable prompt). Circuit-break to
			// needs-human — retire + park in one tx, the guarded retire is
			// still the exactly-once dedup.
			note := fmt.Sprintf("watchdog: reviewer frozen (round %d) — no outbox activity for %s past the %s spawn grace; %d in-round respawns already spent, parking needs-human", r.round, idle, spawn, spent)
			applied, err := RetireFrozenAgentCleanup(ctx, pool, r.agentID, r.taskID, note, func(tx pgx.Tx) error {
				return ParkEpisodeTx(ctx, tx, r.taskID, "review", note)
			})
			if err != nil {
				return err
			}
			if applied {
				log.Printf("pipeline: dispatch: watchdog retired frozen reviewer %s on %s — respawn cap (%d) reached, parked needs-human", r.agentID, r.taskID, respawnCap)
				killReviewerPane(sessionName, r.session, r.window, killWindow)
			}
			continue
		}
		note := fmt.Sprintf("watchdog: reviewer frozen (round %d) — no outbox activity for %s past the %s spawn grace; auto-retired, fresh reviewer respawns in-round", r.round, idle, spawn)
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
		spent, err := CountFreezeRetires(ctx, pool, r.taskID, fmt.Sprintf("watchdog: fixer frozen (round %d)", r.round))
		if err != nil {
			return err
		}
		if spent >= respawnCap {
			// Budget spent — park instead of re-arming. Same episode release
			// as the respawn arm, so a human flipping the task back to
			// changes_requested re-arms clean.
			note := fmt.Sprintf("watchdog: fixer frozen (round %d) — no outbox activity for %s past the %s spawn grace; %d episode re-arms already spent, parking needs-human", r.round, idle, spawn, spent)
			applied, err := retireFrozenAgentTx(ctx, pool, r.agentID, r.taskID, note, func(tx pgx.Tx) error {
				var round int
				if err := tx.QueryRow(ctx,
					`SELECT review_rounds FROM tasks WHERE id = $1 FOR UPDATE`, r.taskID).Scan(&round); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `
					DELETE FROM task_context
					WHERE task_id = $1 AND kind = 'fix' AND content = $2
				`, r.taskID, fmt.Sprintf("round %d", round)); err != nil {
					return err
				}
				return ParkEpisodeTx(ctx, tx, r.taskID, "changes_requested", note)
			})
			if err != nil {
				return err
			}
			if applied {
				log.Printf("pipeline: dispatch: watchdog retired frozen fixer %s on %s — respawn cap (%d) reached, parked needs-human", r.agentID, r.taskID, respawnCap)
				killReviewerPane(sessionName, r.session, r.window, killWindow)
			}
			continue
		}
		note := fmt.Sprintf("watchdog: fixer frozen (round %d) — no outbox activity for %s past the %s spawn grace; auto-retired, fix episode re-armed", r.round, idle, spawn)
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
	// MAQ-38: the retire also releases the tmux_window binding. A dead row
	// holding @N is a collision timebomb — tmux window ids restart with the
	// tmux server, and the next pane to draw that id would share it with a
	// corpse (monitor attribution, transcript liveness and pane kills all
	// resolve through this binding).
	tag, err := tx.Exec(ctx, `
		UPDATE agents SET status='dead', last_seen=NOW(), tmux_window=''
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
