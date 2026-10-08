// Implementor-phase completion nudge (ADR-0008 F2): when a claimed task's
// implementor ends its turn without `maquinista-done`, the scheduler sends
// exactly ONE nudge per round — "your turn ended; finish with
// maquinista-done". The review legs' twin lives in internal/pipeline
// (nudge.go); both share the consume helper (pipeline.ConsumeTurnEnd) and
// the prompt body (pipeline.NudgePrompt).
//
// Why a nudge at all: turn end is the first observable moment a round can
// be silently complete (the 08/10 MAQ-37 r8 incident — work done, PR clean,
// zero done verbs, 30m of dead latency before the watchdog reacted).
// Nudging at turn end gives the agent one round-trip to deliver the done
// verb before the freeze arm's 30m silence bound retires the round. The
// nudge never completes on the agent's behalf, and one nudge per round is
// structural: agents rows are per-round mints (-rN) and the guarded consume
// flips turn_end_nudged at most once per row.
//
// Ordering contract with the freeze arm: candidates EXCLUDE frozen agents
// (the shared FreezeFilterSQL), so a wake sees either a nudge (fresh turn
// end) or a retire (silence past the bound) — never a wasted nudge into a
// corpse. Run orders the nudge BEFORE RetireFrozenClaims for exactly that
// boundary: an agent that crossed the bound between passes is retired next
// wake as silent_success, not nudged twice.
package taskscheduler

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
)

// nudgeClaimsSQL: live implementor rows on claimed tasks whose turn ended
// without the done verb and whose one-shot nudge is unconsumed. $1/$2 are
// the shared freeze bounds (idle secs, spawn secs); the freeze predicate is
// excluded so frozen agents flow to the retire arm (which classifies cause)
// instead of getting a pointless nudge in the same wake.
const nudgeClaimsSQL = `
SELECT a.id, a.task_id
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE t.status = 'claimed'
  AND a.role = 'implementor'
  AND a.status IN (` + liveAgentStatusSQL + `)
  AND a.last_turn_end_at IS NOT NULL
  AND NOT a.turn_end_nudged
  AND NOT (TRUE` + pipeline.FreezeFilterSQL + `)`

// NudgeTurnEndedClaims fires the implementor phase's one-shot completion
// nudges. Runs every scheduler wake, BEFORE RetireFrozenClaims. Returns the
// number of nudges THIS call fired (each consumed its guard exactly once).
func NudgeTurnEndedClaims(ctx context.Context, pool *pgxpool.Pool, idle, spawn time.Duration) (int, error) {
	rows, err := pool.Query(ctx, nudgeClaimsSQL, idle.Seconds(), spawn.Seconds())
	if err != nil {
		return 0, err
	}
	type cand struct{ agentID, taskID string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.agentID, &c.taskID); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	fired := 0
	for _, c := range cands {
		ok, err := pipeline.FireNudge(ctx, pool, c.agentID, c.taskID, "implementor")
		if err != nil {
			log.Printf("taskscheduler: nudge: %s on %s: %v", c.agentID, c.taskID, err)
			continue
		}
		if ok {
			fired++
		}
	}
	return fired, nil
}
