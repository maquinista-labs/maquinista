// MAQ-31 freeze arms: RetireFrozenClaims (implementor phase) and
// HealRestartCohort (crash-restart sweep). The freeze predicate itself is
// pinned in internal/pipeline's watchdog tests; these cover the scheduler
// arms and their re-dispatch hand-offs.
package taskscheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedClaim inserts a claimed task with a worktree and a live implementor
// row (started/last_seen 39 minutes ago, zero outbox rows — frozen past the
// default bounds).
func seedClaim(t *testing.T, pool *pgxpool.Pool, taskID, agentID string) {
	t.Helper()
	dir := t.TempDir()
	exec(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, claimed_at, claimed_by)
		VALUES ($1, $4, 'claimed', $2, NOW() - INTERVAL '40 minutes', $3)
	`, taskID, dir, "@"+agentID, "task "+taskID)
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ($2, 'maquinista', $2, 'implementor', $1, 'running',
		        'pi', $3, $2, NOW() - INTERVAL '39 minutes', NOW() - INTERVAL '39 minutes', FALSE)
	`, taskID, agentID, dir)
}

// exec is the tiny helper the other tests in this package inline; errors
// fail the test.
func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func agentStatus(t *testing.T, pool *pgxpool.Pool, agentID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM agents WHERE id=$1`, agentID).Scan(&status); err != nil {
		t.Fatalf("agent %s: %v", agentID, err)
	}
	return status
}

func taskStatus(t *testing.T, pool *pgxpool.Pool, taskID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&status); err != nil {
		t.Fatalf("task %s: %v", taskID, err)
	}
	return status
}

func pipelineNotifyCount(t *testing.T, pool *pgxpool.Pool, needle string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'text' LIKE '%' || $1 || '%'
	`, needle).Scan(&n); err != nil {
		t.Fatalf("notify count: %v", err)
	}
	return n
}

// TestRetireFrozenClaims is the MAQ-27 victim shape: an implementor on a
// claimed task, running row, live pane, zero outbox rows for 39 minutes.
// The arm retires the row (🆘 exactly once); the next wake's reaper
// requeues the task to ready.
func TestRetireFrozenClaims(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "FZ", "impl-fz")

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	if got := agentStatus(t, pool, "impl-fz"); got != "dead" {
		t.Fatalf("frozen implementor status = %q, want dead", got)
	}
	if got := taskStatus(t, pool, "FZ"); got != "claimed" {
		t.Fatalf("task status = %q, want claimed (the reaper owns the requeue)", got)
	}
	if n := pipelineNotifyCount(t, pool, "went silent"); n != 1 {
		t.Fatalf("🆘 notes = %d, want exactly 1", n)
	}

	// The reaper hands off: requeues to ready for a fresh -rN spawn.
	reaped, err := ReapStaleClaims(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	if got := taskStatus(t, pool, "FZ"); got != "ready" {
		t.Fatalf("task status after reap = %q, want ready", got)
	}
}

// TestRetireFrozenClaims_ActiveUntouched is AC 2: an implementor with
// outbox activity younger than the idle bound is never touched, regardless
// of started_at age.
func TestRetireFrozenClaims_ActiveUntouched(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "FA", "impl-fa")
	exec(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('impl-fa', '{"text":"working the spec"}'::jsonb)
	`)

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("retired = %d, want 0 (fresh outbox = alive)", retired)
	}
	if got := agentStatus(t, pool, "impl-fa"); got != "running" {
		t.Fatalf("active implementor status = %q, want running", got)
	}
}

// TestRetireFrozenClaims_TranscriptGrowthUntouched pins the MAQ-9 veto on
// the implementor arm: a long silent command streams tool events, not
// outbox text — transcript growth inside the idle window is liveness.
func TestRetireFrozenClaims_TranscriptGrowthUntouched(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "FT", "impl-ft")
	exec(t, pool, `UPDATE agents SET last_transcript_at = NOW() - INTERVAL '5 minutes' WHERE id='impl-ft'`)

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("retired = %d, want 0 (transcript growth = alive)", retired)
	}
}

// TestRetireFrozenClaims_YoungUntouched: inside the spawn grace an agent
// with no signal on either channel is still a newborn — untouchable.
func TestRetireFrozenClaims_YoungUntouched(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "FY", "impl-fy")
	exec(t, pool, `
		UPDATE agents SET started_at = NOW() - INTERVAL '5 minutes',
		                  last_seen = NOW() - INTERVAL '5 minutes'
		WHERE id='impl-fy'
	`)

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("retired = %d, want 0 (spawn grace protects newborns)", retired)
	}
}

// TestHealRestartCohort is AC 3: restarting the unit with a frozen cohort
// present heals them within the first pass. Ghosts across all three
// re-dispatch shapes (claimed implementor, review reviewer, fixer episode)
// retire; a newborn is spared; the claimed task requeues via the reaper.
func TestHealRestartCohort(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	boot := time.Now()

	// Ghost 1: implementor on a claimed task (the crash-restart shape — a
	// graceful stop deletes task agents, so any survivor is a crash relic).
	seedClaim(t, pool, "GH", "impl-gh")
	exec(t, pool, `UPDATE agents SET last_seen = $1 WHERE id='impl-gh'`, boot.Add(-time.Minute))

	// Ghost 2: reviewer on a review task, never streamed.
	exec(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, metadata)
		VALUES ('GR', 'task GR', 'review', '/tmp/wt-gr', '{"ticket_issue_id":"gh-r"}'::jsonb)
	`)
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-gr', 'maquinista', 'reviewer-gr', 'reviewer', 'GR', 'running',
		        'pi', '/tmp/wt-gr', 'reviewer-gr', NOW() - INTERVAL '30 minutes', $1, FALSE)
	`, boot.Add(-time.Minute))

	// Ghost 3: fixer mid-episode with a fix row + undriven prompt.
	exec(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, review_rounds, metadata)
		VALUES ('GF', 'task GF', 'changes_requested', '/tmp/wt-gf', 1, '{"ticket_issue_id":"gh-f"}'::jsonb)
	`)
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('fixer-gf', 'maquinista', 'fixer-gf', 'fixer', 'GF', 'running',
		        'pi', '/tmp/wt-gf', 'fixer-gf', NOW() - INTERVAL '30 minutes', $1, FALSE)
	`, boot.Add(-time.Minute))
	exec(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('GF', 'fixer-gf', 'fix', 'round 1')
	`)
	exec(t, pool, `
		INSERT INTO agent_inbox (agent_id, from_kind, origin_channel, external_msg_id, content)
		VALUES ('fixer-gf', 'system', 'task', 'fix:GF:1', '{"type":"fix"}'::jsonb)
	`)

	// Newborn: spawned 2 minutes before the crash — not provably frozen.
	exec(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, claimed_at, claimed_by)
		VALUES ('GN', 'task GN', 'claimed', '/tmp/wt-gn', NOW() - INTERVAL '2 minutes', '@impl-gn')
	`)
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('impl-gn', 'maquinista', 'impl-gn', 'implementor', 'GN', 'running',
		        'pi', '/tmp/wt-gn', 'impl-gn', NOW() - INTERVAL '2 minutes', $1, FALSE)
	`, boot.Add(-time.Minute))

	healed, err := HealRestartCohort(ctx, pool, boot, 10*time.Minute, "maquinista", nil)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 3 {
		t.Fatalf("healed = %d, want 3 (all ghosts, newborn spared)", healed)
	}
	for _, agent := range []string{"impl-gh", "reviewer-gr", "fixer-gf"} {
		if got := agentStatus(t, pool, agent); got != "dead" {
			t.Fatalf("ghost %s status = %q, want dead", agent, got)
		}
	}
	if got := agentStatus(t, pool, "impl-gn"); got != "running" {
		t.Fatalf("newborn status = %q, want running (spawn grace)", got)
	}
	// The fix episode re-armed and the ghost's undriven prompt dropped.
	var fixRows, inboxRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM task_context WHERE task_id='GF' AND kind='fix'`).Scan(&fixRows); err != nil {
		t.Fatal(err)
	}
	if fixRows != 0 {
		t.Fatalf("fix rows = %d, want 0 (episode re-armed)", fixRows)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE agent_id='fixer-gf'`).Scan(&inboxRows); err != nil {
		t.Fatal(err)
	}
	if inboxRows != 0 {
		t.Fatalf("ghost prompt rows = %d, want 0", inboxRows)
	}
	// Every heal notified, exactly once each.
	if n := pipelineNotifyCount(t, pool, "leftover agent"); n != 3 {
		t.Fatalf("🆘 notes = %d, want 3", n)
	}

	// And the hand-offs: reaper requeues the claimed ghost's task.
	reaped, err := ReapStaleClaims(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1 (GH)", reaped)
	}
	if got := taskStatus(t, pool, "GH"); got != "ready" {
		t.Fatalf("GH status = %q, want ready", got)
	}
	if got := taskStatus(t, pool, "GR"); got != "review" {
		t.Fatalf("GR status = %q, want review (dispatchPass respawns)", got)
	}
	if got := taskStatus(t, pool, "GF"); got != "changes_requested" {
		t.Fatalf("GF status = %q, want changes_requested (fixerPass re-arms)", got)
	}
}

// TestHealRestartCohort_PostBootTranscriptSpared pins the sweep's MAQ-9
// veto: a pre-boot row (last_seen predates the boot, past the spawn grace,
// zero outbox rows ever) whose transcript GREW after the boot is a pane
// that survived the crash and is mid-turn — the monitor re-bound it and it
// streams tool events, not outbox text. Not a ghost; the sweep must leave
// it to the continuous arms.
func TestHealRestartCohort_PostBootTranscriptSpared(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	boot := time.Now()

	seedClaim(t, pool, "GS", "impl-gs")
	exec(t, pool, `
		UPDATE agents SET last_seen = $1,
		                  last_transcript_at = $2
		WHERE id='impl-gs'
	`, boot.Add(-time.Minute), boot.Add(time.Minute))

	healed, err := HealRestartCohort(ctx, pool, boot, 10*time.Minute, "maquinista", nil)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 0 {
		t.Fatalf("healed = %d, want 0 (post-boot transcript growth = alive)", healed)
	}
	if got := agentStatus(t, pool, "impl-gs"); got != "running" {
		t.Fatalf("streaming survivor status = %q, want running", got)
	}
	if n := pipelineNotifyCount(t, pool, "leftover agent"); n != 0 {
		t.Fatalf("🆘 notes = %d, want 0 (a spared agent is never notified)", n)
	}
}

// TestRetireFrozenClaims_RespawnCapParks pins the implementor arm's
// circuit breaker: a task whose respawn budget is spent parks needs-human
// atomically with the retire instead of requeueing — and the reaper leaves
// a parked task alone (the freeze→requeue→claim→freeze outage loop ends).
func TestRetireFrozenClaims_RespawnCapParks(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "FP", "impl-fp")
	// Three freeze cycles already spent (the observation ledger the guarded
	// retires wrote).
	for i := 0; i < 3; i++ {
		exec(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content, cause)
			VALUES ('FP', 'impl-fp', 'observation',
			        'watchdog: implementor impl-fp frozen — no outbox activity; auto-retired', 'true_freeze')
		`)
	}

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	if got := agentStatus(t, pool, "impl-fp"); got != "dead" {
		t.Fatalf("frozen implementor status = %q, want dead", got)
	}
	if got := taskStatus(t, pool, "FP"); got != "pending_approval" {
		t.Fatalf("task status = %q, want pending_approval (cap reached — no requeue)", got)
	}
	// The reaper must not touch the parked task.
	reaped, err := ReapStaleClaims(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 (parked tasks are out of the reaper's scope)", reaped)
	}
	// The park verdict names the reason, exactly once.
	var verdicts int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM task_context WHERE task_id='FP' AND kind='verdict'`).Scan(&verdicts); err != nil {
		t.Fatal(err)
	}
	if verdicts != 1 {
		t.Fatalf("verdict rows = %d, want 1", verdicts)
	}
}

// TestRun_RestartSweepSparesStreamingPane is the round-2 review headline
// fix, pinned end-to-end through Run: the sweep must NOT run at +0s — the
// monitor's first poll lands ~one poll interval after `go mon.Run`, so a
// boot-instant sweep sees only pre-boot transcripts, the boot-relative
// veto is dead code, and a mid-turn crash survivor is murdered on sight.
// Run defers the sweep a few monitor polls: a touch written during the
// grace window (the streaming survivor below) vetoes the heal, while an
// untouched ghost still sweeps on the same boot.
func TestRun_RestartSweepSparesStreamingPane(t *testing.T) {
	pool := setup(t)

	// The survivor: pre-boot implementor on a claimed task, zero outbox
	// rows. Its transcript was touched 5m before the boot (within the
	// continuous arm's idle window — so RetireFrozenClaims spares it
	// throughout), and the "monitor" touches it again 30ms after boot,
	// inside the sweep grace (3 × 200ms) — post-boot growth, veto fires.
	seedClaim(t, pool, "RS", "impl-rs")
	exec(t, pool, `UPDATE agents SET last_seen = $1, last_transcript_at = NOW() - INTERVAL '5 minutes' WHERE id='impl-rs'`,
		time.Now().Add(-time.Minute))
	go func() {
		time.Sleep(30 * time.Millisecond)
		exec(t, pool, `UPDATE agents SET last_transcript_at = NOW() WHERE id='impl-rs'`)
	}()

	// The ghost: pre-boot reviewer on a review task, silent on both
	// channels — reviewers are outside the scheduler's continuous arm, so
	// only the restart sweep can heal it in this Run.
	exec(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, metadata)
		VALUES ('RG', 'task RG', 'review', '/tmp/wt-rg', '{"ticket_issue_id":"gh-rg"}'::jsonb)
	`)
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-rg', 'maquinista', 'reviewer-rg', 'reviewer', 'RG', 'running',
		        'pi', '/tmp/wt-rg', 'reviewer-rg', NOW() - INTERVAL '30 minutes', $1, FALSE)
	`, time.Now().Add(-time.Minute))

	cfg := Config{
		PollInterval:        20 * time.Millisecond,
		MonitorPollInterval: 200 * time.Millisecond, // sweep grace = 600ms
		EnsureAgent: func(context.Context, string, string) (string, error) {
			return "", nil
		},
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(runCtx, pool, cfg) // ctx.Err on cancel is expected
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}

	// The streaming survivor is alive, its claim intact.
	if got := agentStatus(t, pool, "impl-rs"); got != "running" {
		t.Fatalf("streaming survivor status = %q, want running (post-boot transcript veto)", got)
	}
	if got := taskStatus(t, pool, "RS"); got != "claimed" {
		t.Fatalf("survivor task status = %q, want claimed", got)
	}
	// The true ghost swept exactly once — the deferral did not break AC 3.
	if got := agentStatus(t, pool, "reviewer-rg"); got != "dead" {
		t.Fatalf("ghost status = %q, want dead (sweep still heals)", got)
	}
	if n := pipelineNotifyCount(t, pool, "leftover agent"); n != 1 {
		t.Fatalf("🆘 notes = %d, want exactly 1", n)
	}
}

// TestHealRestartCohort_EmptyWhenClean: a graceful stop deletes task agents
// outright, so a clean restart sweeps nothing.
func TestHealRestartCohort_EmptyWhenClean(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	healed, err := HealRestartCohort(ctx, pool, time.Now(), 10*time.Minute, "maquinista", nil)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 0 {
		t.Fatalf("healed = %d, want 0 on a clean board", healed)
	}
}

// TestRetireFrozenClaims_NoteReportsPaneExistence: MAQ-38 AC 2 (the retire
// states whether a tmux pane existed for the retired id — name-based probe,
// so "no" (pane gone) and "yes" (pane present during an apparent freeze —
// the stale-id starvation signature from the 07/10 incident) are both
// diagnosable) restated under MAQ-37's split: the LEDGER note carries the
// machine fact verbatim; the 🆘 carries prose, and only the live-pane case
// (the operator-relevant one) surfaces in the sentence.
func TestRetireFrozenClaims_NoteReportsPaneExistence(t *testing.T) {
	ctx := context.Background()

	probe := func(probed *[]string) func(session, name string) bool {
		return func(session, name string) bool {
			*probed = append(*probed, session+":"+name)
			return name == "impl-yes" // pane live for one victim, gone for the other
		}
	}

	for _, tc := range []struct {
		agentID, wantPane, wantProse string
	}{
		{"impl-yes", "tmux pane for this id: yes", "even though its terminal pane was still open"},
		{"impl-no", "tmux pane for this id: no", "went silent"},
	} {
		pool := setup(t)
		seedClaim(t, pool, "PZ-"+tc.agentID, tc.agentID)
		var probed []string

		retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, probe(&probed), nil)
		if err != nil {
			t.Fatal(err)
		}
		if retired != 1 {
			t.Fatalf("%s: retired = %d, want 1", tc.agentID, retired)
		}
		// The probe asked for the retired id by NAME, in the row's session
		// (falls back to the session name when the row's own is empty).
		if len(probed) != 1 || probed[0] != "maquinista:"+tc.agentID {
			t.Fatalf("%s: probe = %v, want [maquinista:%s]", tc.agentID, probed, tc.agentID)
		}
		// The 🆘 (and its task_context observation twin) carries the fact.
		var note string
		if err := pool.QueryRow(ctx, `
			SELECT content FROM task_context
			WHERE task_id = $1 AND kind = 'observation'
		`, "PZ-"+tc.agentID).Scan(&note); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(note, tc.wantPane) {
			t.Fatalf("%s: observation = %q, want it to contain %q", tc.agentID, note, tc.wantPane)
		}
		// The 🆘 never repeats the ledger's machine phrase (MAQ-37); the
		// live-pane contradiction is the one fact worth prose.
		if n := pipelineNotifyCount(t, pool, tc.wantPane); n != 0 {
			t.Fatalf("%s: 🆘 notes carrying machine phrase %q = %d, want 0", tc.agentID, tc.wantPane, n)
		}
		if n := pipelineNotifyCount(t, pool, tc.wantProse); n != 1 {
			t.Fatalf("%s: 🆘 notes carrying %q = %d, want exactly 1", tc.agentID, tc.wantProse, n)
		}
	}
}

// TestRetireFrozenClaims_NilPaneProbeShipsUnknown: a caller with no tmux
// access still retires (and notifies) — the ledger note says "unknown"
// instead of inventing a pane fact, and the 🆘 makes no pane claim at all
// (MAQ-37: no machine vocabulary in prose).
func TestRetireFrozenClaims_NilPaneProbeShipsUnknown(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seedClaim(t, pool, "PUNK", "impl-punk")

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "maquinista", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired = %d, want 1", retired)
	}
	var note string
	if err := pool.QueryRow(ctx, `
		SELECT content FROM task_context
		WHERE task_id = 'PUNK' AND kind = 'observation'
	`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "tmux pane for this id: unknown") {
		t.Fatalf("observation = %q, want the unknown pane fact in the ledger", note)
	}
	if n := pipelineNotifyCount(t, pool, "pane"); n != 0 {
		t.Fatalf("🆘 notes mentioning a pane = %d, want 0 (unknown → no claim)", n)
	}
}
