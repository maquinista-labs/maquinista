package pipeline

// Tests for the comment-driven re-round (MAQ-30): a plain (non-verb) PR
// comment from an allowed login re-opens a round on the task behind the PR.
// Run at the same seams as the comment-command tests (fake CommentSource,
// fake spawner, real disposable Postgres; GitHub faked at GhRunner).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedReroundTask inserts a pipeline task with a PR (pull/77) for the
// trigger tests. Empty worktree seeds no worktree (the guard's no-op case).
func seedReroundTask(t *testing.T, pool *pgxpool.Pool, taskID, status, worktree string, rounds int) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, pr_state, metadata, review_rounds)
		VALUES ($1, $2, $3, NULLIF($4, ''), 'https://github.com/o/r/pull/77', 'open',
		        '{"ticket_issue_id":"iss-77"}'::jsonb, $5)
	`, taskID, "task "+taskID, status, worktree, rounds)
}

func reroundDeps(t *testing.T, pool *pgxpool.Pool, gh *fakeGh, sp *fakeSpawner, logins ...string) CommentDeps {
	t.Helper()
	d := commentDepsForTest(pool, &fakeComments{}, logins...)
	d.Gh = gh
	d.Spawn = sp
	return d
}

func TestCommentRound_FixerOnParkedTask(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	seedReroundTask(t, pool, taskID, "pending_approval", "/tmp/wt-rr", 2)
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")

	c := PRComment{ID: 901, Author: "alice", Body: "please also add X", CreatedAt: time.Now()}
	disp, err := DispatchCommentCommand(ctx, d, nil, 77, c)
	if err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v, want ok", disp, err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
	s := sp.spawns[0]
	if s.Role != fixerRole || s.TaskID != taskID || s.WorktreePath != "/tmp/wt-rr" || s.SoulTemplateID != FixerSoulTemplate {
		t.Errorf("spawn = %+v, want fixer round on the task worktree", s)
	}
	// The park is lifted: changes_requested is the state every fixer leg
	// (watchdog arm, prompt heal, candidates) keys on.
	if got := taskCol(t, pool, taskID, "status"); got != "changes_requested" {
		t.Errorf("status = %q, want changes_requested", got)
	}
	// The round never moves: a comment does not reset the cap (AC 4).
	if got := taskCol(t, pool, taskID, "review_rounds"); got != "2" {
		t.Errorf("review_rounds = %q, want untouched 2", got)
	}
	// Episode claimed (fix row) + prompt enqueued under the standard fix id.
	if n := count(t, pool, `SELECT count(*) FROM task_context
		WHERE task_id = $1 AND kind = 'fix' AND content = 'round 2'`, taskID); n != 1 {
		t.Errorf("fix rows = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM agent_inbox
		WHERE agent_id = $1 AND external_msg_id = $2`, s.AgentID, fmt.Sprintf("fix:%s:2", taskID)); n != 1 {
		t.Errorf("prompt rows = %d, want 1", n)
	}
	var prompt string
	if err := pool.QueryRow(ctx, `SELECT content->>'prompt' FROM agent_inbox
		WHERE agent_id = $1 AND external_msg_id = $2`, s.AgentID, fmt.Sprintf("fix:%s:2", taskID)).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "please also add X") || !strings.Contains(prompt, "@alice") {
		t.Errorf("prompt does not carry the comment as the work order:\n%s", prompt)
	}
	// The one-liner fired exactly once (deduped by the fix row).
	if n := count(t, pool, `SELECT count(*) FROM agent_outbox
		WHERE agent_id = 'pipeline' AND content->>'text' LIKE '🔧%working @alice%'`); n != 1 {
		t.Errorf("re-round one-liners = %d, want 1", n)
	}

	// Re-delivery of the same comment (poll overlap, restart catch-up):
	// exactly-once, no second spawn (AC 1).
	if disp, err := DispatchCommentCommand(ctx, d, nil, 77, c); err != nil || disp != "duplicate" {
		t.Fatalf("re-delivery: disp=%q err=%v, want duplicate", disp, err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns after re-delivery = %d, want 1", len(sp.spawns))
	}
}

func TestCommentRound_FixerPromptCarriesFindings(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	seedReroundTask(t, pool, taskID, "changes_requested", "/tmp/wt-rr2", 2)
	// A request_changes verdict + the reviewer's findings outbox row: the
	// prompt must carry them as context alongside the comment.
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('reviewer-vx', 'sess', 'v', 'reviewer', $1, 'dead', 'pi', '/tmp/wt', 'v', NOW(), NOW(), FALSE)
	`, taskID)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, 'reviewer-vx', 'verdict', 'VERDICT: request_changes')
	`, taskID)
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content, created_at)
		VALUES ('reviewer-vx', '{"type":"text","text":"1) tests missing for the edge case\nVERDICT: request_changes"}'::jsonb, NOW())
	`)

	gh := &fakeGh{}
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	d := reroundDeps(t, pool, gh, sp, "alice")

	c := PRComment{ID: 902, Author: "alice", Body: "the fix broke the edge case, look again", CreatedAt: time.Now()}
	if disp, err := DispatchCommentCommand(ctx, d, nil, 77, c); err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v, want ok", disp, err)
	}
	var prompt string
	if err := pool.QueryRow(ctx, `SELECT content->>'prompt' FROM agent_inbox
		WHERE agent_id = $1 AND external_msg_id = $2`, sp.spawns[0].AgentID,
		fmt.Sprintf("fix:%s:2", taskID)).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "the fix broke the edge case") {
		t.Errorf("prompt missing the comment:\n%s", prompt)
	}
	if !strings.Contains(prompt, "tests missing for the edge case") {
		t.Errorf("prompt missing the reviewer findings:\n%s", prompt)
	}
	// The PR narrates the pickup with the comment as the reason (AC 1).
	if len(gh.postedBodies) != 1 {
		t.Fatalf("posted = %v, want one pickup comment", gh.postedBodies)
	}
	if !strings.Contains(gh.postedBodies[0], "fixer round 2 picked this up - the fix broke the edge case") {
		t.Errorf("pickup body = %q", gh.postedBodies[0])
	}

	// The standard fixer pass must not spawn a second fixer for the episode
	// (the fix row it keys on is already there — exactly one round per
	// comment, and the pass and the trigger share the episode dedup).
	if err := fixerPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns after fixerPass = %d, want 1", len(sp.spawns))
	}
}

func TestCommentRound_ReopenReview(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	seedReroundTask(t, pool, taskID, "ready_to_merge", "/tmp/wt-rr3", 1)
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch)
		VALUES ($1, 'merger', 't-rr3/feature', '/tmp/wt-rr3', 'main')
	`, taskID)

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")

	c := PRComment{ID: 903, Author: "alice", Body: "wait — this misses the case from the ticket", CreatedAt: time.Now()}
	if disp, err := DispatchCommentCommand(ctx, d, nil, 77, c); err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v, want ok", disp, err)
	}
	if got := taskCol(t, pool, taskID, "status"); got != "review" {
		t.Fatalf("status = %q, want review", got)
	}
	// The pending merge entry failed: the drain can neither merge over the
	// objection nor churn claim-and-release behind the fresh round.
	var entryStatus, entryErr string
	if err := pool.QueryRow(ctx, `SELECT status, COALESCE(error_msg,'') FROM merge_queue
		WHERE task_id = $1`, taskID).Scan(&entryStatus, &entryErr); err != nil {
		t.Fatal(err)
	}
	if entryStatus != "failed" || !strings.Contains(entryErr, "re-opened review") {
		t.Errorf("entry = %s/%q, want failed + re-opened-review note", entryStatus, entryErr)
	}

	// The dispatch loop takes it from here: a fresh reviewer round spawns
	// and bumps the round (the 🔁 marker + comment fold-in are that pass's
	// own machinery — MAQ-25/MAQ-16).
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].Role != reviewerRole {
		t.Fatalf("spawns = %+v, want one reviewer", sp.spawns)
	}
	if got := taskCol(t, pool, taskID, "review_rounds"); got != "2" {
		t.Fatalf("review_rounds = %q, want 2", got)
	}
}

func TestCommentRound_ReopenReviewGuards(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sp := &fakeSpawner{t: t, pool: pool}
	gh := &fakeGh{}

	t.Run("no worktree is a clean no-op (guard never bypassed)", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		seedReroundTask(t, pool, taskID, "ready_to_merge", "", 1)
		d := reroundDeps(t, pool, gh, sp, "alice")
		disp, err := DispatchCommentCommand(ctx, d, nil, 77,
			PRComment{ID: 911, Author: "alice", Body: "objection"})
		if err != nil || disp != DispNoOp {
			t.Fatalf("disp=%q err=%v, want no-op", disp, err)
		}
		if got := taskCol(t, pool, taskID, "status"); got != "ready_to_merge" {
			t.Fatalf("status = %q, want untouched", got)
		}
	})

	t.Run("merging entry wins — flip skipped", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		seedReroundTask(t, pool, taskID, "ready_to_merge", "/tmp/wt-rr4", 1)
		execOK(t, pool, `
			INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, status, started_at)
			VALUES ($1, 'merger', 't-rr4/feature', '/tmp/wt-rr4', 'main', 'merging', NOW())
		`, taskID)
		d := reroundDeps(t, pool, gh, sp, "alice")
		disp, err := DispatchCommentCommand(ctx, d, nil, 77,
			PRComment{ID: 912, Author: "alice", Body: "too late, merge is running"})
		if err != nil || disp != DispNoOp {
			t.Fatalf("disp=%q err=%v, want no-op", disp, err)
		}
		if got := taskCol(t, pool, taskID, "status"); got != "ready_to_merge" {
			t.Fatalf("status = %q, want untouched", got)
		}
		if got := count(t, pool, `SELECT count(*) FROM merge_queue WHERE task_id = $1 AND status = 'merging'`, taskID); got != 1 {
			t.Fatalf("merging entries = %d, want untouched 1", got)
		}
	})
}

func TestCommentRound_NoOps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sp := &fakeSpawner{t: t, pool: pool}

	t.Run("review round in flight", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		seedReroundTask(t, pool, taskID, "review", "/tmp/wt-rr5", 1)
		seedReviewer(t, pool, "reviewer-live-"+taskID, taskID)
		d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")
		disp, err := DispatchCommentCommand(ctx, d, nil, 77,
			PRComment{ID: 921, Author: "alice", Body: "a note mid-review"})
		if err != nil || disp != DispNoOp {
			t.Fatalf("disp=%q err=%v, want no-op", disp, err)
		}
		if got := taskCol(t, pool, taskID, "status"); got != "review" {
			t.Fatalf("status = %q, want untouched", got)
		}
		if len(sp.spawns) != 0 {
			t.Fatalf("spawns = %d, want 0", len(sp.spawns))
		}
	})

	t.Run("done task", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		seedReroundTask(t, pool, taskID, "done", "", 1)
		d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")
		if disp, err := DispatchCommentCommand(ctx, d, nil, 77,
			PRComment{ID: 922, Author: "alice", Body: "thanks"}); err != nil || disp != DispNoOp {
			t.Fatalf("disp=%q err=%v, want no-op", disp, err)
		}
	})

	t.Run("non-pipeline task", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		execOK(t, pool, `
			INSERT INTO tasks (id, title, status, worktree_path, pr_url)
			VALUES ($1, 'plain task', 'ready_to_merge', '/tmp/wt', 'https://github.com/o/r/pull/77')
		`, taskID)
		d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")
		if disp, err := DispatchCommentCommand(ctx, d, nil, 77,
			PRComment{ID: 923, Author: "alice", Body: "objection"}); err != nil || disp != DispNoOp {
			t.Fatalf("disp=%q err=%v, want no-op", disp, err)
		}
		if got := taskCol(t, pool, taskID, "status"); got != "ready_to_merge" {
			t.Fatalf("status = %q, want untouched", got)
		}
	})

	t.Run("bot author never triggers (AC 3)", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		seedReroundTask(t, pool, taskID, "pending_approval", "/tmp/wt-rr6", 2)
		d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")
		disp, err := DispatchCommentCommand(ctx, d, nil, 77,
			PRComment{ID: 924, Author: "renovate[bot]", IsBot: true, Body: "chore: bump deps"})
		if err != nil || disp != DispNoOp {
			t.Fatalf("disp=%q err=%v, want no-op", disp, err)
		}
		var row string
		if err := pool.QueryRow(ctx,
			`SELECT disposition FROM gh_comment_commands WHERE comment_id = 924`).Scan(&row); err != nil || row != DispNoOp {
			t.Fatalf("claim row=%q err=%v, want claimed no_op", row, err)
		}
		if got := taskCol(t, pool, taskID, "status"); got != "pending_approval" {
			t.Fatalf("status = %q, want untouched", got)
		}
		if len(sp.spawns) != 0 {
			t.Fatalf("spawns = %d, want 0", len(sp.spawns))
		}
	})

	t.Run("non-allowed login never triggers (AC 3)", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		seedReroundTask(t, pool, taskID, "pending_approval", "/tmp/wt-rr7", 2)
		d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")
		disp, err := DispatchCommentCommand(ctx, d, nil, 77,
			PRComment{ID: 925, Author: "mallory", Body: "please also add X"})
		if err != nil || disp != DispUnauthorized {
			t.Fatalf("disp=%q err=%v, want unauthorized", disp, err)
		}
		if got := taskCol(t, pool, taskID, "status"); got != "pending_approval" {
			t.Fatalf("status = %q, want untouched", got)
		}
		if len(sp.spawns) != 0 {
			t.Fatalf("spawns = %d, want 0", len(sp.spawns))
		}
	})
}

func TestCommentRound_VerbCommentNeverTriggers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// A parked task with a worktree — a prose comment would spawn a fixer;
	// the verb comment must only run the verb (never BOTH, task AC).
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	seedReroundTask(t, pool, taskID, "pending_approval", "/tmp/wt-rr8", 2)
	sp := &fakeSpawner{t: t, pool: pool}
	d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")

	RegisterCommentVerb("probe", func(context.Context, CommentContext) error { return nil })
	t.Cleanup(func() { unregisterCommentVerb("probe") })

	if disp, err := DispatchCommentCommand(ctx, d, nil, 77,
		PRComment{ID: 931, Author: "alice", Body: "maquinista probe"}); err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v, want verb ok", disp, err)
	}
	if len(sp.spawns) != 0 {
		t.Fatalf("spawns = %d, want 0 (a verb comment never also triggers a round)", len(sp.spawns))
	}
	var verb string
	if err := pool.QueryRow(ctx,
		`SELECT verb FROM gh_comment_commands WHERE comment_id = 931`).Scan(&verb); err != nil || verb != "probe" {
		t.Fatalf("claim verb=%q err=%v, want probe", verb, err)
	}
}

func TestCommentRound_CapIntact(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// A cap-parked task (rounds=3=max): the comment spawns ONE more fixer
	// episode; the follow-up request_changes verdict still parks at the cap.
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	seedReroundTask(t, pool, taskID, "pending_approval", "/tmp/wt-rr9", 3)
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	d := reroundDeps(t, pool, &fakeGh{}, sp, "alice")

	if disp, err := DispatchCommentCommand(ctx, d, nil, 77,
		PRComment{ID: 941, Author: "alice", Body: "one more try"}); err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v, want ok", disp, err)
	}
	if got := taskCol(t, pool, taskID, "review_rounds"); got != "3" {
		t.Fatalf("review_rounds = %q after trigger, want untouched 3", got)
	}

	// Close the episode the way the loop does: fixer retires, task re-enters
	// review, a fresh reviewer round spawns and its request_changes parks —
	// the cap machinery is untouched by the trigger.
	execOK(t, pool, `UPDATE agents SET status='dead' WHERE role = 'fixer' AND task_id = $1`, taskID)
	execOK(t, pool, `UPDATE tasks SET status='review' WHERE id = $1`, taskID)
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if got := taskCol(t, pool, taskID, "review_rounds"); got != "4" {
		t.Fatalf("review_rounds = %q, want 4", got)
	}
	reviewerID := sp.spawns[len(sp.spawns)-1].AgentID
	landed, applied, err := applyVerdict(ctx, pool, reviewerID, taskID, VerdictRequestChanges, "changes_requested", 3)
	if err != nil || !applied {
		t.Fatalf("applyVerdict: landed=%q applied=%v err=%v", landed, applied, err)
	}
	if landed != "pending_approval" {
		t.Fatalf("landed = %q, want pending_approval (cap still enforced)", landed)
	}
}

func TestCommentPickupReason(t *testing.T) {
	if got := commentPickupReason("please also add X\n\n(second line)"); got != "please also add X" {
		t.Errorf("commentPickupReason simple = %q", got)
	}
	long := strings.Repeat("x", 200)
	got := commentPickupReason(long)
	if !strings.HasSuffix(got, " …") || len([]rune(strings.TrimSuffix(got, " …"))) != maxPickupReasonChars {
		t.Errorf("commentPickupReason long = %d runes, want capped %d + ellipsis", len([]rune(got)), maxPickupReasonChars)
	}
	if got := commentPickupReason("   \n  "); got != commentTriggerFallbackReason {
		t.Errorf("commentPickupReason blank = %q, want fallback", got)
	}
}
