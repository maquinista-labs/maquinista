package pipeline

// Tests for the Telegram reply → PR comment surface (MAQ-24): task stamping
// on notifications, reply→task resolution via channel_deliveries, the
// exactly-once claim, and the PostPRComment guards (open-PR gate, verbatim
// post, URL capture).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// ---- notification task stamping ----

func TestNotifyTask_StampsTaskID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := NotifyTask(ctx, pool, "task-abc", "hello PR world"); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if err := Notify(ctx, pool, "legacy note"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	var withTask, withoutTask string
	if err := pool.QueryRow(ctx, `
		SELECT content FROM agent_outbox WHERE agent_id = $1 AND content->>'text' = 'hello PR world'
	`, NotifyAgentID).Scan(&withTask); err != nil {
		t.Fatalf("outbox row: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(withTask), &m); err != nil {
		t.Fatalf("content json: %v", err)
	}
	if m["task_id"] != "task-abc" {
		t.Errorf("task_id = %q, want task-abc", m["task_id"])
	}
	if m["text"] != "hello PR world" {
		t.Errorf("text = %q — stamping must not touch the rendered text", m["text"])
	}

	if err := pool.QueryRow(ctx, `
		SELECT content FROM agent_outbox WHERE agent_id = $1 AND content->>'text' = 'legacy note'
	`, NotifyAgentID).Scan(&withoutTask); err != nil {
		t.Fatalf("outbox row 2: %v", err)
	}
	m = map[string]string{}
	if err := json.Unmarshal([]byte(withoutTask), &m); err != nil {
		t.Fatalf("content json 2: %v", err)
	}
	if m["task_id"] != "" {
		t.Errorf("plain Notify must not stamp a task_id, got %q", m["task_id"])
	}
}

// ---- reply → task resolution ----

func TestTaskForTelegramReply(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := NotifyTask(ctx, pool, "task-xyz", "✅ probe note"); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	var outboxID string
	if err := pool.QueryRow(ctx, `
		SELECT id FROM agent_outbox WHERE agent_id = $1 AND content->>'text' = '✅ probe note'
	`, NotifyAgentID).Scan(&outboxID); err != nil {
		t.Fatalf("outbox lookup: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_deliveries (outbox_id, channel, user_id, thread_id, chat_id, binding_type, status, external_msg_id)
		VALUES ($1, 'telegram', '100', '555', -1001234, 'owner', 'sent', 777)
	`, outboxID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	taskID, ok, err := TaskForTelegramReply(ctx, pool, -1001234, 777)
	if err != nil || !ok {
		t.Fatalf("ok = %v err = %v, want resolved", ok, err)
	}
	if taskID != "task-xyz" {
		t.Errorf("taskID = %q, want task-xyz", taskID)
	}

	// Unknown message: not a pipeline notification.
	if _, ok, err := TaskForTelegramReply(ctx, pool, -1001234, 778); err != nil || ok {
		t.Errorf("unknown message: ok = %v err = %v, want false/nil", ok, err)
	}
	// Different chat: not ours.
	if _, ok, err := TaskForTelegramReply(ctx, pool, -1009999, 777); err != nil || ok {
		t.Errorf("other chat: ok = %v err = %v, want false/nil", ok, err)
	}

	// Legacy notification (no task_id stamped): delivered, but not
	// reply-commentable.
	if err := Notify(ctx, pool, "old-style note"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var legacyID string
	if err := pool.QueryRow(ctx, `
		SELECT id FROM agent_outbox WHERE agent_id = $1 AND content->>'text' = 'old-style note'
	`, NotifyAgentID).Scan(&legacyID); err != nil {
		t.Fatalf("legacy outbox lookup: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_deliveries (outbox_id, channel, user_id, thread_id, chat_id, binding_type, status, external_msg_id)
		VALUES ($1, 'telegram', '100', '555', -1001234, 'owner', 'sent', 888)
	`, legacyID); err != nil {
		t.Fatalf("seed legacy delivery: %v", err)
	}
	if _, ok, err := TaskForTelegramReply(ctx, pool, -1001234, 888); err != nil || ok {
		t.Errorf("legacy notification: ok = %v err = %v, want false/nil", ok, err)
	}
}

// ---- exactly-once claim ----

func TestClaimTelegramComment_ExactlyOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	claimed, err := ClaimTelegramComment(ctx, pool, -1001234, 42, "task-1")
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed = %v err = %v, want true/nil", claimed, err)
	}
	// Retried / redelivered delivery of the same reply: never claims twice.
	claimed, err = ClaimTelegramComment(ctx, pool, -1001234, 42, "task-1")
	if err != nil || claimed {
		t.Fatalf("second claim: claimed = %v err = %v, want false/nil", claimed, err)
	}

	// The claim row carries the task; dispositions audit the outcome.
	var task string
	if err := pool.QueryRow(ctx,
		`SELECT task_id FROM telegram_pr_comments WHERE chat_id = -1001234 AND message_id = 42`).
		Scan(&task); err != nil {
		t.Fatalf("claim row: %v", err)
	}
	if task != "task-1" {
		t.Errorf("claim task_id = %q, want task-1", task)
	}
	if err := SetTelegramCommentDisposition(ctx, pool, -1001234, 42,
		TgDispOK, "", "https://github.com/o/r/pull/9#issuecomment-1", 9); err != nil {
		t.Fatalf("disposition: %v", err)
	}
	var disp, url string
	var pr int
	if err := pool.QueryRow(ctx,
		`SELECT disposition, comment_url, pr FROM telegram_pr_comments WHERE chat_id = -1001234 AND message_id = 42`).
		Scan(&disp, &url, &pr); err != nil {
		t.Fatalf("disposition row: %v", err)
	}
	if disp != TgDispOK || url == "" || pr != 9 {
		t.Errorf("row = %s/%s/%d", disp, url, pr)
	}
	// A different chat's same-numbered message is an independent claim.
	claimed, err = ClaimTelegramComment(ctx, pool, -1005678, 42, "task-2")
	if err != nil || !claimed {
		t.Errorf("other-chat claim: claimed = %v err = %v, want true/nil", claimed, err)
	}
}

// ---- PostPRComment guards ----

type fakePoster struct {
	state    string
	stateErr error
	postErr  error
	url      string
	bodies   []string
}

func (f *fakePoster) PRState(context.Context, int) (string, error) {
	return f.state, f.stateErr
}

func (f *fakePoster) PRPostCommentURL(_ context.Context, _ int, body string) (string, error) {
	f.bodies = append(f.bodies, body)
	if f.postErr != nil {
		return "", f.postErr
	}
	return f.url, nil
}

func TestPostPRComment(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())

	// No PR at all: the graceful ErrNoOpenPR case — nothing posted.
	execOK(t, pool, `INSERT INTO tasks (id, title, status) VALUES ($1, 'no pr', 'review')`, taskID)
	poster := &fakePoster{state: "OPEN", url: "https://github.com/o/r/pull/1#issuecomment-1"}
	if _, _, err := PostPRComment(ctx, pool, poster, taskID, "x"); !errors.Is(err, ErrNoOpenPR) {
		t.Errorf("no pr_url: err = %v, want ErrNoOpenPR", err)
	}
	if len(poster.bodies) != 0 {
		t.Errorf("nothing must be posted without a PR, got %v", poster.bodies)
	}

	// Non-GitHub PR URL: cannot resolve a PR number — graceful too.
	execOK(t, pool, `UPDATE tasks SET pr_url = 'https://gitlab.com/o/r/-/merge_requests/1' WHERE id = $1`, taskID)
	if _, _, err := PostPRComment(ctx, pool, poster, taskID, "x"); !errors.Is(err, ErrNoOpenPR) {
		t.Errorf("non-github url: err = %v, want ErrNoOpenPR", err)
	}

	// PR exists but is merged: still ErrNoOpenPR (a comment there can never
	// reach a review round).
	execOK(t, pool, `UPDATE tasks SET pr_url = 'https://github.com/o/r/pull/2' WHERE id = $1`, taskID)
	poster.state = "MERGED"
	if _, _, err := PostPRComment(ctx, pool, poster, taskID, "x"); !errors.Is(err, ErrNoOpenPR) {
		t.Errorf("merged PR: err = %v, want ErrNoOpenPR", err)
	}

	// Transient state probe failure: a plain error (retryable upstream), not
	// the graceful case.
	poster.state = ""
	poster.stateErr = errors.New("gh api down")
	if _, _, err := PostPRComment(ctx, pool, poster, taskID, "x"); err == nil || errors.Is(err, ErrNoOpenPR) {
		t.Errorf("state probe failure: err = %v, want non-ErrNoOpenPR", err)
	}

	// Happy path: open PR, verbatim body, URL returned.
	poster.state = "OPEN"
	poster.stateErr = nil
	poster.url = "https://github.com/o/r/pull/2#issuecomment-99"
	body := "the fixer missed the error wrap in handler.go — see line 42"
	pr, url, err := PostPRComment(ctx, pool, poster, taskID, body)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if pr != 2 || url != poster.url {
		t.Errorf("pr = %d url = %q, want 2 / %q", pr, url, poster.url)
	}
	if len(poster.bodies) != 1 || poster.bodies[0] != body {
		t.Errorf("body must be posted verbatim, got %q", poster.bodies)
	}
}
