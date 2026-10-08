package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	t              *testing.T
	pool           *pgxpool.Pool
	spawns         []ReviewSpawnParams
	insertRow      bool
	failUniqueLive bool // surface the uq_agents_task_live INSERT failure
}

func (f *fakeSpawner) SpawnReviewer(_ context.Context, p ReviewSpawnParams) error {
	if f.failUniqueLive {
		return fmt.Errorf("insert agent row: ERROR: duplicate key value violates unique constraint %q (SQLSTATE 23505)", "uq_agents_task_live")
	}
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

// seedImplementor inserts a live implementor agent bound to the task;
// startedAt is a raw SQL timestamptz expression (e.g. "NOW() - interval '1 hour'").
func seedImplementor(t *testing.T, pool *pgxpool.Pool, agentID, taskID, startedAt string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ($1, 'sess', $1, 'implementor', $2, 'running', 'pi', '/tmp/wt', $1, `+startedAt+`, NOW(), FALSE)
	`, agentID, taskID)
}

// seedOutboxRow gives the agent an assistant outbox row aged by the raw SQL
// interval expression (e.g. "30 minutes") — the monitor's liveness signal.
func seedOutboxRow(t *testing.T, pool *pgxpool.Pool, agentID, age string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content, created_at)
		VALUES ($1, '{"type":"text","text":"PR opened."}'::jsonb, NOW() - $2::interval)
	`, agentID, age)
}

// seedOutboxText is seedOutboxRow with arbitrary assistant text — the
// findings-writeup rows the MAQ-36 salvage scan reads.
func seedOutboxText(t *testing.T, pool *pgxpool.Pool, agentID, text, age string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"type": "text", "text": text})
	if err != nil {
		t.Fatalf("marshal outbox text: %v", err)
	}
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content, created_at)
		VALUES ($1, $2::jsonb, NOW() - $3::interval)
	`, agentID, string(raw), age)
}

func agentStatus(t *testing.T, pool *pgxpool.Pool, agentID string) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM agents WHERE id = $1`, agentID).Scan(&v); err != nil {
		t.Fatalf("agent %s: %v", agentID, err)
	}
	return v
}

// pipelineNotifications counts pipeline-topic outbox rows mentioning the
// stuck-implementor auto-retire — the exactly-once notification surface.
func pipelineNotifications(t *testing.T, pool *pgxpool.Pool, taskID string) int {
	t.Helper()
	return count(t, pool, `
		SELECT count(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'text' LIKE '%auto-retired%'
		  AND content->>'text' LIKE '%' || $1 || '%'
	`, taskID)
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
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
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
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
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
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
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
		if err := promptPass(ctx, pool, nil); err != nil {
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
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
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
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
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

			if err := verdictPass(ctx, pool, nil, parkFanout{}, 3, "sess", nil); err != nil {
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

	if err := verdictPass(ctx, pool, nil, parkFanout{}, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if got := taskCol(t, pool, "tm", "status"); got != "review" {
		t.Fatalf("status = %q, want review (malformed must wait)", got)
	}
}

// TestWatchdog_FrozenReviewerRetired is the MAQ-31 core regression: a
// reviewer past the spawn grace with zero outbox rows and no transcript
// growth (the 06/10 victim shape: spawned, never wired, live pane) is
// FROZEN — auto-retired within one pass while the task STAYS 'review' so
// the next dispatch tick respawns a fresh reviewer in-round. The old
// watchdog exempted every agent younger than the full stall timeout (2h)
// and parked needs-human; both behaviors are gone.
func TestWatchdog_FrozenReviewerRetired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tw", "uuid-w", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tw", "tw")
	// Past the 10m spawn grace, silent on both channels for 31m.
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='reviewer-tw'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	// Task stays review — the re-dispatch is a fresh reviewer, not a park.
	if got := taskCol(t, pool, "tw", "status"); got != "review" {
		t.Fatalf("frozen task status = %q, want review (respawns in-round)", got)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-tw'`).Scan(&status); err != nil {
		t.Fatalf("reviewer row: %v", err)
	}
	if status != "dead" {
		t.Fatalf("frozen reviewer status = %q, want dead", status)
	}
	// The freeze observation lands for downstream attempts.
	var note string
	if err := pool.QueryRow(ctx, `
		SELECT content FROM task_context WHERE task_id='tw' AND kind='observation'
	`).Scan(&note); err != nil {
		t.Fatalf("freeze observation row: %v", err)
	}
	if !strings.Contains(note, "watchdog") {
		t.Fatalf("observation content = %q, want watchdog note", note)
	}
	// 🆘 exactly once: a second pass must be a no-op.
	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("second watchdogPass: %v", err)
	}
	var notifies int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'text' LIKE '%watchdog%'
	`).Scan(&notifies); err != nil {
		t.Fatalf("notify count: %v", err)
	}
	if notifies != 1 {
		t.Fatalf("🆘 notifications = %d, want exactly 1 after two passes", notifies)
	}

	// And the re-dispatch: the next spawn pass mints a FRESH reviewer (-r2).
	sp := &fakeSpawner{t: t, pool: pool, insertRow: false}
	if err := dispatchPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].AgentID != "reviewer-tw-r2" {
		t.Fatalf("respawn = %+v, want exactly reviewer-tw-r2", sp.spawns)
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

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
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

// TestWatchdog_TranscriptGrowthKeepsAlive pins the MAQ-9 liveness signal:
// a reviewer past the spawn grace with ZERO outbox rows but recent transcript
// growth (last_transcript_at inside the idle window — a long `go test`
// streams tool events, not assistant text) is HEALTHY and must not be
// retired. Regression: 2026-10-02, the EX-07 round-3 reviewer spent minutes
// running the suite with zero outbox rows.
func TestWatchdog_TranscriptGrowthKeepsAlive(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tg", "uuid-g", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tg", "tg")
	// Past the spawn grace (PR #12), silent outbox, but the transcript grew
	// five minutes ago — mid-command liveness.
	execOK(t, pool, `
		UPDATE agents SET started_at = NOW() - interval '31 minutes',
		                   last_transcript_at = NOW() - interval '5 minutes'
		WHERE id='reviewer-tg'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "tg", "status"); got != "review" {
		t.Fatalf("growing-transcript task status = %q, want review (growth is liveness)", got)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-tg'`).Scan(&status); err != nil {
		t.Fatalf("reviewer row: %v", err)
	}
	if status != "running" {
		t.Fatalf("growing-transcript reviewer status = %q, want running", status)
	}
}

// TestWatchdog_StaleTranscriptStillRetired is the AC-2 regression: the
// liveness signal must not become an amnesty. A reviewer past the spawn
// grace whose transcript last grew BEFORE the idle window (grew once at
// spawn, silent since) and with zero outbox rows is truly frozen — retired.
func TestWatchdog_StaleTranscriptStillRetired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "ts", "uuid-s", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-ts", "ts")
	execOK(t, pool, `
		UPDATE agents SET started_at = NOW() - interval '62 minutes',
		                   last_transcript_at = NOW() - interval '31 minutes'
		WHERE id='reviewer-ts'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-ts'`).Scan(&status); err != nil {
		t.Fatalf("reviewer row: %v", err)
	}
	if status != "dead" {
		t.Fatalf("stale-transcript reviewer status = %q, want dead", status)
	}
}

// TestWatchdog_FrozenNewbornRetired is the exact MAQ-31 victim shape: an
// agent spawned 11 minutes ago with NO signal on either channel (never
// wired — prompt delivery lost, sidecar never attached) is frozen the
// moment the spawn grace lapses, not at the old 2h timeout.
func TestWatchdog_FrozenNewbornRetired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tn", "uuid-n", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tn", "tn")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '11 minutes' WHERE id='reviewer-tn'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-tn'`).Scan(&status); err != nil {
		t.Fatalf("reviewer row: %v", err)
	}
	if status != "dead" {
		t.Fatalf("frozen newborn status = %q, want dead (spawn grace lapsed)", status)
	}
}

// TestWatchdog_RespawnCapParks pins the freeze→respawn circuit breaker
// (round-2 review): a reviewer round whose respawn budget is spent parks
// the task needs-human on the next freeze — atomically with the retire
// (exactly-once 🆘 + verdict row) — instead of respawning forever. The
// budget is round-scoped: a fresh round starts clean even with the
// previous round's ledger full.
func TestWatchdog_RespawnCapParks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tc", "uuid-c", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tc", "tc")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='reviewer-tc'`)
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1 WHERE id = 'tc'`)
	// Three freeze cycles already spent this round — the observation rows
	// each guarded retire writes are the budget ledger.
	for i := 0; i < 3; i++ {
		execOK(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content)
			VALUES ('tc', 'reviewer-tc', 'observation',
			        'watchdog: reviewer frozen (round 1) — no outbox activity; auto-retired')
		`)
	}

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "tc", "status"); got != "pending_approval" {
		t.Fatalf("capped task status = %q, want pending_approval (circuit breaker)", got)
	}
	if got := agentStatus(t, pool, "reviewer-tc"); got != "dead" {
		t.Fatalf("capped reviewer status = %q, want dead", got)
	}
	var verdict string
	if err := pool.QueryRow(ctx, `
		SELECT content FROM task_context WHERE task_id='tc' AND kind='verdict'
	`).Scan(&verdict); err != nil {
		t.Fatalf("park verdict row: %v", err)
	}
	if !strings.Contains(verdict, "parking needs-human") {
		t.Fatalf("verdict = %q, want park reason", verdict)
	}
	// Exactly once: a second pass is a no-op (the retire guard), so the
	// park verdict never duplicates.
	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("second watchdogPass: %v", err)
	}
	var verdicts int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM task_context WHERE task_id='tc' AND kind='verdict'`).Scan(&verdicts); err != nil {
		t.Fatal(err)
	}
	if verdicts != 1 {
		t.Fatalf("verdict rows = %d, want 1 after two passes", verdicts)
	}

	// A fresh round starts with a fresh budget: the same freeze with the
	// task at round 2 respawns even though round 1's ledger is full.
	seedReviewTask(t, pool, "td", "uuid-d", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-td", "td")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='reviewer-td'`)
	execOK(t, pool, `UPDATE tasks SET review_rounds = 2 WHERE id = 'td'`)
	for i := 0; i < 3; i++ {
		execOK(t, pool, `
			INSERT INTO task_context (task_id, agent_id, kind, content)
			VALUES ('td', 'reviewer-td', 'observation',
			        'watchdog: reviewer frozen (round 1) — no outbox activity; auto-retired')
		`)
	}
	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass (td): %v", err)
	}
	if got := taskCol(t, pool, "td", "status"); got != "review" {
		t.Fatalf("fresh-round task status = %q, want review (budget is round-scoped)", got)
	}
	if got := agentStatus(t, pool, "reviewer-td"); got != "dead" {
		t.Fatalf("fresh-round reviewer status = %q, want dead (respawn path)", got)
	}
}

// TestWatchdog_FrozenFixerReArms: a frozen fixer (task changes_requested)
// is retired and its EPISODE re-armed — the round's fix row is released so
// the next fixer pass mints a fresh fixer with a working prompt (the
// frozen agent's shadowed inbox row is dropped).
func TestWatchdog_FrozenFixerReArms(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	dir := t.TempDir()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, review_rounds, metadata)
		VALUES ('tf', 'task tf', 'changes_requested', $1, 2, '{"ticket_issue_id":"uuid-f"}'::jsonb)
	`, dir)
	// A dead round-1 reviewer (the verdict author — the FK on agent_outbox
	// requires the row) whose findings the fresh fix prompt embeds.
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-tf', 'sess', 'reviewer-tf', 'reviewer', 'tf', 'dead',
		        'pi', $1, 'reviewer-tf', NOW(), NOW(), FALSE)
	`, dir)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tf', 'reviewer-tf', 'verdict', 'VERDICT: request_changes')
	`)
	// The verdict's findings text — what enqueueFixPrompt embeds in the
	// fresh fixer's prompt (latestFindings reads the verdict author's
	// newest outbox row).
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('reviewer-tf', '{"text":"1) tests missing\nVERDICT: request_changes\n"}'::jsonb)
	`)
	seedFixer(t, pool, "fixer-tf", "tf")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '45 minutes' WHERE id='fixer-tf'`)
	// The episode's fix row (blocks respawn) + the fixer's undriven prompt.
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tf', 'fixer-tf', 'fix', 'round 2')
	`)
	execOK(t, pool, `
		INSERT INTO agent_inbox (agent_id, from_kind, origin_channel, external_msg_id, content)
		VALUES ('fixer-tf', 'system', 'task', 'fix:tf:2', '{"type":"fix"}'::jsonb)
	`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='fixer-tf'`).Scan(&status); err != nil {
		t.Fatalf("fixer row: %v", err)
	}
	if status != "dead" {
		t.Fatalf("frozen fixer status = %q, want dead", status)
	}
	if got := taskCol(t, pool, "tf", "status"); got != "changes_requested" {
		t.Fatalf("task status = %q, want changes_requested (episode re-arms, no park)", got)
	}
	var fixRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM task_context WHERE task_id='tf' AND kind='fix'`).Scan(&fixRows); err != nil {
		t.Fatal(err)
	}
	if fixRows != 0 {
		t.Fatalf("fix rows = %d, want 0 (episode re-armed)", fixRows)
	}
	var inboxRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE agent_id='fixer-tf'`).Scan(&inboxRows); err != nil {
		t.Fatal(err)
	}
	if inboxRows != 0 {
		t.Fatalf("ghost prompt rows = %d, want 0 (undriven prompts dropped)", inboxRows)
	}

	// Re-dispatch: fixerPass mints a fresh fixer for the SAME round.
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].AgentID != "fixer-tf-r2" {
		t.Fatalf("fixer respawn = %+v, want exactly fixer-tf-r2", sp.spawns)
	}
	var prompts int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE agent_id='fixer-tf-r2'`).Scan(&prompts); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Fatalf("fresh fixer prompt rows = %d, want 1", prompts)
	}
}

// TestWatchdog_YoungAgentUntouched pins the young-agent guard: a freshly
// spawned reviewer with ZERO outbox activity (prompt delivery racing pi's
// cold boot) must NOT be parked — the watchdog may only kill agents that
// have lived past the stall bound. Regression: 2026-10-02, the watchdog
// retired a 10-second-old reviewer in the same tick that spawned it.
func TestWatchdog_YoungAgentUntouched(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "ty", "uuid-y", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-ty", "ty") // started_at = NOW()

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := taskCol(t, pool, "ty", "status"); got != "review" {
		t.Fatalf("young task status = %q, want review (no parking on sight)", got)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-ty'`).Scan(&status); err != nil {
		t.Fatalf("reviewer row: %v", err)
	}
	if status != "running" {
		t.Fatalf("young reviewer status = %q, want running", status)
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

// ---- stuck-implementor self-heal (MAQ-14) -------------------------------

func TestIsUniqueLiveErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("new tmux window: exited unexpectedly"), false},
		{fmt.Errorf("insert agent row: %w", errors.New(`ERROR: duplicate key value violates unique constraint "uq_agents_task_live" (SQLSTATE 23505)`)), true},
	}
	for i, c := range cases {
		if got := isUniqueLiveErr(c.err); got != c.want {
			t.Errorf("case %d: isUniqueLiveErr = %v, want %v", i, got, c.want)
		}
	}
}

// TestDispatch_StuckImplementor_AutoRetireThenReviewerSpawns is the MAQ-14
// regression: an implementor opens the PR (set-pr → task 'review'), ends its
// turn without retiring, and its live row blocks every reviewer spawn with
// uq_agents_task_live. Once the implementor's outbox is idle past the bound,
// dispatch retires it exactly once (one pipeline-topic notification) and the
// next tick spawns the reviewer.
func TestDispatch_StuckImplementor_AutoRetireThenReviewerSpawns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "ts", "uuid-s", "/tmp/wt-ts")
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('ts', 'implementor-ts', 'result', 'opened the PR')
	`)
	seedImplementor(t, pool, "implementor-ts", "ts", `NOW() - interval '1 hour'`)
	seedOutboxRow(t, pool, "implementor-ts", "30 minutes")

	// Tick 1: spawn dies on the unique-live index → self-heal retires.
	sp := &fakeSpawner{t: t, pool: pool, failUniqueLive: true}
	if err := dispatchPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 0 {
		t.Fatalf("spawns = %d, want 0 (blocked tick)", len(sp.spawns))
	}
	if got := agentStatus(t, pool, "implementor-ts"); got != "dead" {
		t.Fatalf("stuck implementor status = %q, want dead", got)
	}
	if n := pipelineNotifications(t, pool, "ts"); n != 1 {
		t.Fatalf("auto-retire notifications = %d, want exactly 1", n)
	}
	if got := taskCol(t, pool, "ts", "status"); got != "review" {
		t.Fatalf("task status = %q, want review (self-heal must not park)", got)
	}

	// Tick 2: blocker gone → reviewer spawns; no second notification.
	sp2 := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, nil, sp2, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass 2: %v", err)
	}
	if len(sp2.spawns) != 1 || sp2.spawns[0].AgentID != "reviewer-ts" {
		t.Fatalf("spawns = %+v, want one reviewer-ts", sp2.spawns)
	}
	if n := pipelineNotifications(t, pool, "ts"); n != 1 {
		t.Fatalf("notifications after recovery = %d, want still 1", n)
	}
}

// An implementor that never streamed a single outbox row idles from
// started_at (COALESCE fallback) — same self-heal bound.
func TestDispatch_StuckImplementor_NeverStreamed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tn", "uuid-n", "/tmp/wt-tn")
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tn', 'implementor-tn', 'result', 'opened the PR')
	`)
	seedImplementor(t, pool, "implementor-tn", "tn", `NOW() - interval '1 hour'`)

	sp := &fakeSpawner{t: t, pool: pool, failUniqueLive: true}
	if err := dispatchPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if got := agentStatus(t, pool, "implementor-tn"); got != "dead" {
		t.Fatalf("implementor status = %q, want dead (idle from started_at)", got)
	}
	if n := pipelineNotifications(t, pool, "tn"); n != 1 {
		t.Fatalf("notifications = %d, want exactly 1", n)
	}
}

// A fresh implementor (outbox activity within the bound) is left strictly
// alone: no retire, no notification — silent retry until the idle bound.
func TestDispatch_FreshImplementor_KeepsWaiting(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tf", "uuid-f", "/tmp/wt-tf")
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tf', 'implementor-tf', 'result', 'opened the PR')
	`)
	seedImplementor(t, pool, "implementor-tf", "tf", `NOW() - interval '1 hour'`)
	seedOutboxRow(t, pool, "implementor-tf", "1 minute")

	sp := &fakeSpawner{t: t, pool: pool, failUniqueLive: true}
	if err := dispatchPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if got := agentStatus(t, pool, "implementor-tf"); got != "running" {
		t.Fatalf("fresh implementor status = %q, want running", got)
	}
	if n := pipelineNotifications(t, pool, "tf"); n != 0 {
		t.Fatalf("notifications = %d, want 0", n)
	}
}

// A non-implementor blocker (e.g. a live reviewer row from a cross-process
// race) must NOT be auto-retired — the generic retry path stays.
func TestDispatch_NonImplementorBlocker_NotRetired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tr", "uuid-r", "/tmp/wt-tr")
	seedReviewer(t, pool, "reviewer-tr", "tr")
	seedOutboxRow(t, pool, "reviewer-tr", "30 minutes")

	sp := &fakeSpawner{t: t, pool: pool, failUniqueLive: true}
	if err := dispatchPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if got := agentStatus(t, pool, "reviewer-tr"); got != "running" {
		t.Fatalf("reviewer blocker status = %q, want running (untouched)", got)
	}
	if n := pipelineNotifications(t, pool, "tr"); n != 0 {
		t.Fatalf("notifications = %d, want 0", n)
	}
}

// ---- MAQ-36: honest retire notes + findings salvage ---------------------

// TestWatchdog_RetireNoteStatesRealTrigger pins the retire note against a
// known activity timeline. The freeze predicate is parallel bounds — silence
// on BOTH channels (outbox + transcript) for the full idle bound, agent
// older than the spawn grace — so the note must state exactly that, not the
// old "no outbox activity for X past the Y spawn grace" which read like a
// sequential window and hid the transcript veto (MAQ-35's forensics hit).
func TestWatchdog_RetireNoteStatesRealTrigger(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tn", "uuid-n", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tn", "tn")
	// A live reviewer round is never round 0 — dispatch increments
	// review_rounds at spawn — so pin the note against a round-1 episode.
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1 WHERE id='tn'`)

	// The MAQ-35 timeline shape: spawned ~10m ago, outbox activity 5m ago.
	// At the documented bounds this is a LIVE agent — the watchdog must not
	// fire, so no note can misdescribe it.
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '10 minutes' WHERE id='reviewer-tn'`)
	seedOutboxRow(t, pool, "reviewer-tn", "5 minutes")
	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass (recent activity): %v", err)
	}
	if got := agentStatus(t, pool, "reviewer-tn"); got != "running" {
		t.Fatalf("active reviewer status = %q, want running", got)
	}

	// Now the truly frozen timeline: silent on both channels for 31m. When
	// the watchdog fires, the observation note is pinned to the honest text.
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes', last_transcript_at = NOW() - interval '31 minutes' WHERE id='reviewer-tn'`)
	execOK(t, pool, `UPDATE agent_outbox SET created_at = NOW() - interval '31 minutes' WHERE agent_id='reviewer-tn'`)
	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass (frozen): %v", err)
	}
	var note string
	if err := pool.QueryRow(ctx, `
		SELECT content FROM task_context WHERE task_id='tn' AND kind='observation'
	`).Scan(&note); err != nil {
		t.Fatalf("freeze observation row: %v", err)
	}
	want := "watchdog: reviewer frozen (round 1) — silent for 30m0s (no outbox row and no transcript growth; spawn grace 10m0s elapsed); auto-retired, fresh reviewer respawns in-round"
	if note != want {
		t.Fatalf("retire note = %q, want %q", note, want)
	}
}

// TestSalvageFindings_PrefersLongRows: the salvage scan skips short progress
// notes/acks and returns the newest writeup-length row; no qualifying row →
// "" with no error (the best-effort fallback).
func TestSalvageFindings_PrefersLongRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tsf", "uuid-sf", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tsf", "tsf")

	// Nothing written: fallback.
	got, err := salvageFindings(ctx, pool, "reviewer-tsf")
	if err != nil || got != "" {
		t.Fatalf("empty outbox salvage = %q, %v; want \"\", nil", got, err)
	}

	// Short note, then a writeup: the writeup wins even though it is older.
	seedOutboxRow(t, pool, "reviewer-tsf", "2 minutes")
	long := "1. internal/x/y.go:42 — nil deref when the queue empties. " + strings.Repeat("Detail. ", 40)
	seedOutboxText(t, pool, "reviewer-tsf", long, "10 minutes")
	got, err = salvageFindings(ctx, pool, "reviewer-tsf")
	if err != nil {
		t.Fatalf("salvageFindings: %v", err)
	}
	if !strings.Contains(got, "nil deref") {
		t.Fatalf("salvage = %q, want the long findings row", got)
	}
}

// TestWatchdog_SalvageCarriesFindingsToRespawn is the MAQ-36 acceptance
// flow: a reviewer completes its findings writeup but never delivers the
// verdict, the watchdog retires it, and the respawned round's prompt carries
// the salvaged findings ("verify + post the verdict") — until a verdict
// lands, which retires the salvage as stale.
func TestWatchdog_SalvageCarriesFindingsToRespawn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tsv", "uuid-sv", t.TempDir())
	seedReviewer(t, pool, "reviewer-tsv", "tsv")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='reviewer-tsv'`)

	// The delivery-failure shape (MAQ-35): a short progress note, then the
	// complete findings — no VERDICT line anywhere in the outbox. Both rows
	// sit OUTSIDE the 30m idle bound: after the writeup the reviewer stalled
	// on delivery, and silence on both channels is what the watchdog sees.
	seedOutboxRow(t, pool, "reviewer-tsv", "40 minutes")
	findings := "1. internal/pipeline/freeze.go:206 — the retire note misstates the timer math. " + strings.Repeat("Evidence. ", 40)
	seedOutboxText(t, pool, "reviewer-tsv", findings, "35 minutes")

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if got := agentStatus(t, pool, "reviewer-tsv"); got != "dead" {
		t.Fatalf("frozen reviewer status = %q, want dead", got)
	}
	var salvAgent, salvText string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(agent_id, ''), content FROM task_context
		WHERE task_id='tsv' AND kind='salvage'
	`).Scan(&salvAgent, &salvText); err != nil {
		t.Fatalf("salvage row after retire: %v", err)
	}
	if salvAgent != "reviewer-tsv" || !strings.Contains(salvText, "misstates the timer math") {
		t.Fatalf("salvage row = (%q, %q), want the frozen reviewer's findings", salvAgent, salvText)
	}

	// The respawn: the next spawn pass mints -r2 and its round prompt ships
	// the salvage section with the verify-and-deliver framing.
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].AgentID != "reviewer-tsv-r2" {
		t.Fatalf("respawn = %+v, want exactly reviewer-tsv-r2", sp.spawns)
	}
	prompt := inboxPrompts(t, pool, "reviewer-tsv-r2")[0]
	for _, want := range []string{
		"Salvaged review from reviewer-tsv",
		"never delivered the VERDICT line",
		"VERIFY those findings",
		"misstates the timer math", // the findings body rides along
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("respawn prompt missing salvage framing %q:\n%s", want, prompt)
		}
	}

	// Once a verdict lands, the salvage is stale: no later prompt carries it.
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tsv', 'reviewer-tsv-r2', 'verdict', 'VERDICT: approve')
	`)
	if _, s := fetchSalvagedFindings(ctx, pool, "tsv"); s != "" {
		t.Fatalf("salvage still live after a verdict: %q", s)
	}
}

// TestWatchdog_NoFindings_NoSalvage: the fallback — a frozen reviewer that
// wrote nothing writeup-length leaves no salvage row, and the respawned
// round re-reviews from scratch (today's behavior).
func TestWatchdog_NoFindings_NoSalvage(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tnf", "uuid-nf", t.TempDir())
	seedReviewer(t, pool, "reviewer-tnf", "tnf")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='reviewer-tnf'`)
	// Only short chatter, and outside the idle bound so the freeze predicate
	// actually fires.
	seedOutboxRow(t, pool, "reviewer-tnf", "40 minutes")

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM task_context WHERE task_id='tnf' AND kind='salvage'`); n != 0 {
		t.Fatalf("salvage rows = %d, want 0", n)
	}
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, nil, sp, 10*time.Minute); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if prompt := inboxPrompts(t, pool, "reviewer-tnf-r2")[0]; strings.Contains(prompt, "Salvaged review") {
		t.Fatalf("fallback prompt leaked a salvage section:\n%s", prompt)
	}
}
