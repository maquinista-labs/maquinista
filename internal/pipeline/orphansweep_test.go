package pipeline

// MAQ-41 proofs: pipeline worker rows never lose their task binding
// silently, and a fixer's completion always advances the state machine.
//
// The incident (MAQ-37, PR #44): a reround fixer finished its round on a
// CLEAN PR, but its agents row ended task_id=NULL + idle — the completion
// route (scripts/maquinista-done) releases agents worker-pool style — so
// every completion leg (all JOIN agents.task_id) went blind: no re-review,
// no verdict, no merge, the task wedged changes_requested for 16h+.
//
// Fix, three legs, each proven here:
//  1. fixer episodes CLAIM the task (claimed_by = the fixer) so the done
//     script's guarded UPDATE matches and flips the task back to review;
//  2. agentspawn refuses pipeline-role spawns without a TaskID (unit-tested
//     in the agentspawn package);
//  3. the dispatch tick's orphan sweep reconciles NULL-task_id rows —
//     backfill live ones, retire + re-arm (fixer) or retire-only completed
//     ones, retire + alert anything unrecoverable. Never silent.
//
// AC4 is the loop-closure proof: fixerPass → done-SQL (the script's exact
// statements) → dispatchPass → verdictPass lands ready_to_merge in two
// dispatch ticks.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// runDoneScriptSQL applies the exact statements scripts/maquinista-done
// runs on a passing turn (the completion route for implementors and
// fixers) — the production contract these fixes must satisfy.
func runDoneScriptSQL(t *testing.T, pool *pgxpool.Pool, taskID, agentID string) {
	t.Helper()
	execOK(t, pool, `
		UPDATE tasks SET
		  status = CASE WHEN metadata->>'ticket_issue_id' IS NOT NULL THEN 'review' ELSE 'done' END,
		  done_at=NOW(), claimed_by=NULL, claimed_at=NULL
		WHERE id=$1 AND claimed_by IN ($2, '@' || $2)
	`, taskID, agentID)
	execOK(t, pool, `
		UPDATE agents SET task_id=NULL, status='idle', last_seen=NOW()
		WHERE id=$1
	`, agentID)
}

// AC1: a reround-spawned fixer row carries the task binding.
func TestCommentRound_FixerRowHasTaskID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	seedReroundTask(t, pool, taskID, "pending_approval", "/tmp/wt-rr2", 1)
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")

	c := PRComment{ID: 911, Author: "alice", Body: "also fix Y", CreatedAt: time.Now()}
	if disp, err := DispatchCommentCommand(ctx, d, nil, 77, c); err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v, want ok", disp, err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
	var bound *string
	if err := pool.QueryRow(ctx,
		`SELECT task_id FROM agents WHERE id = $1`, sp.spawns[0].AgentID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound == nil || *bound != taskID {
		t.Fatalf("PASS-check: fixer row task_id = %v, want %s", bound, taskID)
	}
	t.Log("PASS TestCommentRound_FixerRowHasTaskID")
}

// The episode claim: fixerPass records the fix row AND claims the task for
// the fixer, so the done script's guarded UPDATE can match.
func TestFixerSpawn_ClaimsTaskForEpisode(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "fc", "uuid-fc", "/tmp/wt-fc", 2)
	// A stale pre-existing claim (the incident shape: the claim of a long
	// dead implementor still on the row) must be replaced, not respected.
	execOK(t, pool, `UPDATE tasks SET claimed_by = '@implementor-dead-r7' WHERE id = 'fc'`)

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
	var claimedBy *string
	if err := pool.QueryRow(ctx,
		`SELECT claimed_by FROM tasks WHERE id = 'fc'`).Scan(&claimedBy); err != nil {
		t.Fatal(err)
	}
	if claimedBy == nil || *claimedBy != sp.spawns[0].AgentID {
		t.Fatalf("PASS-check: claimed_by = %v, want %s", claimedBy, sp.spawns[0].AgentID)
	}
	t.Log("PASS TestFixerSpawn_ClaimsTaskForEpisode")
}

// AC4: the loop closes in two dispatch ticks — fixerPass claims + spawns
// (tick 1), the done script's SQL flips the task to review, dispatchPass
// mints the fresh reviewer and bumps the round, and the verdict pass lands
// ready_to_merge (tick 2). This is the MAQ-37 sequence that previously
// wedged forever.
func TestFixerCompletion_ReachesReadyToMergeInTwoTicks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f4", "uuid-f4", "/tmp/wt-f4", 1)
	execOK(t, pool, `UPDATE tasks SET claimed_by = '@implementor-stale' WHERE id = 'f4'`)

	// Tick 1: the fixer leg spawns the episode worker (and claims).
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("tick 1 fixerPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("tick 1 spawns = %d, want 1", len(sp.spawns))
	}
	fixerID := sp.spawns[0].AgentID

	// The fixer finishes: maquinista-done's exact SQL.
	runDoneScriptSQL(t, pool, "f4", fixerID)
	if got := taskCol(t, pool, "f4", "status"); got != "review" {
		t.Fatalf("after done: status = %q, want review (done-path branch)", got)
	}

	// Tick 2: the review spawn pass mints a FRESH reviewer (round bumps —
	// the fake spawner materialized reviewer-f4-r2's row), then the verdict
	// pass reads the approve and lands ready_to_merge.
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("tick 2 dispatchPass: %v", err)
	}
	if got := taskCol(t, pool, "f4", "review_rounds"); got != "2" {
		t.Fatalf("tick 2 review_rounds = %q, want 2", got)
	}
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('reviewer-f4-r2', '{"text":"all findings resolved\nVERDICT: approve\n"}'::jsonb)
	`)
	if err := verdictPass(ctx, pool, nil, 3, "sess", nil); err != nil {
		t.Fatalf("tick 2 verdictPass: %v", err)
	}
	if got := taskCol(t, pool, "f4", "status"); got != "ready_to_merge" {
		t.Fatalf("PASS-check: status = %q, want ready_to_merge", got)
	}
	t.Log("PASS TestFixerCompletion_ReachesReadyToMergeInTwoTicks")
}

// orphanSweepAlerts counts pipeline-topic outbox rows the sweep wrote for
// the given task — the exactly-once alert surface (the 🩹 backfills and 🆘
// re-arms/retries all carry the 'orphan sweep' note prefix).
func orphanSweepAlerts(t *testing.T, pool *pgxpool.Pool, taskID string) int {
	t.Helper()
	return count(t, pool, `
		SELECT count(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'text' LIKE '%orphan sweep%'
		  AND content->>'text' LIKE '%' || $1 || '%'
	`, taskID)
}

// AC3 (sweep): a mid-flight row with a NULL binding is backfilled — the
// completion legs see it again — with one observation + notification, and a
// second tick does not duplicate them.
func TestOrphanSweep_BackfillsLiveRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, metadata)
		VALUES ('ob', 'task ob', 'changes_requested', '/tmp/wt-ob', '{"ticket_issue_id":"u-ob"}'::jsonb)
	`)
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('fixer-ob', 'sess', 'fixer-ob', 'fixer', NULL, 'running',
		        'pi', '/tmp/wt-ob', 'fixer-ob', NOW(), NOW(), FALSE)
	`)

	if err := orphanSweepPass(ctx, pool); err != nil {
		t.Fatalf("orphanSweepPass: %v", err)
	}
	var taskID *string
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT task_id, status FROM agents WHERE id = 'fixer-ob'`).Scan(&taskID, &status); err != nil {
		t.Fatal(err)
	}
	if taskID == nil || *taskID != "ob" || status != "running" {
		t.Fatalf("PASS-check: row = (%v, %s), want (ob, running)", taskID, status)
	}
	if n := count(t, pool, `SELECT count(*) FROM task_context
		WHERE task_id='ob' AND kind='observation' AND content LIKE 'orphan sweep%'`); n != 1 {
		t.Fatalf("observations = %d, want 1", n)
	}
	if n := orphanSweepAlerts(t, pool, "ob"); n != 1 {
		t.Fatalf("notifications = %d, want 1", n)
	}

	// Second tick: the backfill is its own dedup — no new rows.
	if err := orphanSweepPass(ctx, pool); err != nil {
		t.Fatalf("second orphanSweepPass: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM task_context
		WHERE task_id='ob' AND kind='observation' AND content LIKE 'orphan sweep%'`); n != 1 {
		t.Fatalf("observations after 2nd tick = %d, want 1", n)
	}
	t.Log("PASS TestOrphanSweep_BackfillsLiveRow")
}

// AC3 (sweep), the incident shape: a COMPLETED fixer (idle, binding
// released by the done route) whose task still sits changes_requested on
// the current episode. The sweep restores the binding, retires the row,
// re-arms the episode (fix row released) and alerts exactly once — the
// next fixerPass then mints a fresh fixer instead of the task wedging.
func TestOrphanSweep_RearmsCompletedFixer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "oc", "uuid-oc", "/tmp/wt-oc", 3)
	// The fixer's episode was minted, its turn ran to completion via the
	// done route — but the task UPDATE missed (no claim, the MAQ-37 shape),
	// so the row is idle + unbound and the episode row is still there.
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('fixer-oc', 'sess', 'fixer-oc', 'fixer', NULL, 'idle',
		        'pi', '/tmp/wt-oc', 'fixer-oc', NOW(), NOW(), FALSE)
	`)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('oc', 'fixer-oc', 'fix', 'round 3')
	`)
	execOK(t, pool, `
		INSERT INTO agent_inbox (agent_id, from_kind, origin_channel, external_msg_id, content)
		VALUES ('fixer-oc', 'system', 'task', 'fix:oc:3', '{"type":"fix"}'::jsonb)
	`)

	if err := orphanSweepPass(ctx, pool); err != nil {
		t.Fatalf("orphanSweepPass: %v", err)
	}
	var taskID *string
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT task_id, status FROM agents WHERE id = 'fixer-oc'`).Scan(&taskID, &status); err != nil {
		t.Fatal(err)
	}
	if taskID == nil || *taskID != "oc" || status != "dead" {
		t.Fatalf("PASS-check: row = (%v, %s), want (oc, dead)", taskID, status)
	}
	if n := count(t, pool, `SELECT count(*) FROM task_context
		WHERE task_id='oc' AND kind='fix'`); n != 0 {
		t.Fatalf("fix rows = %d, want 0 (episode re-armed)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_inbox
		WHERE agent_id = 'fixer-oc' AND origin_channel = 'task'`); n != 0 {
		t.Fatalf("ghost prompts = %d, want 0", n)
	}
	if n := orphanSweepAlerts(t, pool, "oc"); n != 1 {
		t.Fatalf("notifications = %d, want 1 (exactly-once alert)", n)
	}

	// The un-wedge: the next fixer pass mints a FRESH fixer for the round.
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass after re-arm: %v", err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].AgentID != "fixer-oc-r2" {
		t.Fatalf("PASS-check: spawns = %+v, want fresh [fixer-oc-r2]", sp.spawns)
	}
	t.Log("PASS TestOrphanSweep_RearmsCompletedFixer")
}

// A completed row whose task already advanced is a benign remnant of the
// worker-pool release: binding restored for the record, row retired, NO
// alert (the episode is not stuck) and no episode release.
func TestOrphanSweep_CompletedRowTaskAdvancedStaysQuiet(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, metadata, review_rounds)
		VALUES ('oa', 'task oa', 'review', '/tmp/wt-oa', '{"ticket_issue_id":"u-oa"}'::jsonb, 2)
	`)
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('fixer-oa', 'sess', 'fixer-oa', 'fixer', NULL, 'idle',
		        'pi', '/tmp/wt-oa', 'fixer-oa', NOW(), NOW(), FALSE)
	`)

	if err := orphanSweepPass(ctx, pool); err != nil {
		t.Fatalf("orphanSweepPass: %v", err)
	}
	var taskID *string
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT task_id, status FROM agents WHERE id = 'fixer-oa'`).Scan(&taskID, &status); err != nil {
		t.Fatal(err)
	}
	if taskID == nil || *taskID != "oa" || status != "dead" {
		t.Fatalf("PASS-check: row = (%v, %s), want (oa, dead)", taskID, status)
	}
	if n := orphanSweepAlerts(t, pool, "oa"); n != 0 {
		t.Fatalf("notifications = %d, want 0 (task advanced — no wedge)", n)
	}
	t.Log("PASS TestOrphanSweep_CompletedRowTaskAdvancedStaysQuiet")
}

// A NULL-task_id pipeline row whose id embeds no recoverable task id is
// retired dead with exactly one pipeline-topic alert.
func TestOrphanSweep_RetiresUnrecoverableRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('fixer-notaskid', 'sess', 'w', 'fixer', NULL, 'running',
		        'pi', '/tmp', 'w', NOW(), NOW(), FALSE)
	`)

	if err := orphanSweepPass(ctx, pool); err != nil {
		t.Fatalf("orphanSweepPass: %v", err)
	}
	if got := agentStatus(t, pool, "fixer-notaskid"); got != "dead" {
		t.Fatalf("PASS-check: status = %q, want dead", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'text' LIKE '%orphan sweep%fixer-notaskid%'`); n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
	// Second tick: the retire is the dedup — no second alert.
	if err := orphanSweepPass(ctx, pool); err != nil {
		t.Fatalf("second orphanSweepPass: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'text' LIKE '%orphan sweep%fixer-notaskid%'`); n != 1 {
		t.Fatalf("alerts after 2nd tick = %d, want 1", n)
	}
	t.Log("PASS TestOrphanSweep_RetiresUnrecoverableRow")
}

// taskIDFromOrphanAgentID parses the mint shape (<role>-<taskID>[-rN]);
// non-mint ids are rejected so the sweep never guesses a binding.
func TestTaskIDFromOrphanAgentID(t *testing.T) {
	cases := []struct {
		id   string
		want string
		ok   bool
	}{
		{"fixer-6b44dc7e-4d7b-4dd0-bcf4-82edc87dea26", "6b44dc7e-4d7b-4dd0-bcf4-82edc87dea26", true},
		{"fixer-6b44dc7e-4d7b-4dd0-bcf4-82edc87dea26-r2", "6b44dc7e-4d7b-4dd0-bcf4-82edc87dea26", true},
		{"implementor-f8", "f8", true},
		{"reviewer-ob-r12", "ob", true},
		{"merger-m1", "m1", true},
		{"blazing-comet-drift", "", false},
		{"t--1003896184686-11213", "", false},
	}
	for _, c := range cases {
		got, ok := taskIDFromOrphanAgentID(c.id)
		if ok != c.ok || got != c.want {
			t.Errorf("taskIDFromOrphanAgentID(%q) = (%q,%v), want (%q,%v)", c.id, got, ok, c.want, c.ok)
		}
	}
}
