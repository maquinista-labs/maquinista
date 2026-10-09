// ADR-0008 F3 tests: the review legs' one-shot completion nudge and the
// cause-aware split of the reviewer/fixer watchdog arms. The shared
// consume helper's contract is pinned in internal/taskscheduler's
// nudge_ledger_test.go; these cover the dispatch-side legs, the freeze
// exclusion, and the budget semantics at the cap.
package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedTurnEndedReviewer inserts a review task + live reviewer whose turn
// ended `turnAge` ago with transcript growth predating the turn end (the
// silent-success signature: the last observable event was a clean turn end).
func seedTurnEndedReviewer(t *testing.T, pool *pgxpool.Pool, taskID, issueID, agentID string, turnAge time.Duration) {
	t.Helper()
	seedReviewTask(t, pool, taskID, issueID, "/tmp/wt")
	seedReviewer(t, pool, agentID, taskID)
	execOK(t, pool, `
		UPDATE agents
		SET started_at = NOW() - interval '40 minutes',
		    last_turn_end_at = NOW() - make_interval(secs => $1),
		    last_transcript_at = NOW() - make_interval(secs => $2)
		WHERE id = $3
	`, turnAge.Seconds(), (turnAge + 5*time.Minute).Seconds(), agentID)
}

// inboxRows counts an agent's inbox rows by external_msg_id.
func inboxRows(t *testing.T, pool *pgxpool.Pool, agentID, msgID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM agent_inbox WHERE agent_id = $1 AND external_msg_id = $2
	`, agentID, msgID).Scan(&n); err != nil {
		t.Fatalf("inbox rows: %v", err)
	}
	return n
}

// latestObservationCause reads the newest freeze observation's cause.
func latestObservationCause(t *testing.T, pool *pgxpool.Pool, taskID string) string {
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

// TestNudgePass_ReviewerOneShot pins the review leg: a reviewer whose turn
// ended without a VERDICT line gets exactly one nudge — later passes and
// fresh turn ends never re-nudge within the round.
func TestNudgePass_ReviewerOneShot(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedTurnEndedReviewer(t, pool, "nz", "uuid-nz", "reviewer-nz", 2*time.Minute)

	if err := nudgePass(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
		t.Fatalf("nudgePass: %v", err)
	}
	if n := inboxRows(t, pool, "reviewer-nz", "nudge:nz:reviewer-nz"); n != 1 {
		t.Fatalf("nudge rows = %d, want exactly 1", n)
	}
	// The nudge names the terminal action: the VERDICT line.
	var prompt string
	if err := pool.QueryRow(ctx, `
		SELECT content->>'prompt' FROM agent_inbox WHERE agent_id='reviewer-nz'
	`).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "VERDICT:") {
		t.Fatalf("reviewer nudge prompt = %q, want the VERDICT-line contract", prompt)
	}
	// Exactly once: subsequent passes and a fresh signal stay silent.
	for i := 0; i < 2; i++ {
		if err := nudgePass(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	execOK(t, pool, `UPDATE agents SET last_turn_end_at = NOW() WHERE id='reviewer-nz'`)
	if err := nudgePass(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := inboxRows(t, pool, "reviewer-nz", "nudge:nz:reviewer-nz"); n != 1 {
		t.Fatalf("nudge rows after re-signal = %d, want still 1 (one per round)", n)
	}
}

// TestNudgePass_FixerOneShot: same contract for the fixer leg, naming the
// done verb.
func TestNudgePass_FixerOneShot(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "nf", "uuid-nf", "/tmp/wt", 1)
	seedFixer(t, pool, "fixer-nf", "nf")
	execOK(t, pool, `
		UPDATE agents
		SET started_at = NOW() - interval '40 minutes',
		    last_turn_end_at = NOW() - interval '2 minutes',
		    last_transcript_at = NOW() - interval '7 minutes'
		WHERE id = 'fixer-nf'
	`)

	if err := nudgePass(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
		t.Fatalf("nudgePass: %v", err)
	}
	if n := inboxRows(t, pool, "fixer-nf", "nudge:nf:fixer-nf"); n != 1 {
		t.Fatalf("nudge rows = %d, want exactly 1", n)
	}
	var prompt string
	if err := pool.QueryRow(ctx, `
		SELECT content->>'prompt' FROM agent_inbox WHERE agent_id='fixer-nf'
	`).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "maquinista-done nf") {
		t.Fatalf("fixer nudge prompt = %q, want the done-verb contract", prompt)
	}
	if err := nudgePass(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := inboxRows(t, pool, "fixer-nf", "nudge:nf:fixer-nf"); n != 1 {
		t.Fatalf("nudge rows after second pass = %d, want 1", n)
	}
}

// TestNudgePass_FrozenExcluded: an agent past the freeze bounds belongs to
// the watchdog (which classifies cause), not the nudge.
func TestNudgePass_FrozenExcluded(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedTurnEndedReviewer(t, pool, "nx", "uuid-nx", "reviewer-nx", 45*time.Minute)

	if err := nudgePass(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := inboxRows(t, pool, "reviewer-nx", "nudge:nx:reviewer-nx"); n != 0 {
		t.Fatalf("nudge rows = %d, want 0 (frozen agents go to the watchdog)", n)
	}
}

// TestNudgePass_VerdictPromptedAgentUntouched: a reviewer whose turn ended
// but whose verdict already landed is retired by the verdict pass before
// the nudge ever matters; a live reviewer WITHOUT a turn-end signal (still
// working) gets nothing.
func TestNudgePass_VerdictPromptedAgentUntouched(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedTurnEndedReviewer(t, pool, "nu", "uuid-nu", "reviewer-nu", 2*time.Minute)
	// Fresh reviewer, mid-turn (no turn end yet): not a nudge candidate.
	seedReviewTask(t, pool, "nw", "uuid-nw", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-nw", "nw")

	if err := nudgePass(ctx, pool, 30*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := inboxRows(t, pool, "reviewer-nw", "nudge:nw:reviewer-nw"); n != 0 {
		t.Fatalf("mid-turn reviewer nudged = %d rows, want 0", n)
	}
	if n := inboxRows(t, pool, "reviewer-nu", "nudge:nu:reviewer-nu"); n != 1 {
		t.Fatalf("turn-ended reviewer nudge rows = %d, want 1", n)
	}
}

// TestWatchdog_SilentSuccessReviewerNoCapBurn is the F3 acceptance: a
// silent-success reviewer respawns in-round even with the true-freeze
// budget spent — no park, no burn, cause on the ledger.
func TestWatchdog_SilentSuccessReviewerNoCapBurn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedTurnEndedReviewer(t, pool, "ns", "uuid-ns", "reviewer-ns", 40*time.Minute)
	// Budget spent by true freezes.
	for i := 0; i < 3; i++ {
		execOK(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content, cause)
			VALUES ('ns', 'reviewer-ns', 'observation', 'watchdog: reviewer frozen (round 1)', 'true_freeze')
		`)
	}

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatal(err)
	}
	if got := agentStatus(t, pool, "reviewer-ns"); got != "dead" {
		t.Fatalf("status = %q, want dead (retired)", got)
	}
	if got := taskCol(t, pool, "ns", "status"); got != "review" {
		t.Fatalf("task status = %q, want review (respawn in-round, NOT parked)", got)
	}
	if got := latestObservationCause(t, pool, "ns"); got != CauseSilentSuccess {
		t.Fatalf("observation cause = %q, want silent_success", got)
	}
	// The pre-existing true_freeze ledger is untouched.
	var trueFreezes int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_context
		WHERE task_id='ns' AND kind='observation' AND cause='true_freeze'
	`).Scan(&trueFreezes); err != nil {
		t.Fatal(err)
	}
	if trueFreezes != 3 {
		t.Fatalf("true_freeze rows = %d, want 3 (silent success added none)", trueFreezes)
	}
}

// TestWatchdog_TrueFreezeReviewerStillParks: no turn end + spent cap =
// unchanged MAQ-31 circuit breaker.
func TestWatchdog_TrueFreezeReviewerStillParks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "nt", "uuid-nt", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-nt", "nt")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes',
	                                  last_transcript_at = NOW() - interval '31 minutes' WHERE id='reviewer-nt'`)
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1 WHERE id = 'nt'`)
	for i := 0; i < 3; i++ {
		execOK(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content, cause)
			VALUES ('nt', 'reviewer-nt', 'observation', 'watchdog: reviewer frozen (round 1)', 'true_freeze')
		`)
	}

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatal(err)
	}
	if got := taskCol(t, pool, "nt", "status"); got != "pending_approval" {
		t.Fatalf("task status = %q, want pending_approval (true freeze at cap parks)", got)
	}
	if got := latestObservationCause(t, pool, "nt"); got != CauseTrueFreeze {
		t.Fatalf("observation cause = %q, want true_freeze", got)
	}
}

// TestWatchdog_SilentSuccessFixerReArms: a silent-success fixer's episode
// re-arms without burning budget — the fix row releases, task stays
// changes_requested.
func TestWatchdog_SilentSuccessFixerReArms(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "nz2", "uuid-nz2", "/tmp/wt", 1)
	seedFixer(t, pool, "fixer-nz2", "nz2")
	execOK(t, pool, `
		UPDATE agents
		SET started_at = NOW() - interval '40 minutes',
		    last_turn_end_at = NOW() - interval '40 minutes',
		    last_transcript_at = NOW() - interval '45 minutes'
		WHERE id = 'fixer-nz2'
	`)
	// Budget spent — a true freeze would park here.
	for i := 0; i < 3; i++ {
		execOK(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content, cause)
			VALUES ('nz2', 'fixer-nz2', 'observation', 'watchdog: fixer frozen (round 1)', 'true_freeze')
		`)
	}

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatal(err)
	}
	if got := agentStatus(t, pool, "fixer-nz2"); got != "dead" {
		t.Fatalf("fixer status = %q, want dead", got)
	}
	if got := taskCol(t, pool, "nz2", "status"); got != "changes_requested" {
		t.Fatalf("task status = %q, want changes_requested (episode re-armed, NOT parked)", got)
	}
	if got := latestObservationCause(t, pool, "nz2"); got != CauseSilentSuccess {
		t.Fatalf("observation cause = %q, want silent_success", got)
	}
	// The episode's fix row was released for the next fixerPass.
	var fixRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_context WHERE task_id='nz2' AND kind='fix'
	`).Scan(&fixRows); err != nil {
		t.Fatal(err)
	}
	if fixRows != 0 {
		t.Fatalf("fix rows = %d, want 0 (episode re-armed)", fixRows)
	}
}

// TestFreezeCauseOf pins the classifier's truth table (ADR-0008): the last
// observable event being a clean turn end is a silent success; growth after
// the turn end, or no turn end at all, is a true freeze.
func TestFreezeCauseOf(t *testing.T) {
	turn := time.Now().Add(-40 * time.Minute)
	after := turn.Add(5 * time.Minute)
	before := turn.Add(-5 * time.Minute)

	cases := []struct {
		name                string
		turnEnd, transcript *time.Time
		want                string
	}{
		{"turn end, no transcript touch", &turn, nil, CauseSilentSuccess},
		{"turn end is the last event", &turn, &before, CauseSilentSuccess},
		{"turn end same instant as touch", &turn, &turn, CauseSilentSuccess},
		{"growth after the turn end (died mid-next-turn)", &turn, &after, CauseTrueFreeze},
		{"no turn end ever", nil, &after, CauseTrueFreeze},
		{"no signals at all", nil, nil, CauseTrueFreeze},
	}
	for _, tc := range cases {
		if got := FreezeCauseOf(tc.turnEnd, tc.transcript); got != tc.want {
			t.Errorf("%s: FreezeCauseOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}
