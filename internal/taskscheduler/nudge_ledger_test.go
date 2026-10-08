// ADR-0008 F2 tests: the implementor-phase nudge leg and the cause-aware
// freeze ledger. The freeze predicate itself is pinned in freeze_test.go;
// these cover the turn-end nudge exactly-once contract and the
// silent_success vs true_freeze split of the respawn budget.
package taskscheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedTurnEndedClaim inserts a claimed task + live implementor whose turn
// ended `turnAge` ago with the transcript's last growth predating the turn
// end (the silent-success signature: the last observable event of the round
// was a clean turn end).
func seedTurnEndedClaim(t *testing.T, pool *pgxpool.Pool, taskID, agentID string, turnAge time.Duration) {
	t.Helper()
	seedClaim(t, pool, taskID, agentID)
	exec(t, pool, `
		UPDATE agents
		SET last_turn_end_at = NOW() - make_interval(secs => $1),
		    last_transcript_at = NOW() - make_interval(secs => $2)
		WHERE id = $3
	`, turnAge.Seconds(), (turnAge + 5*time.Minute).Seconds(), agentID)
}

// inboxCount counts the agent's undriven task-channel nudge prompts.
func inboxCount(t *testing.T, pool *pgxpool.Pool, agentID, msgID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM agent_inbox
		WHERE agent_id = $1 AND external_msg_id = $2
	`, agentID, msgID).Scan(&n); err != nil {
		t.Fatalf("inbox count: %v", err)
	}
	return n
}

// observationCause reads the cause of the task's single freeze observation.
func observationCause(t *testing.T, pool *pgxpool.Pool, taskID string) string {
	t.Helper()
	var cause *string
	if err := pool.QueryRow(context.Background(), `
		SELECT cause FROM task_context
		WHERE task_id = $1 AND kind = 'observation'
		ORDER BY created_at DESC LIMIT 1
	`, taskID).Scan(&cause); err != nil {
		t.Fatalf("observation row: %v", err)
	}
	if cause == nil {
		return ""
	}
	return *cause
}

// TestNudgeTurnEndedClaims_OneShotPerRound is the exactly-once contract:
// one nudge per round, ever — the guarded consume makes every later pass a
// no-op even when the monitor keeps re-signaling turn ends.
func TestNudgeTurnEndedClaims_OneShotPerRound(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedTurnEndedClaim(t, pool, "NZ", "impl-nz", 2*time.Minute)

	fired, err := NudgeTurnEndedClaims(ctx, pool, 30*time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if n := inboxCount(t, pool, "impl-nz", "nudge:NZ:impl-nz"); n != 1 {
		t.Fatalf("nudge inbox rows = %d, want exactly 1", n)
	}
	// The ⏰ announce fired exactly once with the nudge.
	if n := pipelineNotifyCount(t, pool, "one-shot completion nudge"); n != 1 {
		t.Fatalf("⏰ notes = %d, want exactly 1", n)
	}

	// Later passes are no-ops — the consume guard is spent for this round.
	for i := 0; i < 3; i++ {
		fired, err := NudgeTurnEndedClaims(ctx, pool, 30*time.Minute, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if fired != 0 {
			t.Fatalf("pass %d fired = %d, want 0 (one-shot per round)", i+2, fired)
		}
	}
	// Even a FRESH turn end (the agent worked after the nudge and stopped
	// again) does not re-nudge: agent rows are per-round mints.
	exec(t, pool, `UPDATE agents SET last_turn_end_at = NOW() WHERE id='impl-nz'`)
	fired, err = NudgeTurnEndedClaims(ctx, pool, 30*time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("fired after fresh turn end = %d, want 0 (one nudge per round)", fired)
	}
	if n := inboxCount(t, pool, "impl-nz", "nudge:NZ:impl-nz"); n != 1 {
		t.Fatalf("nudge inbox rows = %d, want still 1", n)
	}
}

// TestNudgeTurnEndedClaims_FreshRoundNudges pins the round scoping: the
// nudged flag is per agent row, and a fresh -rN mint starts un-nudged.
func TestNudgeTurnEndedClaims_FreshRoundNudges(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedTurnEndedClaim(t, pool, "NR", "impl-nr-r1", 2*time.Minute)
	if _, err := NudgeTurnEndedClaims(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	// Round 2: the old row retires (DispatchOne releases it on re-claim),
	// a new agent row mints, new signal, new nudge budget.
	exec(t, pool, `UPDATE agents SET status='dead' WHERE id='impl-nr-r1'`)
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested,
		                    last_turn_end_at, last_transcript_at)
		VALUES ('impl-nr-r2', 'maquinista', 'impl-nr-r2', 'implementor', 'NR', 'running',
		        'pi', '/tmp/wt-nr2', 'impl-nr-r2', NOW() - INTERVAL '20 minutes', NOW() - INTERVAL '20 minutes', FALSE,
		        NOW() - INTERVAL '2 minutes', NOW() - INTERVAL '3 minutes')
	`)
	fired, err := NudgeTurnEndedClaims(ctx, pool, 30*time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (fresh round = fresh nudge budget)", fired)
	}
	if n := inboxCount(t, pool, "impl-nr-r2", "nudge:NR:impl-nr-r2"); n != 1 {
		t.Fatalf("r2 nudge inbox rows = %d, want 1", n)
	}
}

// TestNudgeTurnEndedClaims_FrozenExcluded pins the arm split: an agent past
// the freeze bounds belongs to the retire arm (which classifies cause), not
// the nudge — a nudge into a corpse would be a wasted prompt.
func TestNudgeTurnEndedClaims_FrozenExcluded(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// Turn ended 45m ago, nothing since: nudge candidate by signal, freeze
	// candidate by the shared predicate — the freeze exclusion must win.
	seedTurnEndedClaim(t, pool, "NF", "impl-nf", 45*time.Minute)

	fired, err := NudgeTurnEndedClaims(ctx, pool, 30*time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0 (frozen agents go to the retire arm)", fired)
	}
	if n := inboxCount(t, pool, "impl-nf", "nudge:NF:impl-nf"); n != 0 {
		t.Fatalf("nudge inbox rows = %d, want 0", n)
	}
}

// TestNudgeVsRetireRace_SingleFire is the ADR acceptance race: the nudge
// leg and the freeze arm converging on the same agent (the boundary case)
// degrade to single fire on BOTH sides — one nudge row, one retire.
func TestNudgeVsRetireRace_SingleFire(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// The agent is frozen (45m silent) but nudgable-by-signal: run both
	// legs concurrently and repeatedly.
	seedTurnEndedClaim(t, pool, "NC", "impl-nc", 45*time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = NudgeTurnEndedClaims(ctx, pool, 30*time.Minute, 10*time.Minute)
		}()
		go func() {
			defer wg.Done()
			_, _ = RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
		}()
	}
	wg.Wait()

	if n := inboxCount(t, pool, "impl-nc", "nudge:NC:impl-nc"); n > 1 {
		t.Fatalf("nudge inbox rows = %d, want ≤ 1 (never a double fire)", n)
	}
	if got := agentStatus(t, pool, "impl-nc"); got != "dead" {
		t.Fatalf("agent status = %q, want dead (retired exactly once)", got)
	}
	var retires int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_context WHERE task_id='NC' AND kind='observation'
	`).Scan(&retires); err != nil {
		t.Fatal(err)
	}
	if retires != 1 {
		t.Fatalf("observation rows = %d, want 1 (single retire)", retires)
	}
}

// TestRetireFrozenClaims_SilentSuccessNoCapBurn is the headline ADR-0008
// acceptance: a silent success retires WITHOUT burning respawn budget —
// even with the true-freeze ledger spent, the task requeues to ready
// instead of parking needs-human (the r8 incident, replayed).
func TestRetireFrozenClaims_SilentSuccessNoCapBurn(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedTurnEndedClaim(t, pool, "SS", "impl-ss", 45*time.Minute)
	// The budget is GONE: three true freezes already burned this episode.
	for i := 0; i < 3; i++ {
		exec(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content, cause)
			VALUES ('SS', 'impl-ss', 'observation', 'watchdog: implementor frozen', 'true_freeze')
		`)
	}

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	if got := agentStatus(t, pool, "impl-ss"); got != "dead" {
		t.Fatalf("status = %q, want dead", got)
	}
	// NOT parked: a silent success is not a freeze failure. The reaper
	// requeues the claimed task the same wake.
	if got := taskStatus(t, pool, "SS"); got != "claimed" {
		t.Fatalf("task status = %q, want claimed (the reaper owns the requeue)", got)
	}
	reaped, err := ReapStaleClaims(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	if got := taskStatus(t, pool, "SS"); got != "ready" {
		t.Fatalf("task status after reap = %q, want ready", got)
	}
	// The ledger records the cause — and it is NOT true_freeze, so this
	// retire never appears in any future budget count.
	if got := observationCause(t, pool, "SS"); got != "silent_success" {
		t.Fatalf("observation cause = %q, want silent_success", got)
	}
	var trueFreezes int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_context
		WHERE task_id='SS' AND kind='observation' AND cause='true_freeze'
	`).Scan(&trueFreezes); err != nil {
		t.Fatal(err)
	}
	if trueFreezes != 3 {
		t.Fatalf("true_freeze rows = %d, want 3 (the silent success did not add one)", trueFreezes)
	}
}

// TestRetireFrozenClaims_SilentSuccessStraightToReview pins the artifacts
// path: PR open + branch up to date + ticket-mapped → the retire tx flips
// the task straight to 'review' (the MarkDone done-path shape), the reaper
// leaves it alone, and the dispatch loop's spawn pass takes over.
func TestRetireFrozenClaims_SilentSuccessStraightToReview(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedTurnEndedClaim(t, pool, "SR", "impl-sr", 45*time.Minute)
	exec(t, pool, `
		UPDATE tasks
		SET pr_url = 'https://github.com/o/r/pull/1', pr_state = 'open',
		    metadata = '{"ticket_issue_id":"iss-sr"}'::jsonb
		WHERE id = 'SR'
	`)
	upToDate := 0
	branchUpToDate := func(worktree string) bool {
		upToDate++
		return true
	}

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, branchUpToDate)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	if upToDate == 0 {
		t.Fatalf("branchUpToDate never consulted")
	}
	if got := taskStatus(t, pool, "SR"); got != "review" {
		t.Fatalf("task status = %q, want review (straight to review, no respawn)", got)
	}
	// The reaper must not touch a task the retire already transitioned.
	reaped, err := ReapStaleClaims(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 (already in review)", reaped)
	}
	if got := observationCause(t, pool, "SR"); got != "silent_success" {
		t.Fatalf("observation cause = %q, want silent_success", got)
	}
	// The transition is journaled for the downstream reviewer.
	var verdict string
	if err := pool.QueryRow(ctx, `
		SELECT content FROM task_context WHERE task_id='SR' AND kind='verdict'
	`).Scan(&verdict); err != nil {
		t.Fatalf("verdict row: %v", err)
	}
	if !strings.Contains(verdict, "straight to review") {
		t.Fatalf("verdict = %q, want the silent-success transition note", verdict)
	}
}

// TestRetireFrozenClaims_SilentSuccessNotCurrent requeues when the branch
// is NOT up to date: work exists but is not review-ready — a fresh round
// finishes it, without burning budget.
func TestRetireFrozenClaims_SilentSuccessNotCurrent(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedTurnEndedClaim(t, pool, "SN", "impl-sn", 45*time.Minute)
	exec(t, pool, `
		UPDATE tasks
		SET pr_url = 'https://github.com/o/r/pull/2', pr_state = 'open',
		    metadata = '{"ticket_issue_id":"iss-sn"}'::jsonb
		WHERE id = 'SN'
	`)
	branchUpToDate := func(worktree string) bool { return false }

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, branchUpToDate)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	if got := taskStatus(t, pool, "SN"); got != "claimed" {
		t.Fatalf("task status = %q, want claimed (reaper requeues)", got)
	}
	reaped, err := ReapStaleClaims(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	if got := taskStatus(t, pool, "SN"); got != "ready" {
		t.Fatalf("task status after reap = %q, want ready", got)
	}
}

// TestRetireFrozenClaims_TrueFreezeStillParks pins the other half of the
// ledger: with NO turn-end signal, the spent cap still parks needs-human —
// the circuit breaker keeps its teeth for genuine freezes.
func TestRetireFrozenClaims_TrueFreezeStillParks(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "TF", "impl-tf") // no turn-end signal at all
	for i := 0; i < 3; i++ {
		exec(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content, cause)
			VALUES ('TF', 'impl-tf', 'observation', 'watchdog: implementor frozen', 'true_freeze')
		`)
	}

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	if got := taskStatus(t, pool, "TF"); got != "pending_approval" {
		t.Fatalf("task status = %q, want pending_approval (true freeze at cap still parks)", got)
	}
	if got := observationCause(t, pool, "TF"); got != "true_freeze" {
		t.Fatalf("observation cause = %q, want true_freeze", got)
	}
}

// TestRetireFrozenClaims_TrueFreezeMidTurnAfterTurnEnd pins the classifier
// boundary: a turn end OLDER than later transcript growth means a later
// turn started and died mid-way — that is a TRUE freeze (budget burns,
// cap parks), not a silent success.
func TestRetireFrozenClaims_TrueFreezeMidTurnAfterTurnEnd(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "TM", "impl-tm")
	// Turn ended 40m ago; the nudge arrived and the transcript GREW 31m
	// ago (after the turn end, before the freeze bound), then hung. The
	// last observable event is not the turn end — a true freeze.
	exec(t, pool, `
		UPDATE agents
		SET last_turn_end_at = NOW() - INTERVAL '40 minutes',
		    last_transcript_at = NOW() - INTERVAL '31 minutes'
		WHERE id = 'impl-tm'
	`)

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	if got := observationCause(t, pool, "TM"); got != "true_freeze" {
		t.Fatalf("observation cause = %q, want true_freeze (transcript grew after the turn end)", got)
	}
}
