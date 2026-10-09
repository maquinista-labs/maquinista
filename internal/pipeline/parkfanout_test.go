package pipeline

// Tests for the MAQ-34 park fan-out: one PR comment + one issue comment per
// park episode, each quoting the exact approval paths, deduped against
// watchdog-tick re-posts by the episode marker (task_context kind
// 'parkfanout'), and graceful on every degraded surface (no PR, no issue
// mapping, provider without IssueCommenter).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeIssueCommenter is a TicketProvider that can also comment on issues —
// the production Linear provider shape the fan-out probes for.
type fakeIssueCommenter struct {
	fakeTickets
	comments []issueCommentCall
	err      error
}

type issueCommentCall struct{ issueID, body string }

func (f *fakeIssueCommenter) CommentOnIssue(_ context.Context, issueID, body string) error {
	if f.err != nil {
		return f.err
	}
	f.comments = append(f.comments, issueCommentCall{issueID, body})
	return nil
}

func TestNotifyParkFanout_PostsBothSurfaces(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())

	execOK(t, pool, `INSERT INTO tasks (id, title, status, pr_url) VALUES ($1, '[MAQ-x] fanout task', 'pending_approval', 'https://github.com/o/r/pull/7')`, taskID)
	execOK(t, pool, `INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id) VALUES ('iss-1', 'MAQ-99', 'team-1', $1)`, taskID)

	poster := &fakePoster{state: "OPEN", url: "https://github.com/o/r/pull/7#issuecomment-1"}
	prov := &fakeIssueCommenter{}

	NotifyParkFanout(ctx, pool, poster, prov, taskID, "Review round cap 3 reached (request_changes).")

	// PR surface: one comment carrying the summary + all three approval paths.
	if len(poster.bodies) != 1 {
		t.Fatalf("PR comments = %d, want 1 (%v)", len(poster.bodies), poster.bodies)
	}
	body := poster.bodies[0]
	// MAQ-37: the fan-out headline is the canonical `[MAQ-n] <title>` from
	// ticket_issue_map — the same identifier Linear and the PR title use.
	for _, want := range []string{
		"[MAQ-99] fanout task",
		"Review round cap 3 reached (request_changes).",
		"comment `approve` on the PR",
		"./maquinista approve " + taskID,
		"Approve button on the Telegram card in the Approvals topic",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR comment missing %q:\n%s", want, body)
		}
	}
	// Issue surface: same body, on the mapped issue.
	if len(prov.comments) != 1 {
		t.Fatalf("issue comments = %d, want 1 (%v)", len(prov.comments), prov.comments)
	}
	if prov.comments[0].issueID != "iss-1" {
		t.Errorf("issue id = %q, want iss-1", prov.comments[0].issueID)
	}
	if prov.comments[0].body != body {
		t.Errorf("issue body must match the PR body:\nPR:    %q\nissue: %q", body, prov.comments[0].body)
	}
	// The episode marker row exists, anchored on the newest context row.
	var markers int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_context WHERE task_id = $1 AND kind = 'parkfanout'`, taskID).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 1 {
		t.Fatalf("parkfanout markers = %d, want 1", markers)
	}
}

func TestNotifyParkFanout_DedupPerEpisode(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())

	execOK(t, pool, `INSERT INTO tasks (id, title, status, pr_url) VALUES ($1, '[MAQ-x] dedup task', 'pending_approval', 'https://github.com/o/r/pull/8')`, taskID)
	execOK(t, pool, `INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id) VALUES ('iss-2', 'MAQ-98', 'team-1', $1)`, taskID)

	poster := &fakePoster{state: "OPEN", url: "u"}
	prov := &fakeIssueCommenter{}

	// The park transition writes its verdict row (what every park site does
	// inside its guarded tx), then the winner branch fans out.
	execOK(t, pool, `INSERT INTO task_context (task_id, kind, content) VALUES ($1, 'verdict', 'VERDICT: needs_human (round cap 3 reached)')`, taskID)
	NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked.")
	NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked.") // watchdog tick over the SAME parked task

	if len(poster.bodies) != 1 || len(prov.comments) != 1 {
		t.Fatalf("re-post on the same episode: PR=%d issue=%d, want 1/1", len(poster.bodies), len(prov.comments))
	}

	// A RE-park (episode 2): the state machine left pending_approval and
	// came back — the new park's verdict row moves the anchor, so the fresh
	// episode fans out again.
	execOK(t, pool, `UPDATE tasks SET status = 'changes_requested' WHERE id = $1`, taskID)
	execOK(t, pool, `INSERT INTO task_context (task_id, kind, content) VALUES ($1, 'verdict', 'VERDICT: needs_human (round cap 3 reached, again)')`, taskID)
	execOK(t, pool, `UPDATE tasks SET status = 'pending_approval' WHERE id = $1`, taskID)
	NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked again.")

	if len(poster.bodies) != 2 || len(prov.comments) != 2 {
		t.Fatalf("new episode must re-fan-out: PR=%d issue=%d, want 2/2", len(poster.bodies), len(prov.comments))
	}
}

func TestNotifyParkFanout_GracefulDegradation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("no PR url: issue comment alone", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		execOK(t, pool, `INSERT INTO tasks (id, title, status) VALUES ($1, '[MAQ-x] no pr', 'pending_approval')`, taskID)
		execOK(t, pool, `INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id) VALUES ('iss-3', 'MAQ-97', 'team-1', $1)`, taskID)
		poster := &fakePoster{state: "OPEN", url: "u"}
		prov := &fakeIssueCommenter{}
		NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked.")
		if len(poster.bodies) != 0 {
			t.Errorf("nothing must post without a PR, got %v", poster.bodies)
		}
		if len(prov.comments) != 1 {
			t.Errorf("issue comment = %d, want 1", len(prov.comments))
		}
	})

	t.Run("PR merged: no PR comment, issue still notified", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		execOK(t, pool, `INSERT INTO tasks (id, title, status, pr_url) VALUES ($1, '[MAQ-x] merged pr', 'pending_approval', 'https://github.com/o/r/pull/9')`, taskID)
		execOK(t, pool, `INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id) VALUES ('iss-6', 'MAQ-94', 'team-1', $1)`, taskID)
		poster := &fakePoster{state: "MERGED", url: "u"}
		prov := &fakeIssueCommenter{}
		NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked.")
		if len(poster.bodies) != 0 {
			t.Errorf("closed PR must not be commented, got %v", poster.bodies)
		}
		if len(prov.comments) != 1 {
			t.Errorf("issue comment = %d, want 1", len(prov.comments))
		}
	})

	t.Run("unmapped task: PR comment alone", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		execOK(t, pool, `INSERT INTO tasks (id, title, status, pr_url) VALUES ($1, '[MAQ-x] unmapped', 'pending_approval', 'https://github.com/o/r/pull/10')`, taskID)
		poster := &fakePoster{state: "OPEN", url: "u"}
		prov := &fakeIssueCommenter{}
		NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked.")
		if len(poster.bodies) != 1 {
			t.Errorf("PR comment = %d, want 1", len(poster.bodies))
		}
		if len(prov.comments) != 0 {
			t.Errorf("no issue comment without a mapping, got %v", prov.comments)
		}
	})

	t.Run("provider without IssueCommenter: PR comment alone", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		execOK(t, pool, `INSERT INTO tasks (id, title, status, pr_url) VALUES ($1, '[MAQ-x] plain provider', 'pending_approval', 'https://github.com/o/r/pull/11')`, taskID)
		execOK(t, pool, `INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id) VALUES ('iss-4', 'MAQ-96', 'team-1', $1)`, taskID)
		poster := &fakePoster{state: "OPEN", url: "u"}
		NotifyParkFanout(ctx, pool, poster, &fakeTickets{}, taskID, "parked.")
		if len(poster.bodies) != 1 {
			t.Errorf("PR comment = %d, want 1", len(poster.bodies))
		}
	})

	t.Run("poster failure is logged, issue still notified, marker still written", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		execOK(t, pool, `INSERT INTO tasks (id, title, status, pr_url) VALUES ($1, '[MAQ-x] post err', 'pending_approval', 'https://github.com/o/r/pull/12')`, taskID)
		execOK(t, pool, `INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id) VALUES ('iss-5', 'MAQ-95', 'team-1', $1)`, taskID)
		poster := &fakePoster{state: "OPEN", postErr: errors.New("gh down")}
		prov := &fakeIssueCommenter{}
		NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked.")
		if len(prov.comments) != 1 {
			t.Errorf("issue comment = %d, want 1 (independent of the PR leg)", len(prov.comments))
		}
		// The claim is spent either way: a retry must not double-post the issue.
		NotifyParkFanout(ctx, pool, poster, prov, taskID, "parked.")
		if len(prov.comments) != 1 {
			t.Errorf("issue comments after retry = %d, want 1 (episode dedup)", len(prov.comments))
		}
	})

	t.Run("zero value fans out nothing", func(t *testing.T) {
		taskID := fmt.Sprintf("t-%d", nextTaskNum())
		execOK(t, pool, `INSERT INTO tasks (id, title, status, pr_url) VALUES ($1, '[MAQ-x] zero', 'pending_approval', 'https://github.com/o/r/pull/13')`, taskID)
		fan := parkFanout{}
		fan.notify(ctx, pool, taskID, "parked.") // must be a no-op, no error paths
	})
}

// TestVerdictPark_FansOut (MAQ-34): the dispatch round-cap park and the
// needs_human escalation fan out through verdictPass; approve and
// request_changes (fixer-loop arms, no park) never do.
func TestVerdictPark_FansOut(t *testing.T) {
	cases := []struct {
		name, verdict string
		bumpRounds    bool
		wantComments  int
	}{
		{"approve", VerdictApprove, false, 0},
		{"request-changes", VerdictRequestChanges, false, 0},
		{"needs-human", VerdictNeedsHuman, false, 1},
		{"round-cap", VerdictRequestChanges, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			taskID := "tvfo-" + c.name
			seedReviewTask(t, pool, taskID, "uuid-"+c.name, "/tmp/wt")
			execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`, taskID, "https://github.com/o/r/pull/20")
			if c.bumpRounds {
				execOK(t, pool, `UPDATE tasks SET review_rounds = 3 WHERE id = $1`, taskID)
			}
			seedReviewer(t, pool, "reviewer-"+taskID, taskID)
			execOK(t, pool, `INSERT INTO agent_outbox (agent_id, content) VALUES ($1, $2::jsonb)`,
				"reviewer-"+taskID, `{"text":"findings...\nVERDICT: `+c.verdict+`\n"}`)

			poster := &fakePoster{state: "OPEN", url: "u"}
			fan := parkFanout{gh: poster}
			if err := verdictPass(ctx, pool, nil, fan, 3, "sess", nil); err != nil {
				t.Fatalf("verdictPass: %v", err)
			}
			if len(poster.bodies) != c.wantComments {
				t.Fatalf("PR comments = %d, want %d (%v)", len(poster.bodies), c.wantComments, poster.bodies)
			}
			if c.wantComments > 0 && !strings.Contains(poster.bodies[0], "./maquinista approve "+taskID) {
				t.Errorf("park comment must quote the CLI approval path, got:\n%s", poster.bodies[0])
			}
		})
	}
}

func TestParkApproverHint(t *testing.T) {
	t.Setenv("MAQUINISTA_TICKETS_APPROVERS", "otaviio@gmail.com,otaviocarvalho")
	if got := parkApproverHint(); got != "otaviocarvalho" {
		t.Errorf("hint = %q, want the non-email handle otaviocarvalho", got)
	}
	t.Setenv("MAQUINISTA_TICKETS_APPROVERS", "otaviio@gmail.com")
	if got := parkApproverHint(); got != "otaviio@gmail.com" {
		t.Errorf("hint = %q, want the only entry verbatim", got)
	}
	t.Setenv("MAQUINISTA_TICKETS_APPROVERS", "")
	if got := parkApproverHint(); got != "<your-approver-id>" {
		t.Errorf("hint = %q, want the placeholder", got)
	}
}
