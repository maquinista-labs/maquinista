package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- fakes -------------------------------------------------------------

// fakeSpawner stands in for agentspawn.SpawnFresh: it records the dispatch
// params and optionally materializes the agents row the real spawner would
// (pane mechanics themselves are SpawnFresh's own tests' business — EX-03
// owns the DB surface).
type fakeSpawner struct {
	t         *testing.T
	pool      *pgxpool.Pool
	spawns    []ReviewSpawnParams
	insertRow bool
}

func (f *fakeSpawner) SpawnReviewer(_ context.Context, p ReviewSpawnParams) error {
	f.spawns = append(f.spawns, p)
	if f.insertRow {
		role := p.Role
		if role == "" {
			role = "reviewer"
		}
		execOK(f.t, f.pool, `
			INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
			                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
			VALUES ($1, 'sess', $1, $5, $2, 'running', $3, $4, $1, NOW(), NOW(), FALSE)
		`, p.AgentID, p.TaskID, p.RunnerType, p.WorktreePath, role)
	}
	return nil
}

// seedReviewTask creates a pipeline task in 'review' with a worktree.
func seedReviewTask(t *testing.T, pool *pgxpool.Pool, taskID, issueID, worktree string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, metadata)
		VALUES ($1, $2, 'review', $3, $4::jsonb)
	`, taskID, "task "+taskID, worktree, `{"ticket_issue_id":"`+issueID+`"}`)
}

// seedReviewer inserts a live reviewer agent bound to the task.
func seedReviewer(t *testing.T, pool *pgxpool.Pool, agentID, taskID string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ($1, 'sess', $1, 'reviewer', $2, 'running', 'pi', '/tmp/wt', $1, NOW(), NOW(), FALSE)
	`, agentID, taskID)
}

func taskCol(t *testing.T, pool *pgxpool.Pool, taskID, col string) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(),
		`SELECT `+col+` FROM tasks WHERE id = $1`, taskID).Scan(&v); err != nil {
		t.Fatalf("task %s col %s: %v", taskID, col, err)
	}
	return v
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// ---- pure units --------------------------------------------------------

func TestParseVerdict_Table(t *testing.T) {
	cases := []struct {
		text    string
		want    string
		wantOK  bool
		wantBad bool // HasMalformedVerdictLine
	}{
		{"VERDICT: approve\n", "approve", true, false},
		{"checked the diff\nVERDICT: request_changes\n", "request_changes", true, false},
		{"VERDICT: needs_human", "needs_human", true, false},
		{"  VERDICT:  approve  ", "approve", true, false},
		{"verdict: approve\n", "", false, false},      // case-sensitive prefix
		{"VERDICT:approved\n", "", false, true},       // missing space
		{"VERDICT: maybe\n", "", false, true},         // unknown value
		{"VERDICT: approve extra\n", "", false, true}, // trailing words
		{"no verdict here\nVERDICT-ish\n", "", false, false},
	}
	for _, c := range cases {
		got, ok := ParseVerdict(c.text)
		if ok != c.wantOK || got != c.want {
			t.Errorf("ParseVerdict(%q) = (%q,%v), want (%q,%v)", c.text, got, ok, c.want, c.wantOK)
		}
		if bad := HasMalformedVerdictLine(c.text); bad != c.wantBad {
			t.Errorf("HasMalformedVerdictLine(%q) = %v, want %v", c.text, bad, c.wantBad)
		}
	}
}

func TestResolveExec_Table(t *testing.T) {
	cases := []struct {
		cfgRunner, extrasRunner, class, high, std string
		wantRunner, wantModel                     string
	}{
		{"pi", "pi", "standard", "m-high", "m-std", "pi", "m-std"},
		{"pi", "pi", "high", "m-high", "m-std", "pi", "m-high"},
		{"pi", "pi", "high", "", "m-std", "pi", "m-std"},        // no high model configured
		{"pi", "", "high", "m-high", "m-std", "pi", "m-high"},   // extras runner empty → cfg
		{"pi", "codex", "standard", "", "", "codex", ""},        // empty model = runner's own chain
		{"pi", "pi", "weird", "m-high", "m-std", "pi", "m-std"}, // unknown class → std
	}
	for i, c := range cases {
		r, m := ResolveExec(c.cfgRunner, c.extrasRunner, c.class, c.high, c.std)
		if r != c.wantRunner || m != c.wantModel {
			t.Errorf("case %d: ResolveExec = (%q,%q), want (%q,%q)", i, r, m, c.wantRunner, c.wantModel)
		}
	}
}

// ---- DB-backed ---------------------------------------------------------

func TestMintReviewerID_BumpsSuffix(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "t9", "uuid-9", "/tmp/wt-t9")

	id, err := mintReviewerID(ctx, pool, "t9")
	if err != nil || id != "reviewer-t9" {
		t.Fatalf("fresh mint = (%q,%v), want (reviewer-t9,nil)", id, err)
	}
	seedReviewer(t, pool, "reviewer-t9", "t9")
	// A retired round-2 reviewer still blocks its id in the mint scan, but
	// only one live agent per task is allowed (uq_agents_task_live).
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-t9-r2', 'sess', 'reviewer-t9-r2', 'reviewer', 't9', 'dead',
		        'pi', '/tmp/wt', 'reviewer-t9-r2', NOW(), NOW(), FALSE)
	`)

	id, err = mintReviewerID(ctx, pool, "t9")
	if err != nil || id != "reviewer-t9-r3" {
		t.Fatalf("bumped mint = (%q,%v), want (reviewer-t9-r3,nil)", id, err)
	}
}

// TestZeroAuthor_RejectsSelfReview: a pathological author identity equal to
// the reviewer mint never spawns — the task parks in needs-human instead.
func TestZeroAuthor_RejectsSelfReview(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tz", "uuid-z", "/tmp/wt-z")
	// The author recorded a result under the id the mint would pick.
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tz', 'reviewer-tz', 'result', 'done')
	`)

	sp := &fakeSpawner{t: t, pool: pool}
	if err := dispatchPass(ctx, pool, sp); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 0 {
		t.Fatalf("spawns = %d, want 0 (zero-author must refuse)", len(sp.spawns))
	}
	if got := taskCol(t, pool, "tz", "status"); got != "pending_approval" {
		t.Fatalf("task status = %q, want pending_approval", got)
	}
}

// TestDispatch_SpawnsReviewerWithSoulAndBinding covers the happy dispatch:
// one spawn with the frozen template's exec hints, the round recorded, the
// prompt enqueued — and a second tick is a strict no-op (idempotency).
func TestDispatch_SpawnsReviewerWithSoulAndBinding(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "ta", "uuid-a", "/tmp/wt-ta")
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('ta', 'impl-ta', 'result', 'done')
	`)

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, sp); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
	p := sp.spawns[0]
	if p.AgentID != "reviewer-ta" || p.TaskID != "ta" || p.WorktreePath != "/tmp/wt-ta" {
		t.Fatalf("spawn params = %+v", p)
	}
	// Frozen extras contract (migration 035): default_runner pi.
	if p.RunnerType != "pi" {
		t.Fatalf("runner = %q, want pi (template extras)", p.RunnerType)
	}

	var role, taskID, status string
	if err := pool.QueryRow(ctx, `
		SELECT role, task_id, status FROM agents WHERE id = 'reviewer-ta'
	`).Scan(&role, &taskID, &status); err != nil {
		t.Fatalf("agents row: %v", err)
	}
	if role != "reviewer" || taskID != "ta" || status != "running" {
		t.Fatalf("agent row = (%q,%q,%q)", role, taskID, status)
	}

	if got := taskCol(t, pool, "ta", "review_rounds"); got != "1" {
		t.Fatalf("review_rounds = %q, want 1", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_inbox
		WHERE agent_id='reviewer-ta' AND external_msg_id='review:ta:1'
		AND origin_channel='task' AND content->>'type'='review'`); n != 1 {
		t.Fatalf("review prompt rows = %d, want 1", n)
	}

	// Second tick: everything already in place → strict no-op.
	before := len(sp.spawns)
	if err := dispatchPass(ctx, pool, sp); err != nil {
		t.Fatalf("dispatchPass 2: %v", err)
	}
	if len(sp.spawns) != before {
		t.Fatalf("second tick spawned %d extra reviewers", len(sp.spawns)-before)
	}
	if got := taskCol(t, pool, "ta", "review_rounds"); got != "1" {
		t.Fatalf("review_rounds after 2nd tick = %q, want 1", got)
	}
}

// TestDispatch_HealsMissingPrompt: crash between spawn and enqueue — the
// reviewer exists, the round prompt doesn't. promptPass enqueues exactly
// one, and repeats are no-ops.
func TestDispatch_HealsMissingPrompt(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tb", "uuid-b", "/tmp/wt-tb")
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1 WHERE id = 'tb'`)
	seedReviewer(t, pool, "reviewer-tb", "tb")

	for i := 0; i < 2; i++ {
		if err := promptPass(ctx, pool); err != nil {
			t.Fatalf("promptPass %d: %v", i, err)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_inbox
		WHERE agent_id='reviewer-tb' AND external_msg_id='review:tb:1'`); n != 1 {
		t.Fatalf("healed prompt rows = %d, want 1", n)
	}
}

// TestReviewRounds_IncrementsPerSpawn drives a full second round: verdict →
// changes_requested → (fixer requeue) review → fresh reviewer, rounds=2.
func TestReviewRounds_IncrementsPerSpawn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tc", "uuid-c", "/tmp/wt-tc")

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, sp); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}

	if _, _, err := applyVerdict(ctx, pool, "reviewer-tc", "tc", VerdictRequestChanges, "changes_requested", 3); err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if got := taskCol(t, pool, "tc", "status"); got != "changes_requested" {
		t.Fatalf("status = %q, want changes_requested", got)
	}

	// EX-04 fixer requeue (simulated): task returns to review.
	execOK(t, pool, `UPDATE tasks SET status = 'review' WHERE id = 'tc'`)
	if err := dispatchPass(ctx, pool, sp); err != nil {
		t.Fatalf("dispatchPass round 2: %v", err)
	}
	if len(sp.spawns) != 2 {
		t.Fatalf("spawns = %d, want 2", len(sp.spawns))
	}
	if sp.spawns[1].AgentID != "reviewer-tc-r2" {
		t.Fatalf("round-2 reviewer = %q, want reviewer-tc-r2", sp.spawns[1].AgentID)
	}
	if got := taskCol(t, pool, "tc", "review_rounds"); got != "2" {
		t.Fatalf("review_rounds = %q, want 2", got)
	}
}

// TestVerdictTransitions runs all three verdicts end-to-end through the
// verdict pass: outbox scan → task transition + verdict row + reviewer
// retired (live slot freed for the next round's mint).
func TestVerdictTransitions(t *testing.T) {
	cases := []struct {
		verdict, wantStatus string
	}{
		{VerdictApprove, "ready_to_merge"},
		{VerdictRequestChanges, "changes_requested"},
		{VerdictNeedsHuman, "pending_approval"},
	}
	for _, c := range cases {
		t.Run(c.verdict, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			taskID := "tv-" + c.verdict
			seedReviewTask(t, pool, taskID, "uuid-"+c.verdict, "/tmp/wt")
			seedReviewer(t, pool, "reviewer-"+taskID, taskID)
			execOK(t, pool, `
				INSERT INTO agent_outbox (agent_id, content)
				VALUES ('reviewer-`+taskID+`', '{"text":"working on it"}'::jsonb)
			`)
			execOK(t, pool, `
				INSERT INTO agent_outbox (agent_id, content)
				VALUES ('reviewer-`+taskID+`', $1::jsonb)
			`, `{"text":"findings...\nVERDICT: `+c.verdict+`\n"}`)

			if err := verdictPass(ctx, pool, 3, "sess", nil); err != nil {
				t.Fatalf("verdictPass: %v", err)
			}

			if got := taskCol(t, pool, taskID, "status"); got != c.wantStatus {
				t.Fatalf("status = %q, want %q", got, c.wantStatus)
			}
			var verdictContent, agentID string
			if err := pool.QueryRow(ctx, `
				SELECT content, agent_id FROM task_context
				WHERE task_id = $1 AND kind = 'verdict'
				ORDER BY created_at DESC LIMIT 1
			`, taskID).Scan(&verdictContent, &agentID); err != nil {
				t.Fatalf("verdict row: %v", err)
			}
			if verdictContent != "VERDICT: "+c.verdict || agentID != "reviewer-"+taskID {
				t.Fatalf("verdict row = (%q by %q)", verdictContent, agentID)
			}
			var status string
			if err := pool.QueryRow(ctx, `
				SELECT status FROM agents WHERE id = 'reviewer-`+taskID+`'
			`).Scan(&status); err != nil {
				t.Fatalf("reviewer row: %v", err)
			}
			if status != "dead" {
				t.Fatalf("reviewer status = %q, want dead", status)
			}
			// Live slot released: a fresh mint for the same task succeeds.
			if _, err := mintReviewerID(ctx, pool, taskID); err != nil {
				t.Fatalf("post-verdict mint: %v", err)
			}
		})
	}
}

// TestVerdict_MalformedWaits: a malformed VERDICT line must NOT transition
// the task — the watchdog is the backstop, not the parser.
func TestVerdict_MalformedWaits(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tm", "uuid-m", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tm", "tm")
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('reviewer-tm', '{"text":"VERDICT: approved-ish"}'::jsonb)
	`)

	if err := verdictPass(ctx, pool, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if got := taskCol(t, pool, "tm", "status"); got != "review" {
		t.Fatalf("status = %q, want review (malformed must wait)", got)
	}
}

// TestWatchdog_StallTimeout parks a reviewer with no outbox activity past
// the bound into needs-human; TestWatchdog_InsideTimeoutUntouched proves
// an active reviewer is left alone.
func TestWatchdog_StallTimeout(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tw", "uuid-w", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tw", "tw")

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "tw", "status"); got != "pending_approval" {
		t.Fatalf("stalled task status = %q, want pending_approval", got)
	}
	var content string
	if err := pool.QueryRow(ctx, `
		SELECT content FROM task_context WHERE task_id='tw' AND kind='verdict'
	`).Scan(&content); err != nil {
		t.Fatalf("watchdog verdict row: %v", err)
	}
	if !strings.Contains(content, "watchdog") {
		t.Fatalf("verdict content = %q, want watchdog note", content)
	}
}

func TestWatchdog_InsideTimeoutUntouched(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tx", "uuid-x", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tx", "tx")
	// Fresh outbox activity = reviewer is alive.
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('reviewer-tx', '{"text":"still reviewing"}'::jsonb)
	`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "tx", "status"); got != "review" {
		t.Fatalf("active task status = %q, want review", got)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-tx'`).Scan(&status); err != nil {
		t.Fatalf("reviewer row: %v", err)
	}
	if status != "running" {
		t.Fatalf("active reviewer status = %q, want running", status)
	}
}

// TestDerivedState_ReviewTransitions pins the new EX-03 arms; the existing
// TestSync_DerivedState covers the pre-change regression set.
func TestDerivedState_ReviewTransitions(t *testing.T) {
	cases := map[string]Column{
		"changes_requested": ColChangesRequested,
		"ready_to_merge":    ColReadyToMerge,
	}
	for status, want := range cases {
		got, ok := DerivedState(status)
		if !ok || got != want {
			t.Fatalf("DerivedState(%q) = (%v,%v), want (%v,true)", status, got, ok, want)
		}
	}
	if name := ColReadyToMerge.String(); name != "Ready to Merge" {
		t.Fatalf("ColReadyToMerge = %q, want Ready to Merge", name)
	}
}
