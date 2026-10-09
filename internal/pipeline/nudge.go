// One-shot completion nudge (ADR-0008 F3): when a pipeline agent's turn
// ends without the round's completion verb, the owning leg sends exactly
// ONE nudge per round — "your turn ended; finish". This file covers the
// review legs (reviewer rounds in 'review', fixer episodes in
// 'changes_requested'); the implementor phase's leg lives in
// internal/taskscheduler (same consume helper, role-specific prompt).
//
// Exactly-once, two independent guards:
//
//  1. The guarded UPDATE (pipeline.ConsumeTurnEnd): `UPDATE agents SET
//     turn_end_nudged = TRUE WHERE id=$1 AND NOT turn_end_nudged AND
//     status <> 'dead'` — the same single-winner pattern as the freeze
//     retire's status flip. Two passes, or the nudge racing a freeze
//     retire, degrade to a single fire: the loser's UPDATE matches zero
//     rows (already nudged) or is blocked by the status guard (retired).
//
//  2. The agent_inbox dedup key `nudge:<taskID>:<agentID>`: agent rows
//     are per-round mints (-rN), so the mailbox's unique
//     (origin_channel, external_msg_id) index caps the nudge at one per
//     round even across restarts, and the retire's undriven-prompt
//     deletion cleans up a nudge an agent never saw.
//
// The nudge never completes on the agent's behalf — `maquinista-done`
// (implementor/fixer) and the `VERDICT:` line (reviewer) remain the only
// accepted completion verbs. A nudged agent that stays silent flows into
// the freeze arm on schedule, where the ADR-0008 cause ledger classifies
// it silent_success (no respawn budget burn) instead of a true freeze.
package pipeline

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// nudgeCandidatesSQL: live review-leg agents whose turn ended without the
// round's completion verb and whose one-shot nudge is unconsumed. $1/$2
// are the shared freeze bounds (idle secs, spawn secs) — the freeze
// predicate is EXCLUDED here so an already-frozen agent goes to the
// watchdog arm (which classifies cause) rather than getting a pointless
// nudge in the same wake; at the boundary the two legs stay
// single-fire-safe via the consume guard. UNION of both roles keeps one
// pass per dispatch tick.
const nudgeCandidatesSQL = `
SELECT a.id, a.task_id, a.role
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE a.status <> 'dead'
  AND a.task_id IS NOT NULL
  AND a.last_turn_end_at IS NOT NULL
  AND NOT a.turn_end_nudged
  AND (
        (a.role = '` + reviewerRole + `' AND t.status = 'review')
     OR (a.role = '` + fixerRole + `' AND t.status = 'changes_requested')
      )
  AND t.metadata->>'ticket_issue_id' IS NOT NULL
  AND NOT (TRUE` + FreezeFilterSQL + `)`

// ConsumeTurnEnd is the one-shot guard shared by every nudge leg
// (ADR-0008): flips agents.turn_end_nudged with a guarded UPDATE — the
// single winner (RowsAffected=1) earned the right to send THE nudge for
// this agent's round. A raced pass loses on `NOT turn_end_nudged`; a
// raced freeze retire wins instead and the status guard blocks the nudge.
func ConsumeTurnEnd(ctx context.Context, pool *pgxpool.Pool, agentID string) (bool, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE agents SET turn_end_nudged = TRUE
		WHERE id = $1 AND NOT turn_end_nudged AND status <> 'dead'
	`, agentID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// nudgePass fires the review legs' one-shot completion nudges: a reviewer
// whose turn ended without a VERDICT line, a fixer whose turn ended
// without maquinista-done. Runs every dispatch tick, before the verdict
// pass; the consume guard keeps it exactly-once per round.
func nudgePass(ctx context.Context, pool *pgxpool.Pool, idle, spawn time.Duration) error {
	rows, err := pool.Query(ctx, nudgeCandidatesSQL, idle.Seconds(), spawn.Seconds())
	if err != nil {
		return err
	}
	type cand struct{ agentID, taskID, role string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.agentID, &c.taskID, &c.role); err != nil {
			rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cands {
		fired, err := FireNudge(ctx, pool, c.agentID, c.taskID, c.role)
		if err != nil {
			log.Printf("pipeline: dispatch: nudge: %s on %s: %v", c.agentID, c.taskID, err)
			continue
		}
		if fired {
			log.Printf("pipeline: dispatch: nudge: %s %s on %s — one-shot completion nudge sent", c.role, c.agentID, c.taskID)
		}
	}
	return nil
}

// FireNudge is one nudge leg's body, shared by the review legs (this
// package) and the implementor phase (internal/taskscheduler): consume the
// one-shot guard, then enqueue + announce. Returns fired=false when the
// guard was already consumed or the agent raced to dead — no double fire.
func FireNudge(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, role string) (bool, error) {
	consumed, err := ConsumeTurnEnd(ctx, pool, agentID)
	if err != nil || !consumed {
		return false, err
	}
	if err := enqueueNudge(ctx, pool, agentID, taskID, role); err != nil {
		return false, err
	}
	completion := "`maquinista-done`"
	if role == reviewerRole {
		completion = "the `VERDICT:` line"
	}
	log.Printf("pipeline: nudge: %s %s on %s — turn ended without %s; one-shot nudge sent", role, agentID, taskID, completion)
	notifyTaskf(ctx, pool, taskID, "⏰ %s: %s %s's turn ended without %s — one-shot completion nudge sent (ADR-0008). Silence past the freeze bound still retires the round.",
		taskTitle(ctx, pool, taskID), role, agentID, completion)
	return true, nil
}

// enqueueNudge delivers the round's single nudge prompt. The external_msg_id
// is per-agent (= per round, agent rows are per-round mints), so the mailbox
// dedup is the second exactly-once guard; a nudge the agent never drives is
// dropped by the retire's undriven-prompt deletion.
func enqueueNudge(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, role string) error {
	content, err := json.Marshal(map[string]any{
		"type":    "nudge",
		"task_id": taskID,
		"role":    role,
		"prompt":  nudgePromptBody(role, taskID),
	})
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, _, err := mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "pipeline",
		OriginChannel: "task",
		ExternalMsgID: "nudge:" + taskID + ":" + agentID,
		Content:       content,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// NudgePrompt is the one-shot completion nudge body (ADR-0008): the
// agent's turn ended without the round's completion verb; the nudge names
// the verb. It never completes on the agent's behalf.
func NudgePrompt(role, taskID string) string {
	if role == reviewerRole {
		return "⚠️ Completion nudge (automated, sent once per review round): your last turn ended, but the pipeline never received a verdict — " +
			"the state machine parses ONLY the final `VERDICT:` line of a reply; findings prose is invisible to it. " +
			"Post your reply now, ending with exactly one line and nothing after it: " +
			"`VERDICT: approve` or `VERDICT: request_changes` or `VERDICT: needs_human`."
	}
	return "⚠️ Completion nudge (automated, sent once per round): your last turn ended without the completion verb, " +
		"so the pipeline has nothing to process. If the work is finished, run the proofs and finish with: " +
		"maquinista-done " + taskID + " \"<summary>\". If it is not finished, continue where you left off."
}

func nudgePromptBody(role, taskID string) string {
	return NudgePrompt(role, taskID)
}
