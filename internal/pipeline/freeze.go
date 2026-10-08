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

// Freeze causes (ADR-0008): why a frozen agent froze. The classifier is
// FreezeCauseOf; the cause rides every freeze observation row
// (task_context.cause) and gates the respawn budget — CountFreezeRetires
// counts only true freezes, so silent successes retire for free.
const (
	// CauseSilentSuccess: a turn end was observed and nothing streamed
	// after it — the agent finished speaking and never called
	// `maquinista-done` (the 08/10 MAQ-37 r8 incident shape). Retires
	// WITHOUT burning respawn budget; the implementor phase goes straight
	// to review when the artifacts allow (open PR + branch up to date).
	CauseSilentSuccess = "silent_success"
	// CauseTrueFreeze: no turn end observed (crash, hang mid-turn — the
	// transcript grew after the last turn end, or never showed one).
	// Unchanged MAQ-31 behavior: counts against the respawn cap.
	CauseTrueFreeze = "true_freeze"
)

// FreezeCauseOf classifies a frozen agent from its monitor-written
// signals (ADR-0008): the last observable event being a clean turn end
// (a turn end at/after the last transcript growth) is a silent success —
// the agent ended its turn and sat silent; transcript growth after the
// last turn end (a later turn started and died mid-way), or no turn end
// at all, is a true freeze — the agent crashed or hung mid-turn. NULL
// transcript timestamps mean "never observed", not "old".
func FreezeCauseOf(turnEnd, transcriptAt *time.Time) string {
	if turnEnd != nil && (transcriptAt == nil || !turnEnd.Before(*transcriptAt)) {
		return CauseSilentSuccess
	}
	return CauseTrueFreeze
}

// FreezeCauseSelectSQL is FreezeCauseOf as a SELECT expression over an
// `agents a` row, so freeze-arm queries can carry the classified cause
// alongside the victim columns.
const FreezeCauseSelectSQL = `CASE
	WHEN a.last_turn_end_at IS NOT NULL
	     AND (a.last_transcript_at IS NULL OR a.last_turn_end_at >= a.last_transcript_at)
	THEN '` + CauseSilentSuccess + `'
	ELSE '` + CauseTrueFreeze + `' END`

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

// CountFreezeRetires counts one arm's prior TRUE-FREEZE retirements for a
// task — the observation rows each guarded retire writes are the respawn
// budget's ledger, and since ADR-0008 only 'true_freeze' rows burn budget:
// a silent success (turn end observed, no done verb) retires for free
// instead of punishing completed work as a freeze failure. The prefix is
// arm- and round-scoped ('watchdog: reviewer frozen (round 2)'), so each
// new round starts with a fresh budget. Rows from before the cause column
// existed carry NULL and stop counting — a deploy-time budget reset per
// task+round, bounded and harmless (the systemic-outage loop the cap
// guards against re-parks within ~2h at the default bounds).
func CountFreezeRetires(ctx context.Context, pool *pgxpool.Pool, taskID, prefix string) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_context
		WHERE task_id = $1 AND kind = 'observation'
		  AND content LIKE $2 || '%' AND cause = $3
	`, taskID, prefix, CauseTrueFreeze).Scan(&n)
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
//
// The reviewer arm also salvages (MAQ-36): a frozen reviewer that already
// wrote its findings but lost only the verdict delivery hands the writeup to
// its respawn — the fresh round verifies + posts the verdict instead of
// re-reviewing the diff from scratch.
// freezeCause renders what the freeze filter (FreezeFilterSQL) actually
// measured — the honest phrasing for every retire note (MAQ-36). The old
// "no outbox activity for %s past the %s spawn grace" misrepresented the
// predicate two ways: the bounds run in PARALLEL (the idle window does not
// start after the spawn grace lapses), and outbox silence alone never
// retires anyone — the MAQ-9 transcript veto must be silent too.
func freezeCause(idle, spawn time.Duration) string {
	return fmt.Sprintf("silent for %s (no outbox row and no transcript growth; spawn grace %s elapsed)", idle, spawn)
}

// minSalvageFindingsChars is the minimum length for an outbox text row to
// count as a salvageable findings writeup (MAQ-36): a real review writeup is
// a numbered findings list; shorter rows are progress notes and acks.
const minSalvageFindingsChars = 200

// salvageFindings captures a frozen reviewer's completed-but-undelivered
// writeup (MAQ-36): the newest outbox text row long enough to be a findings
// list, newest-first scan like latestVerdict. "" when nothing qualifies —
// the best-effort fallback is today's full re-review.
func salvageFindings(ctx context.Context, pool *pgxpool.Pool, agentID string) (string, error) {
	rows, err := pool.Query(ctx, `
		SELECT content->>'text' FROM agent_outbox
		WHERE agent_id = $1 AND content ? 'text'
		ORDER BY created_at DESC LIMIT 10
	`, agentID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return "", err
		}
		if t := strings.TrimSpace(text); len([]rune(t)) >= minSalvageFindingsChars {
			if len(t) > maxFindingsChars {
				t = t[len(t)-maxFindingsChars:]
			}
			return t, nil
		}
	}
	return "", rows.Err()
}

// insertSalvagedFindingsTx records the captured writeup as a task_context
// 'salvage' row inside the retire tx, so the respawned reviewer's prompt
// build can carry it. Empty findings (the fallback) inserts nothing — and a
// prompt build ignores salvage rows once any verdict has landed after them.
func insertSalvagedFindingsTx(ctx context.Context, tx pgx.Tx, taskID, agentID, findings string) error {
	if strings.TrimSpace(findings) == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'salvage', $3)
	`, taskID, agentID, findings)
	return err
}

func watchdogPass(ctx context.Context, pool *pgxpool.Pool, idle, spawn time.Duration, respawnCap int, sessionName string, killWindow func(session, windowID string) error, fan parkFanout) error {
	args := []any{idle.Seconds(), spawn.Seconds()}

	var reviewers []liveReviewer
	if err := scanReviewers(ctx, pool, liveReviewersSQL+FreezeFilterSQL, args, &reviewers); err != nil {
		return err
	}
	for _, r := range reviewers {
		// ADR-0008: the cause splits the arm. A silent success (turn end
		// observed, no done verb — the r8 incident shape) never parks and
		// never burns budget: the worst it earns is a fresh round, and the
		// existing round cap is what bounds a genuinely confused agent. A
		// true freeze keeps MAQ-31's circuit breaker exactly as it was.
		if r.cause == CauseSilentSuccess {
			// MAQ-36 salvage applies doubly here: a reviewer that ended its
			// turn without the verdict very likely wrote its findings —
			// hand them to the fresh round.
			salvage, err := salvageFindings(ctx, pool, r.agentID)
			if err != nil {
				log.Printf("pipeline: dispatch: watchdog: salvage findings %s: %v — respawning without", r.agentID, err)
				salvage = ""
			}
			note := fmt.Sprintf("watchdog: reviewer turn-ended without a verdict (round %d) — %s; auto-retired, fresh reviewer respawns in-round (no respawn budget burned)", r.round, freezeCause(idle, spawn))
			applied, err := RetireFrozenAgentCause(ctx, pool, r.agentID, r.taskID, note, CauseSilentSuccess, func(tx pgx.Tx) error {
				return insertSalvagedFindingsTx(ctx, tx, r.taskID, r.agentID, salvage)
			})
			if err != nil {
				return err
			}
			if applied {
				log.Printf("pipeline: dispatch: watchdog retired silent-success reviewer %s on %s — respawning (budget untouched)", r.agentID, r.taskID)
				killReviewerPane(sessionName, r.session, r.window, killWindow)
			}
			continue
		}
		spent, err := CountFreezeRetires(ctx, pool, r.taskID, fmt.Sprintf("watchdog: reviewer frozen (round %d)", r.round))
		if err != nil {
			return err
		}
		if spent >= respawnCap {
			// Respawn budget spent: a fresh reviewer would freeze the same
			// way (systemic outage, undeliverable prompt). Circuit-break to
			// needs-human — retire + park in one tx, the guarded retire is
			// still the exactly-once dedup.
			note := fmt.Sprintf("watchdog: reviewer frozen (round %d) — %s; %d in-round respawns already spent, parking needs-human", r.round, freezeCause(idle, spawn), spent)
			applied, err := RetireFrozenAgentCause(ctx, pool, r.agentID, r.taskID, note, CauseTrueFreeze, func(tx pgx.Tx) error {
				return ParkEpisodeTx(ctx, tx, r.taskID, "review", note)
			})
			if err != nil {
				return err
			}
			if applied {
				log.Printf("pipeline: dispatch: watchdog retired frozen reviewer %s on %s — respawn cap (%d) reached, parked needs-human", r.agentID, r.taskID, respawnCap)
				killReviewerPane(sessionName, r.session, r.window, killWindow)
				// MAQ-34: the park fans out to the PR + ticket issue (deduped
				// by the fan-out's episode marker).
				fan.notify(ctx, pool, r.taskID, fmt.Sprintf("Reviewer froze repeatedly (round %d) — respawn cap (%d) reached, parked needs-human.", r.round, respawnCap))
			}
			continue
		}
		// MAQ-36 salvage: capture the frozen reviewer's writeup (best effort)
		// BEFORE the retire — its fresh respawn then verifies + delivers
		// instead of re-reviewing from scratch. No writeup → no salvage row,
		// and the fallback is today's full re-review.
		salvage, err := salvageFindings(ctx, pool, r.agentID)
		if err != nil {
			log.Printf("pipeline: dispatch: watchdog: salvage findings %s: %v — respawning without", r.agentID, err)
			salvage = ""
		}
		note := fmt.Sprintf("watchdog: reviewer frozen (round %d) — %s; auto-retired, fresh reviewer respawns in-round", r.round, freezeCause(idle, spawn))
		applied, err := RetireFrozenAgentCause(ctx, pool, r.agentID, r.taskID, note, CauseTrueFreeze, func(tx pgx.Tx) error {
			return insertSalvagedFindingsTx(ctx, tx, r.taskID, r.agentID, salvage)
		})
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
		// ADR-0008: a silent-success fixer (turn ended without done) re-arms
		// its episode for free — no park, no budget burn. A true freeze
		// keeps the circuit breaker.
		if r.cause == CauseSilentSuccess {
			note := fmt.Sprintf("watchdog: fixer turn-ended without done (round %d) — %s; auto-retired, fix episode re-armed (no respawn budget burned)", r.round, freezeCause(idle, spawn))
			applied, err := RetireFrozenAgentCause(ctx, pool, r.agentID, r.taskID, note, CauseSilentSuccess, func(tx pgx.Tx) error {
				return releaseFixEpisodeTx(ctx, tx, r.taskID)
			})
			if err != nil {
				return err
			}
			if applied {
				log.Printf("pipeline: dispatch: watchdog retired silent-success fixer %s on %s — episode re-armed (budget untouched)", r.agentID, r.taskID)
				killReviewerPane(sessionName, r.session, r.window, killWindow)
			}
			continue
		}
		spent, err := CountFreezeRetires(ctx, pool, r.taskID, fmt.Sprintf("watchdog: fixer frozen (round %d)", r.round))
		if err != nil {
			return err
		}
		if spent >= respawnCap {
			// Budget spent — park instead of re-arming. Same episode release
			// as the respawn arm, so a human flipping the task back to
			// changes_requested re-arms clean.
			note := fmt.Sprintf("watchdog: fixer frozen (round %d) — %s; %d episode re-arms already spent, parking needs-human", r.round, freezeCause(idle, spawn), spent)
			applied, err := RetireFrozenAgentCause(ctx, pool, r.agentID, r.taskID, note, CauseTrueFreeze, func(tx pgx.Tx) error {
				if err := releaseFixEpisodeTx(ctx, tx, r.taskID); err != nil {
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
				fan.notify(ctx, pool, r.taskID, fmt.Sprintf("Fixer froze repeatedly (round %d) — respawn cap (%d) reached, parked needs-human.", r.round, respawnCap))
			}
			continue
		}
		note := fmt.Sprintf("watchdog: fixer frozen (round %d) — %s; auto-retired, fix episode re-armed", r.round, freezeCause(idle, spawn))
		applied, err := RetireFrozenAgentCause(ctx, pool, r.agentID, r.taskID, note, CauseTrueFreeze, func(tx pgx.Tx) error {
			// Release the episode: fixerCandidatesSQL keys on the round's
			// fix row, so deleting it lets the next fixerPass mint a fresh
			// fixer (-rN) for the SAME round. The frozen agent's own fix
			// prompt row is already gone (RetireFrozenAgent drops its
			// undriven prompts), so the re-armed episode re-enqueues clean.
			return releaseFixEpisodeTx(ctx, tx, r.taskID)
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

// releaseFixEpisodeTx deletes the CURRENT round's fix row inside a freeze
// retire's tx — the episode release both fixer arms share.
// fixerCandidatesSQL keys on this row, so deleting it lets fixerPass mint a
// fresh fixer (-rN) for the SAME round; the frozen agent's own fix prompt
// row is already gone (RetireFrozenAgent drops undriven prompts), so the
// re-armed episode re-enqueues clean.
func releaseFixEpisodeTx(ctx context.Context, tx pgx.Tx, taskID string) error {
	var round int
	if err := tx.QueryRow(ctx,
		`SELECT review_rounds FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&round); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		DELETE FROM task_context
		WHERE task_id = $1 AND kind = 'fix' AND content = $2
	`, taskID, fmt.Sprintf("round %d", round))
	return err
}

// RetireFrozenAgent retires one frozen agent row: guarded status→dead (the
// exactly-once 🆘 dedup — only the transition's winner notifies), a
// task_context observation for downstream attempts, and deletion of the
// agent's undriven task prompts (the rationale lives on
// retireFrozenAgentTx). applied=false when the row raced to dead elsewhere
// (no notification).
func RetireFrozenAgent(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note string) (bool, error) {
	return retireFrozenAgentTxCause(ctx, pool, agentID, taskID, note, "", nil)
}

// RetireFrozenAgentCleanup is RetireFrozenAgent with a same-tx cleanup hook,
// for retires whose re-dispatch needs an episode-row release (the fixer
// arms delete the round's fix row with it) — atomic with the guarded
// retire, so a raced no-op never releases an episode it didn't win.
func RetireFrozenAgentCleanup(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note string, cleanup func(pgx.Tx) error) (bool, error) {
	return retireFrozenAgentTxCause(ctx, pool, agentID, taskID, note, "", cleanup)
}

// RetireFrozenAgentCause is RetireFrozenAgentCleanup with the ADR-0008
// freeze cause riding the observation row (task_context.cause):
// CauseSilentSuccess (no respawn budget burn) or CauseTrueFreeze (counts
// against the cap). Empty cause = unclassified (pre-ADR callers).
func RetireFrozenAgentCause(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note, cause string, cleanup func(pgx.Tx) error) (bool, error) {
	return retireFrozenAgentTxCause(ctx, pool, agentID, taskID, note, cause, cleanup)
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
func retireFrozenAgentTxCause(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note, cause string, extra func(pgx.Tx) error) (bool, error) {
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
		INSERT INTO task_context (task_id, agent_id, kind, content, cause)
		VALUES ($1, $2, 'observation', $3, NULLIF($4, ''))
	`, taskID, agentID, note, cause); err != nil {
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
