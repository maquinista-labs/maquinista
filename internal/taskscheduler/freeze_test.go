// MAQ-31 freeze arms: RetireFrozenClaims (implementor phase) and
// HealRestartCohort (crash-restart sweep). The freeze predicate itself is
// pinned in internal/pipeline's watchdog tests; these cover the scheduler
// arms and their re-dispatch hand-offs.
package taskscheduler

import (
	"context"
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

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, "maquinista", nil)
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
	if n := pipelineNotifyCount(t, pool, "frozen"); n != 1 {
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

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, "maquinista", nil)
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

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, "maquinista", nil)
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

	retired, err := RetireFrozenClaims(ctx, pool, 30*time.Minute, 10*time.Minute, "maquinista", nil)
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
	if n := pipelineNotifyCount(t, pool, "restart cohort"); n != 3 {
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
