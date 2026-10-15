// MAQ-54 regression suite. Two contracts live here:
//
//  1. The silent-success fixer re-arm is a STATE TRANSITION, not a log
//     line. For a comment-reround episode (born from a park — the newest
//     verdict is approve/needs_human, never request_changes) the fixer
//     pass can never re-derive the leg: fixerCandidatesSQL JOIN-LATERALs
//     on the newest verdict being request_changes. The a00c5d33/MAQ-46
//     incident sat 28 minutes on changes_requested with zero live agents
//     because the re-arm released the fix row and hoped. The watchdog now
//     requeues such episodes to 'ready' inside the retire tx, and the task
//     scheduler re-derives within its normal tick.
//
//  2. The dispatch-liveness belt: a pipeline task in a spawn-expecting
//     state with no live agent and no spawn attempt past the bound fires
//     ONE needs-human note — so a dropped dispatch is loud even when the
//     drop happens below the transitions (dead scheduler, evaporated
//     worktree, a candidate query that stopped matching).
package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestWatchdog_SilentSuccessFixerSupersededRequeuesReady is THE MAQ-54
// regression (end-to-end): a silent-success fixer on a comment-reround
// episode (approve-superseded — the fixer leg cannot re-derive it) is
// retired AND the task requeues to ready with the claim released, in the
// retire tx. Within one scheduler tick the task is claimable again — never
// changes_requested with no live agent.
func TestWatchdog_SilentSuccessFixerSupersededRequeuesReady(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const taskID, fixerID = "tx54", "fixer-tx54"

	// The episode: changes_requested, round 2, worktree, ticket mapping.
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, review_rounds, metadata, claimed_by, claimed_at)
		VALUES ($1, 'task tx54', 'changes_requested', '/tmp/wt-tx54', 2,
		        '{"ticket_issue_id":"uuid-tx54"}'::jsonb, $2, NOW())
	`, taskID, fixerID)
	// The round-2 verdict author: an approve verdict (the incident's
	// approve-then-park-then-comment-reround shape) — no request_changes
	// verdict anywhere, so fixerCandidatesSQL can never re-select the task.
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-tx54', 'sess', 'reviewer-tx54', 'reviewer', $1, 'dead',
		        'pi', '/tmp/wt-tx54', 'reviewer-tx54', NOW(), NOW(), FALSE)
	`, taskID)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, 'reviewer-tx54', 'verdict', 'VERDICT: approve')
	`, taskID)
	// The claimed episode: fix row for the current round.
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'fix', 'round 2')
	`, taskID, fixerID)
	// The fixer: past the freeze bounds, turn end is the last observable
	// event — silent success.
	seedFixer(t, pool, fixerID, taskID)
	execOK(t, pool, `
		UPDATE agents
		SET started_at = NOW() - interval '40 minutes',
		    last_turn_end_at = NOW() - interval '40 minutes',
		    last_transcript_at = NOW() - interval '45 minutes'
		WHERE id = $1
	`, fixerID)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := agentStatus(t, pool, fixerID); got != "dead" {
		t.Fatalf("fixer status = %q, want dead (retired)", got)
	}
	// THE transition: claimable again within one tick — ready, unclaimed.
	if got := taskCol(t, pool, taskID, "status"); got != "ready" {
		t.Fatalf("task status = %q, want ready (requeue IS the re-arm)", got)
	}
	if got := taskCol(t, pool, taskID, "COALESCE(claimed_by, '')"); got != "" {
		t.Fatalf("claimed_by = %q, want empty (dead fixer's claim released)", got)
	}
	if got := latestObservationCause(t, pool, taskID); got != CauseSilentSuccess {
		t.Fatalf("observation cause = %q, want silent_success (no budget burn)", got)
	}
	var fixRows int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM task_context WHERE task_id=$1 AND kind='fix'`, taskID).Scan(&fixRows); err != nil {
		t.Fatal(err)
	}
	if fixRows != 0 {
		t.Fatalf("fix rows = %d, want 0 (episode released with the requeue)", fixRows)
	}
	// The ledger names the transition (kind verdict, like the implementor
	// arm's straight-to-review row).
	var requeueRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_context
		WHERE task_id=$1 AND kind='verdict' AND content LIKE 'silent_success:%ready%'
	`, taskID).Scan(&requeueRows); err != nil {
		t.Fatal(err)
	}
	if requeueRows != 1 {
		t.Fatalf("requeue ledger rows = %d, want 1", requeueRows)
	}
	// The fixer leg confirms it could never have re-derived this: no mint.
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 0 {
		t.Fatalf("fixerPass spawns = %d, want 0 (a ready task is the scheduler's leg, not the fixer pass's)", len(sp.spawns))
	}
}

// TestWatchdog_SilentSuccessFixerStandardEpisodeStaysWithFixer pins the
// other half of the split: a standard episode (newest verdict IS
// request_changes) keeps the fixer-role contract — task stays
// changes_requested, fix row released, fixerPass re-mints next tick.
func TestWatchdog_SilentSuccessFixerStandardEpisodeStaysWithFixer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "ts54", "uuid-ts54", "/tmp/wt-ts54", 2)
	seedFixer(t, pool, "fixer-ts54", "ts54")
	execOK(t, pool, `
		UPDATE agents
		SET started_at = NOW() - interval '40 minutes',
		    last_turn_end_at = NOW() - interval '40 minutes',
		    last_transcript_at = NOW() - interval '45 minutes'
		WHERE id = 'fixer-ts54'
	`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "ts54", "status"); got != "changes_requested" {
		t.Fatalf("task status = %q, want changes_requested (fixer re-claim leg)", got)
	}
	// End-to-end: the released episode re-mints a fresh fixer with a prompt.
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].AgentID != "fixer-ts54-r2" {
		t.Fatalf("fixer respawn = %+v, want exactly fixer-ts54-r2", sp.spawns)
	}
}

// seedLivenessTask inserts a pipeline task with backdated creation (the
// anchor floor for tasks no agent ever touched).
func seedLivenessTask(t *testing.T, pool *pgxpool.Pool, taskID, issueID, status string, createdAge string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, metadata, created_at)
		VALUES ($1, $5, $2, $6, $3::jsonb, NOW() - $4::interval)
	`, taskID, status, `{"ticket_issue_id":"`+issueID+`"}`, createdAge, "task "+taskID, "/tmp/wt-"+taskID)
}

// livenessAlerts counts the belt's notifications for a task — the
// task-stamped 🆘 rows notifyTaskf writes on each claim (marker rows are
// per-episode CLAIMS: they are deleted when a test re-arms the alarm, so
// the outbox count, not the marker count, is the alert ledger).
func livenessAlerts(t *testing.T, pool *pgxpool.Pool, taskID string) int {
	t.Helper()
	return count(t, pool, `
		SELECT count(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'task_id' = $1
		  AND content->>'text' LIKE '%dispatch was expected%'
	`, taskID)
}

// TestWatchdog_TrueFreezeFixerSupersededRequeuesReady: the same superseded
// shape through the TRUE-freeze arm — the requeue transition applies there
// too (the observation still carries cause=true_freeze, so the budget still
// burns; only the task disposition changes).
func TestWatchdog_TrueFreezeFixerSupersededRequeuesReady(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const taskID, fixerID = "tf54", "fixer-tf54"

	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, review_rounds, metadata, claimed_by, claimed_at)
		VALUES ($1, 'task tf54', 'changes_requested', '/tmp/wt-tf54', 2,
		        '{"ticket_issue_id":"uuid-tf54"}'::jsonb, $2, NOW())
	`, taskID, fixerID)
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-tf54', 'sess', 'reviewer-tf54', 'reviewer', $1, 'dead',
		        'pi', '/tmp/wt-tf54', 'reviewer-tf54', NOW(), NOW(), FALSE)
	`, taskID)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, 'reviewer-tf54', 'verdict', 'VERDICT: approve')
	`, taskID)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'fix', 'round 2')
	`, taskID, fixerID)
	seedFixer(t, pool, fixerID, taskID)
	// TRUE freeze: transcript grew AFTER the last turn end (a later turn
	// started and died mid-way) — no turn end is the last observable event.
	execOK(t, pool, `
		UPDATE agents
		SET started_at = NOW() - interval '45 minutes',
		    last_turn_end_at = NOW() - interval '44 minutes',
		    last_transcript_at = NOW() - interval '40 minutes'
		WHERE id = $1
	`, fixerID)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := agentStatus(t, pool, fixerID); got != "dead" {
		t.Fatalf("fixer status = %q, want dead", got)
	}
	if got := taskCol(t, pool, taskID, "status"); got != "ready" {
		t.Fatalf("task status = %q, want ready (superseded episode requeues)", got)
	}
	if got := latestObservationCause(t, pool, taskID); got != CauseTrueFreeze {
		t.Fatalf("observation cause = %q, want true_freeze (budget still burns)", got)
	}
}

// TestLiveness_DroppedDispatchAlertsOnce: a review task whose expected
// reviewer never spawned (no live agent, no spawn attempt past the bound)
// fires exactly one needs-human note; fresh leg activity re-arms the alarm
// for the NEXT drop.
func TestLiveness_DroppedDispatchAlertsOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedLivenessTask(t, pool, "lv1", "uuid-lv1", "review", "2 hours")

	// First pass: the drop is past the bound — one alert.
	if err := dispatchLivenessPass(ctx, pool, 10*time.Minute); err != nil {
		t.Fatalf("liveness pass: %v", err)
	}
	if got := livenessAlerts(t, pool, "lv1"); got != 1 {
		t.Fatalf("alerts = %d, want 1 (dropped dispatch reported)", got)
	}
	// Second pass, nothing changed: silent (once per drop episode) — and
	// exactly one claim marker stands guard.
	if err := dispatchLivenessPass(ctx, pool, 10*time.Minute); err != nil {
		t.Fatalf("liveness pass: %v", err)
	}
	if got := livenessAlerts(t, pool, "lv1"); got != 1 {
		t.Fatalf("alerts = %d, want 1 (no refire without new activity)", got)
	}
	if got := count(t, pool, `SELECT count(*) FROM task_context WHERE task_id='lv1' AND kind='liveness'`); got != 1 {
		t.Fatalf("claim markers = %d, want 1 (the episode's guard)", got)
	}
	// New leg activity (a re-arm's context row lands, the drop recurs):
	// backdate it past the bound and clear the marker — the alarm re-arms.
	execOK(t, pool, `
		INSERT INTO task_context (task_id, kind, content, created_at)
		VALUES ('lv1', 'observation', 'watchdog: reviewer retired — respawns in-round', NOW() - interval '2 hours')
	`)
	execOK(t, pool, `DELETE FROM task_context WHERE task_id='lv1' AND kind='liveness'`)
	if err := dispatchLivenessPass(ctx, pool, 10*time.Minute); err != nil {
		t.Fatalf("liveness pass: %v", err)
	}
	if got := livenessAlerts(t, pool, "lv1"); got != 2 {
		t.Fatalf("alerts = %d, want 2 (recurring drop re-arms the alarm)", got)
	}
}

// TestLiveness_HealthyShapesStaySilent pins the belt's silence: a live
// agent anywhere on the task, fresh leg activity, a designed resting state
// (ready_to_merge waits for the approve verb), and non-pipeline tasks are
// none of them drops.
func TestLiveness_HealthyShapesStaySilent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Live agent on an old task: someone is working it.
	seedLivenessTask(t, pool, "lh1", "uuid-lh1", "review", "2 hours")
	seedReviewer(t, pool, "reviewer-lh1", "lh1")

	// Fresh context activity, no agent yet: within the bound.
	seedLivenessTask(t, pool, "lh2", "uuid-lh2", "changes_requested", "2 hours")
	execOK(t, pool, `
		INSERT INTO task_context (task_id, kind, content)
		VALUES ('lh2', 'verdict', 'VERDICT: request_changes')
	`)

	// ready_to_merge is a designed resting state (approve verb).
	seedLivenessTask(t, pool, "lh3", "uuid-lh3", "ready_to_merge", "2 hours")

	// Not a pipeline task: no dispatch legs to drop.
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, created_at)
		VALUES ('lh4', 'plain task', 'ready', NOW() - interval '2 hours')
	`)

	if err := dispatchLivenessPass(ctx, pool, 10*time.Minute); err != nil {
		t.Fatalf("liveness pass: %v", err)
	}
	for _, id := range []string{"lh1", "lh2", "lh3", "lh4"} {
		if got := livenessAlerts(t, pool, id); got != 0 {
			t.Fatalf("task %s: alerts = %d, want 0 (not a dropped dispatch)", id, got)
		}
	}
}

// TestLiveness_ReadyTaskNoAgentFires covers the scheduler-side drop: a
// ready pipeline task no implementor ever claims (scheduler down, or the
// claim leg broken) gets loud.
func TestLiveness_ReadyTaskNoAgentFires(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedLivenessTask(t, pool, "lv2", "uuid-lv2", "ready", "2 hours")

	if err := dispatchLivenessPass(ctx, pool, 10*time.Minute); err != nil {
		t.Fatalf("liveness pass: %v", err)
	}
	if got := livenessAlerts(t, pool, "lv2"); got != 1 {
		t.Fatalf("alerts = %d, want 1", got)
	}
}
