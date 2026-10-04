package pipeline

// EX-04 fixer-loop proofs (C1–C9). House rule: every proof shows its PASS
// lines, not exit codes. DB-backed proofs use the package testcontainer
// harness (testPool, bridge_test.go).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
)

// seedFixEpisode creates a pipeline task in changes_requested with a
// worktree, a request_changes verdict row (agent = reviewer-<taskID>), and
// review_rounds — the exact post-verdict state the verdict pass leaves.
func seedFixEpisode(t *testing.T, pool *pgxpool.Pool, taskID, issueID, worktree string, rounds int) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, metadata, review_rounds)
		VALUES ($1, $2, 'changes_requested', $3, $4::jsonb, $5)
	`, taskID, "task "+taskID, worktree, `{"ticket_issue_id":"`+issueID+`"}`, rounds)
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-`+taskID+`', 'sess', 'reviewer-`+taskID+`', 'reviewer', $1, 'dead',
		        'pi', $2, 'reviewer-`+taskID+`', NOW(), NOW(), FALSE)
	`, taskID, worktree)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, 'reviewer-`+taskID+`', 'verdict', 'VERDICT: request_changes')
	`, taskID)
	// The reviewer's final message: findings + verdict (the same row
	// latestVerdict parses; the fix prompt embeds it).
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('reviewer-`+taskID+`', $1::jsonb)
	`, `{"text":"1) tests missing\n2) checks.md claim unproven\nVERDICT: request_changes\n"}`)
}

func seedFixer(t *testing.T, pool *pgxpool.Pool, agentID, taskID string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ($1, 'sess', $1, 'fixer', $2, 'running', 'pi', '/tmp/wt', $1, NOW(), NOW(), FALSE)
	`, agentID, taskID)
}

// C1 (AC 1): the happy fixer spawn — role fixer, pipeline-fixer template,
// task worktree as cwd, exec hints from the frozen extras.
func TestFixerSpawn_SpawnsForChangesRequested(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f1", "uuid-f1", "/tmp/wt-f1", 1)

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1 (PASS: fixer spawned for changes_requested)", len(sp.spawns))
	}
	p := sp.spawns[0]
	if p.AgentID != "fixer-f1" || p.Role != "fixer" || p.SoulTemplateID != FixerSoulTemplate ||
		p.TaskID != "f1" || p.WorktreePath != "/tmp/wt-f1" {
		t.Fatalf("PASS-check spawn params = %+v", p)
	}
	if p.RunnerType != "pi" {
		t.Fatalf("runner = %q, want pi (pipeline-fixer extras)", p.RunnerType)
	}

	// The spawner materializes the row: role fixer, task-bound.
	var role, taskID string
	if err := pool.QueryRow(ctx,
		`SELECT role, task_id FROM agents WHERE id = 'fixer-f1'`).Scan(&role, &taskID); err != nil {
		t.Fatalf("fixer agents row: %v", err)
	}
	if role != "fixer" || taskID != "f1" {
		t.Fatalf("PASS-check agent row = (%q,%q), want (fixer,f1)", role, taskID)
	}
	t.Log("PASS TestFixerSpawn_SpawnsForChangesRequested")
}

// C1b (AC 1): a live FIXER on the task blocks another fixer spawn
// (role-scoped by design — a reviewer pane the verdict pass failed to kill
// must not wedge the loop; cross-role collisions rely on the unique-live
// index + next-tick retry).
func TestFixerSpawn_SkipsWhenLiveAgent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f2", "uuid-f2", "/tmp/wt-f2", 1)
	seedFixer(t, pool, "fixer-f2", "f2")

	sp := &fakeSpawner{t: t, pool: pool}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 0 {
		t.Fatalf("PASS-check: spawns = %d, want 0 (live fixer blocks)", len(sp.spawns))
	}
	t.Log("PASS TestFixerSpawn_SkipsWhenLiveAgent")
}

// C2 (AC 2): exactly one fix prompt (dedup id fix:<task>:<round>) embedding
// the findings text + the fix episode row.
func TestFixerSpawn_EnqueuesFixPromptOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f3", "uuid-f3", "/tmp/wt-f3", 2)

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_inbox
		WHERE agent_id='fixer-f3' AND external_msg_id='fix:f3:2'
		AND origin_channel='task' AND content->>'type'='fix'`); n != 1 {
		t.Fatalf("PASS-check: fix prompt rows = %d, want 1", n)
	}
	var prompt string
	if err := pool.QueryRow(ctx, `SELECT content->>'prompt' FROM agent_inbox
		WHERE agent_id='fixer-f3' AND external_msg_id='fix:f3:2'`).Scan(&prompt); err != nil {
		t.Fatalf("fix prompt row: %v", err)
	}
	if !strings.Contains(prompt, "tests missing") || !strings.Contains(prompt, "checks.md claim unproven") {
		t.Fatalf("PASS-check: prompt missing findings text: %q", prompt)
	}
	if !strings.Contains(prompt, "maquinista-done f3") {
		t.Fatalf("PASS-check: prompt missing done reminder: %q", prompt)
	}
	var fixContent string
	if err := pool.QueryRow(ctx, `SELECT content FROM task_context
		WHERE task_id='f3' AND kind='fix'`).Scan(&fixContent); err != nil {
		t.Fatalf("fix episode row: %v", err)
	}
	if fixContent != "round 2" {
		t.Fatalf("PASS-check: fix row = %q, want 'round 2'", fixContent)
	}
	t.Log("PASS TestFixerSpawn_EnqueuesFixPromptOnce")
}

// C4 (AC 4): the episode is consumed once — a second pass over the same
// changes_requested episode spawns nothing.
func TestFixerSpawn_EpisodeIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f4", "uuid-f4", "/tmp/wt-f4", 1)

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	before := len(sp.spawns)
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass 2: %v", err)
	}
	if len(sp.spawns) != before {
		t.Fatalf("PASS-check: second episode pass spawned %d extra fixers", len(sp.spawns)-before)
	}
	t.Log("PASS TestFixerSpawn_EpisodeIdempotent")
}

// C3 (AC 3): crash between spawn and enqueue — the heal enqueues exactly
// one prompt for the live fixer's episode; repeats are no-ops.
func TestFixerPrompt_HealsMissing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f5", "uuid-f5", "/tmp/wt-f5", 1)
	seedFixer(t, pool, "fixer-f5", "f5")

	for i := 0; i < 2; i++ {
		if err := fixerPass(ctx, pool, nil, &fakeSpawner{t: t, pool: pool}, DefaultImplementorIdleAfter); err != nil {
			t.Fatalf("fixerPass %d: %v", i, err)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_inbox
		WHERE agent_id='fixer-f5' AND external_msg_id='fix:f5:1'`); n != 1 {
		t.Fatalf("PASS-check: healed fix prompt rows = %d, want 1", n)
	}
	t.Log("PASS TestFixerPrompt_HealsMissing")
}

// C5 (AC 5): the round cap parks atomically — request_changes at rounds >=
// cap lands pending_approval with a cap-noting verdict row; below the cap it
// lands changes_requested (AC 6 regression).
func TestApplyVerdict_RoundCapParks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "f6", "uuid-f6", "/tmp/wt-f6")
	execOK(t, pool, `UPDATE tasks SET review_rounds = 3 WHERE id = 'f6'`)
	seedReviewer(t, pool, "reviewer-f6", "f6")

	landed, applied, err := applyVerdict(ctx, pool, "reviewer-f6", "f6", VerdictRequestChanges, "changes_requested", 3)
	if err != nil || !applied {
		t.Fatalf("applyVerdict = (%q,%v,%v), want applied", landed, applied, err)
	}
	if landed != "pending_approval" {
		t.Fatalf("PASS-check: landed = %q, want pending_approval", landed)
	}
	if got := taskCol(t, pool, "f6", "status"); got != "pending_approval" {
		t.Fatalf("PASS-check: status = %q, want pending_approval", got)
	}
	var content string
	if err := pool.QueryRow(ctx, `SELECT content FROM task_context
		WHERE task_id='f6' AND kind='verdict'`).Scan(&content); err != nil {
		t.Fatalf("verdict row: %v", err)
	}
	if !strings.Contains(content, "round cap 3") {
		t.Fatalf("PASS-check: verdict content = %q, want cap note", content)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-f6'`).Scan(&status); err != nil {
		t.Fatalf("reviewer row: %v", err)
	}
	if status != "dead" {
		t.Fatalf("PASS-check: reviewer status = %q, want dead (slot freed)", status)
	}
	t.Log("PASS TestApplyVerdict_RoundCapParks")
}

func TestApplyVerdict_UnderCapLandsChanges(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "f7", "uuid-f7", "/tmp/wt-f7")
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1 WHERE id = 'f7'`)
	seedReviewer(t, pool, "reviewer-f7", "f7")

	landed, applied, err := applyVerdict(ctx, pool, "reviewer-f7", "f7", VerdictRequestChanges, "changes_requested", 3)
	if err != nil || !applied {
		t.Fatalf("applyVerdict = (%q,%v,%v), want applied", landed, applied, err)
	}
	if landed != "changes_requested" || taskCol(t, pool, "f7", "status") != "changes_requested" {
		t.Fatalf("PASS-check: landed = %q, status = %q, want changes_requested", landed, taskCol(t, pool, "f7", "status"))
	}
	t.Log("PASS TestApplyVerdict_UnderCapLandsChanges")
}

// C7 (AC 7): the loop closes — a fixer-completed task re-enters review via
// the done-path branch (db.MarkDone, UNCHANGED from EX-03), and the next
// reviewer spawn mints a fresh reviewer id and bumps the round.
func TestMarkDone_FixerCompletedGoesToReview(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f8", "uuid-f8", "/tmp/wt-f8", 1)
	seedFixer(t, pool, "fixer-f8", "f8")
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('f8', 'fixer-f8', 'fix', 'round 1')
	`)
	// The fixer claimed the task the way a worker would.
	execOK(t, pool, `UPDATE tasks SET claimed_by = 'fixer-f8' WHERE id = 'f8'`)

	if err := db.MarkDone(pool, "f8", "fixer-f8", "fixed findings 1 and 2"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if got := taskCol(t, pool, "f8", "status"); got != "review" {
		t.Fatalf("PASS-check: status = %q, want review (loop re-entry)", got)
	}
	var doneAt, claimedBy *string
	if err := pool.QueryRow(ctx,
		`SELECT done_at::text, claimed_by FROM tasks WHERE id='f8'`).Scan(&doneAt, &claimedBy); err != nil {
		t.Fatalf("task row: %v", err)
	}
	if doneAt == nil || claimedBy != nil {
		t.Fatalf("PASS-check: done_at = %v, claimed_by = %v, want set/NULL", doneAt, claimedBy)
	}

	// Next round: the reviewer spawn pass mints a FRESH reviewer (never a
	// fixer id) and bumps the round.
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("PASS-check: spawns = %d, want 1", len(sp.spawns))
	}
	// Fresh mint for the task, reviewer role, NOT a fixer id (round 1's
	// reviewer-f8 row exists dead, so the suffix bumps — still a fresh pane).
	p := sp.spawns[0]
	if !strings.HasPrefix(p.AgentID, "reviewer-f8") || p.Role != "reviewer" {
		t.Fatalf("PASS-check: round-2 spawn = %+v, want fresh reviewer", p)
	}
	if got := taskCol(t, pool, "f8", "review_rounds"); got != "2" {
		t.Fatalf("PASS-check: review_rounds = %q, want 2", got)
	}
	t.Log("PASS TestMarkDone_FixerCompletedGoesToReview")
}

// C8 (AC 8): a stalled fixer parks the task with a watchdog note; an active
// one is untouched.
func TestFixerWatchdog_StallParks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "f9", "uuid-f9", "/tmp/wt-f9", 1)
	seedFixer(t, pool, "fixer-f9", "f9")
	// Backdate past the stall bound (young-agent guard exempts fresh agents).
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='fixer-f9'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "f9", "status"); got != "pending_approval" {
		t.Fatalf("PASS-check: stalled fix task = %q, want pending_approval", got)
	}
	var content string
	if err := pool.QueryRow(ctx, `SELECT content FROM task_context
		WHERE task_id='f9' AND kind='verdict'
		ORDER BY created_at DESC LIMIT 1`).Scan(&content); err != nil {
		t.Fatalf("watchdog row: %v", err)
	}
	if !strings.Contains(content, "fix stalled") {
		t.Fatalf("PASS-check: watchdog content = %q, want fix-stall note", content)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='fixer-f9'`).Scan(&status); err != nil {
		t.Fatalf("fixer row: %v", err)
	}
	if status != "dead" {
		t.Fatalf("PASS-check: fixer status = %q, want dead", status)
	}
	t.Log("PASS TestFixerWatchdog_StallParks")
}

// TestFixerWatchdog_TranscriptGrowthKeepsAlive pins the MAQ-9 liveness
// signal on the fixer arm: a fixer past the age guard, zero outbox rows,
// but with transcript growth inside the stall window (mid-command tool
// events) is healthy — untouched. The stallFilter is shared by both arms;
// this keeps the fixer side pinned in case the arms ever split.
func TestFixerWatchdog_TranscriptGrowthKeepsAlive(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "fb", "uuid-fb", "/tmp/wt-fb", 1)
	seedFixer(t, pool, "fixer-fb", "fb")
	execOK(t, pool, `
		UPDATE agents SET started_at = NOW() - interval '31 minutes',
		                   last_transcript_at = NOW() - interval '5 minutes'
		WHERE id='fixer-fb'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "fb", "status"); got != "changes_requested" {
		t.Fatalf("PASS-check: growing-transcript fix task = %q, want changes_requested (untouched)", got)
	}
	t.Log("PASS TestFixerWatchdog_TranscriptGrowthKeepsAlive")
}

func TestFixerWatchdog_InsideTimeoutUntouched(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "fa", "uuid-fa", "/tmp/wt-fa", 1)
	seedFixer(t, pool, "fixer-fa", "fa")
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('fixer-fa', '{"text":"fixing finding 1"}'::jsonb)
	`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "fa", "status"); got != "changes_requested" {
		t.Fatalf("PASS-check: active fix task = %q, want changes_requested (untouched)", got)
	}
	t.Log("PASS TestFixerWatchdog_InsideTimeoutUntouched")
}

// C9 (AC 9): the generalized resolver reads the named template's frozen
// extras — fixer (standard class) resolves to the std-model path.
func TestResolveTemplateExec_Fixer(t *testing.T) {
	pool := testPool(t)
	t.Setenv("MAQUINISTA_PI_MODEL", "m-std")
	t.Setenv("MAQUINISTA_PI_MODEL_HIGH", "m-high")

	runner, model, err := resolveTemplateExecFor(context.Background(), pool, FixerSoulTemplate)
	if err != nil {
		t.Fatalf("resolveTemplateExecFor(fixer): %v", err)
	}
	if runner != "pi" || model != "m-std" {
		t.Fatalf("PASS-check: fixer exec = (%q,%q), want (pi,m-std)", runner, model)
	}

	runner, model, err = resolveTemplateExecFor(context.Background(), pool, ReviewerSoulTemplate)
	if err != nil {
		t.Fatalf("resolveTemplateExecFor(reviewer): %v", err)
	}
	if runner != "pi" || model != "m-high" {
		t.Fatalf("PASS-check: reviewer exec = (%q,%q), want (pi,m-high) — unchanged", runner, model)
	}
	t.Log("PASS TestResolveTemplateExec_Fixer")
}

// C4b: a SECOND episode (new request_changes at a higher round) gets a new
// fixer — the fix row for round 1 must not block round 2.
func TestFixerSpawn_SecondEpisodeSpawns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedFixEpisode(t, pool, "fb", "uuid-fb", "/tmp/wt-fb", 2)
	// Episode 1 was consumed.
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('fb', 'fixer-fb', 'fix', 'round 1')
	`)

	sp := &fakeSpawner{t: t, pool: pool}
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	// Mint is suffix-per-existing-AGENT-row: episode 1's fixer exists only as
	// a fix row here, so the mint is fixer-fb. What the claim pins is ONE
	// spawn for the NEW episode.
	if len(sp.spawns) != 1 || sp.spawns[0].AgentID != "fixer-fb" {
		t.Fatalf("PASS-check: spawns = %+v, want exactly [fixer-fb]", sp.spawns)
	}
	if n := count(t, pool, `SELECT count(*) FROM task_context
		WHERE task_id='fb' AND kind='fix' AND content='round 2'`); n != 1 {
		t.Fatalf("PASS-check: round-2 fix rows = %d, want 1", n)
	}
	t.Log("PASS TestFixerSpawn_SecondEpisodeSpawns")
}
