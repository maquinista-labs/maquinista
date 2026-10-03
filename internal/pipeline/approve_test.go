package pipeline

// Tests for the comment-action approve verb (MAQ-11): verb parsing,
// approver gating, the shared ApproveRef arm, and the exactly-once comment
// pass. DB-backed tests pair the disposable Postgres with the same local
// git remote trio + fake GhRunner the merge flow tests use (merge_test.go).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/db"
)

// ---- verb parsing ----

func TestIsApproveComment(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"approve", true},
		{"Approve", true},
		{"  approve  ", true},
		{"/approve", true},
		{"approve.", true},
		{"approve!", true},
		{"maquinista approve", true},
		{"Maquinista Approve", true},
		{"/maquinista approve", true},
		{"maquinista   approve", true},
		{"", false},
		{"approved", false},
		{"approve now", false},
		{"approve\nplease merge", false},
		{"lgtm, approve", false},
		{"disapprove", false},
		{"approve the ticket", false},
		{"maquinista approve now", false},
		{"maquinista", false},
	}
	for _, c := range cases {
		if got := IsApproveComment(c.body); got != c.want {
			t.Errorf("IsApproveComment(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

func TestIsAllowedApprover(t *testing.T) {
	approvers := []string{"Op@Example.com", "Ada"}
	if !IsAllowedApprover("op@example.com", approvers) {
		t.Error("email match (case-insensitive) failed")
	}
	if !IsAllowedApprover(" ada ", approvers) {
		t.Error("name match with whitespace failed")
	}
	if IsAllowedApprover("mallory@evil.com", approvers) {
		t.Error("non-approver author must not match")
	}
	if IsAllowedApprover("", approvers) {
		t.Error("empty author must not match")
	}
	if IsAllowedApprover("op@example.com", nil) {
		t.Error("empty approver list is fail-closed — nobody approves")
	}
}

// ---- shared verb arm ----

// TestApproveRef_GateNoOps: approve never forces a transition — a task not
// in ready_to_merge (or a non-gh merge mode) is a no-op with the observed
// status surfaced.
func TestApproveRef_GateNoOps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status) VALUES ($1, 'gated', 'pending_approval')
	`, taskID)

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: false, Gh: gh}

	out, err := ApproveRef(ctx, pool, cfg, &fakeProvider{}, "team-1", taskID, "note")
	if err != nil {
		t.Fatal(err)
	}
	if out.Ran || out.Status != "pending_approval" {
		t.Fatalf("outcome = %+v, want ran=false status=pending_approval", out)
	}
	if gh.mergeCalls != 0 {
		t.Errorf("merge fired %d times on a pending_approval task", gh.mergeCalls)
	}

	// Local mode: even a ready_to_merge task must not run the gh flow.
	execOK(t, pool, `UPDATE tasks SET status = 'ready_to_merge' WHERE id = $1`, taskID)
	localCfg := MergeConfig{Mode: MergeModeLocal, Gh: gh}
	if out, err = ApproveRef(ctx, pool, localCfg, &fakeProvider{}, "team-1", taskID, "note"); err != nil {
		t.Fatal(err)
	}
	if out.Ran {
		t.Fatal("local-mode approve must not run the gh merge flow")
	}
	if gh.mergeCalls != 0 {
		t.Errorf("merge fired %d times in local mode", gh.mergeCalls)
	}

	// Unknown reference: error, not a silent no-op.
	if _, err := ApproveRef(ctx, pool, cfg, &fakeProvider{}, "team-1", "does-not-exist", ""); err == nil {
		t.Fatal("approve of an unknown task must error")
	}
}

// TestApproveRef_RunsGHMerge: the happy path — a ready_to_merge task under
// gh mode merges on approve, records the audit note, and lands done.
func TestApproveRef_RunsGHMerge(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "approveref")
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, metadata)
		VALUES ($1, 'approve me', 'ready_to_merge', $2,
		        'https://github.com/maquinista-labs/maquinista/pull/98', '{"ticket_issue_id":"issue-2"}'::jsonb)
	`, taskID, worktree)
	gitRun(t, worktree, "checkout", "-B", "t-"+taskID+"/feature")

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: false, Gh: gh}

	out, err := ApproveRef(context.Background(), pool, cfg, &fakeProvider{}, "team-1", taskID,
		"approved via comment by op")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Ran || out.Status != "ready_to_merge" {
		t.Fatalf("outcome = %+v, want ran=true status=ready_to_merge", out)
	}
	if status, _ := taskRow(t, pool, taskID); status != "done" {
		t.Errorf("task = %s, want done", status)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM task_context WHERE task_id = $1 AND content = 'approved via comment by op'`,
		taskID).Scan(&n); err != nil || n != 1 {
		t.Errorf("audit observation rows = %d (err %v), want 1", n, err)
	}
}

// ---- comment pass ----

// fakeCommentProvider is a fakeProvider (merge_test.go) with comment support.
type fakeCommentProvider struct {
	fakeProvider
	comments    []IssueComment
	gotIssueIDs []string
	gotSince    time.Time
	err         error
}

func (f *fakeCommentProvider) RecentComments(ctx context.Context, issueIDs []string, since time.Time) ([]IssueComment, error) {
	f.gotIssueIDs = issueIDs
	f.gotSince = since
	if f.err != nil {
		return nil, f.err
	}
	return f.comments, nil
}

func TestCommentApprovalsOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	issueID := "issue-maq11"
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url, metadata)
		VALUES ($1, 'comment approve', 'ready_to_merge',
		        'https://github.com/maquinista-labs/maquinista/pull/101',
		        $2::jsonb)
	`, taskID, fmt.Sprintf(`{"ticket_issue_id":%q}`, issueID))
	execOK(t, pool, `
		INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id, pending_state)
		VALUES ($1, 'MAQ-11', 'team-1', $2, 'Ready to Merge')
	`, issueID, taskID)

	var calls []string
	ca := CommentApprover{
		Approvers: []string{"op@example.com"},
		Approve: func(ctx context.Context, tid, actor string) (*ApproveOutcome, error) {
			calls = append(calls, tid+"|"+actor)
			return &ApproveOutcome{TaskID: tid, Status: "ready_to_merge", Ran: true}, nil
		},
	}
	prov := &fakeCommentProvider{comments: []IssueComment{
		{ID: "c-allowed", IssueID: issueID, Body: "approve", Author: "op@example.com"},
		{ID: "c-prose", IssueID: issueID, Body: "please approve this", Author: "op@example.com"},
		{ID: "c-denied", IssueID: issueID, Body: "approve", Author: "mallory@evil.com"},
		{ID: "c-other-issue", IssueID: "issue-elsewhere", Body: "approve", Author: "op@example.com"},
	}}

	n, err := CommentApprovalsOnce(ctx, pool, prov, "team-1", ca)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(calls) != 1 {
		t.Fatalf("processed = %d calls = %v, want exactly one approve", n, calls)
	}
	if calls[0] != taskID+"|op@example.com" {
		t.Errorf("call = %q, want task+actor %q", calls[0], taskID+"|op@example.com")
	}
	// The fetch was scoped to the candidate issues.
	if len(prov.gotIssueIDs) != 1 || prov.gotIssueIDs[0] != issueID {
		t.Errorf("RecentComments issueIDs = %v, want [%s]", prov.gotIssueIDs, issueID)
	}
	// The consumed comment is ledgered with task + actor.
	var loggedTask, loggedActor string
	if err := pool.QueryRow(ctx,
		`SELECT task_id, actor FROM ticket_comment_log WHERE comment_id = 'c-allowed'`,
	).Scan(&loggedTask, &loggedActor); err != nil {
		t.Fatalf("comment not consumed: %v", err)
	}
	if loggedTask != taskID || loggedActor != "op@example.com" {
		t.Errorf("ledger = (%s, %s), want (%s, op@example.com)", loggedTask, loggedActor, taskID)
	}

	// Second pass, same comments (the poller re-reads the window): the
	// identical comment must NOT re-merge — exactly-once.
	n, err = CommentApprovalsOnce(ctx, pool, prov, "team-1", ca)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(calls) != 1 {
		t.Fatalf("replay processed = %d calls = %v, want no second merge", n, calls)
	}
}

// TestCommentApprovalsOnce_NonRTMTask: comments on a task that left
// ready_to_merge are ignored by construction (approve never forces a
// transition).
func TestCommentApprovalsOnce_NonRTMTask(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status) VALUES ($1, 'moved on', 'pending_approval')
	`, taskID)
	execOK(t, pool, `
		INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id, pending_state)
		VALUES ('issue-nortm', 'MAQ-12', 'team-1', $1, 'Needs Human')
	`, taskID)

	calls := 0
	ca := CommentApprover{
		Approvers: []string{"op@example.com"},
		Approve: func(ctx context.Context, tid, actor string) (*ApproveOutcome, error) {
			calls++
			return &ApproveOutcome{TaskID: tid, Status: "pending_approval"}, nil
		},
	}
	prov := &fakeCommentProvider{comments: []IssueComment{
		{ID: "c-nortm", IssueID: "issue-nortm", Body: "approve", Author: "op@example.com"},
	}}

	n, err := CommentApprovalsOnce(ctx, pool, prov, "team-1", ca)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || calls != 0 {
		t.Fatalf("processed = %d calls = %d, want nothing on a non-ready_to_merge task", n, calls)
	}
}

// TestCommentApprovalsOnce_ApproveFails: a failing verb consumes the comment
// (the attempt happened) and keeps the pass non-fatal — the operator can
// re-approve.
func TestCommentApprovalsOnce_ApproveFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status) VALUES ($1, 'boom', 'ready_to_merge')
	`, taskID)
	execOK(t, pool, `
		INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id, pending_state)
		VALUES ('issue-boom', 'MAQ-13', 'team-1', $1, 'Ready to Merge')
	`, taskID)

	ca := CommentApprover{
		Approvers: []string{"op@example.com"},
		Approve: func(ctx context.Context, tid, actor string) (*ApproveOutcome, error) {
			return nil, errors.New("remote hung up")
		},
	}
	prov := &fakeCommentProvider{comments: []IssueComment{
		{ID: "c-boom", IssueID: "issue-boom", Body: "approve", Author: "op@example.com"},
	}}

	n, err := CommentApprovalsOnce(ctx, pool, prov, "team-1", ca)
	if err != nil {
		t.Fatalf("verb failure must not fail the pass: %v", err)
	}
	_ = n
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ticket_comment_log WHERE comment_id = 'c-boom'`).Scan(&count); err != nil || count != 1 {
		t.Errorf("consumed rows = %d (err %v), want 1 — failing verb still consumes", count, err)
	}
}

// TestRunCommentApprovals_NoCommentSupport: a provider without
// CommentFetcher logs once and exits — the pass is a no-op, not a hot loop.
func TestRunCommentApprovals_NoCommentSupport(t *testing.T) {
	pool := testPool(t)
	if err := RunCommentApprovals(context.Background(), pool, &fakeProvider{}, "team-1", time.Second, CommentApprover{}); err != nil {
		t.Fatalf("RunCommentApprovals without comment support = %v, want nil", err)
	}
}

// TestShortTaskID: the hint id is the uuid's first 8 chars.
func TestShortTaskID(t *testing.T) {
	full := "5dcb9d88-a145-46b2-a3de-504a1528bbfe"
	if got := shortTaskID(full); got != "5dcb9d88" {
		t.Errorf("shortTaskID = %q, want 5dcb9d88", got)
	}
	if got := shortTaskID("tv"); got != "tv" {
		t.Errorf("shortTaskID(short) = %q, want unchanged", got)
	}
}

// TestResolvePartialID_ResolvesShortHint: the short id printed in notes
// resolves like a full uuid (the Telegram verb's mobile ergonomics).
func TestResolvePartialID_ResolvesShortHint(t *testing.T) {
	pool := testPool(t)
	full := "5dcb9d88-a145-46b2-a3de-504a1528bbfe"
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status) VALUES ($1, 'hint', 'ready_to_merge')
	`, full)

	got, err := db.ResolvePartialID(pool, "5dcb9d88")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "5dcb9d88") {
		t.Errorf("resolved = %q, want the seeded task", got)
	}
}
