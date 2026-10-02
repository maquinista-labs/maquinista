// Comment-action approve (MAQ-11): the human release authority is a comment
// from an allowed approver on a surface he already has open — the Telegram
// Pipeline topic or the ticket-system issue. Both surfaces route into the
// same verb arm (ApproveRef → RunMergeOnApprove); there is no new merge code
// path. Exactly-once for ticket comments is enforced by consuming comment
// ids into ticket_comment_log, with merge_queue's partial live index as the
// second guard.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
)

// ApproveOutcome reports what ApproveRef saw and did.
type ApproveOutcome struct {
	TaskID string // resolved (full) task id
	Status string // task status observed before acting
	Ran    bool   // true when the gh merge flow ran for this task
}

// ApproveRef is the shared approve-verb arm behind the CLI, the Telegram
// Pipeline-topic verb and ticket-comment approves. It resolves taskRef (full
// UUID or unambiguous prefix) and — only when the task is ready_to_merge
// under gh merge mode — runs the merge flow immediately, overriding the
// auto-merge gate for this one merge. Any other status is a no-op: approve
// never forces a state transition. note (when non-empty) is recorded as a
// task observation whenever the verb armed a merge, so the audit trail says
// who pulled the trigger from which surface.
func ApproveRef(ctx context.Context, pool *pgxpool.Pool, mCfg MergeConfig, prov TicketProvider, teamID, taskRef, note string) (*ApproveOutcome, error) {
	taskID, err := db.ResolvePartialID(pool, taskRef)
	if err != nil {
		return nil, err
	}
	t, err := db.GetTask(pool, taskID)
	if err != nil {
		return nil, err
	}
	out := &ApproveOutcome{TaskID: taskID, Status: t.Status}
	if t.Status != "ready_to_merge" || mCfg.Mode != MergeModeGH {
		return out, nil
	}
	if note != "" {
		// Audit first: the approve happened even if the merge then hits
		// infrastructure trouble (the queue entry records that side).
		db.AddObservation(pool, taskID, "merger", note)
	}
	if err := RunMergeOnApprove(ctx, pool, mCfg, prov, teamID, taskID); err != nil {
		return out, err
	}
	out.Ran = true
	return out, nil
}

// shortTaskID is the display form used in approve hints: long enough to
// resolve unambiguously in practice, short enough to type on a phone.
func shortTaskID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// approveCommentRe matches the comment verb: exactly `approve` or
// `maquinista approve`, optional leading slash, optional trailing
// punctuation, case-insensitive. Anything else is prose — a comment that
// merely mentions the word must never merge, so the whole body has to be
// the verb.
var approveCommentRe = regexp.MustCompile(`^(?:/?maquinista\s+approve|/?approve)[.!]?$`)

// IsApproveComment reports whether a comment body is exactly the approve
// verb (line-anchored, whitespace/punctuation-tolerant).
func IsApproveComment(body string) bool {
	return approveCommentRe.MatchString(strings.ToLower(strings.TrimSpace(body)))
}

// IsAllowedApprover matches a comment author against the configured approver
// list (case-insensitive, email or display name). An empty list is
// fail-closed: nobody approves by comment until approvers are configured.
func IsAllowedApprover(author string, approvers []string) bool {
	if author == "" || len(approvers) == 0 {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(author))
	for _, want := range approvers {
		if strings.ToLower(strings.TrimSpace(want)) == a {
			return true
		}
	}
	return false
}

// commentApproveWindow is how far back each pass looks for comments. The
// consumed-comment ledger makes re-reads harmless; the window only bounds
// the provider query and covers a restart or API outage of this length
// without losing an approve.
const commentApproveWindow = 20 * time.Minute

// CommentApprover wires the comment-approve pass to the merge verb. The
// split keeps provider/gh wiring at the cmd layer; tests substitute fakes.
type CommentApprover struct {
	// Approvers: ticket-system identities allowed to approve (empty =
	// fail-closed).
	Approvers []string
	// Approve runs the verb for a resolved task id. May be nil in tests
	// that only exercise parsing/filtering (consumption still happens).
	Approve func(ctx context.Context, taskID, actor string) (*ApproveOutcome, error)
}

// CommentApprovalsOnce is one comment-approve pass: for every pipeline task
// in ready_to_merge that maps to a ticket issue, fetch recent comments via
// the provider (when it implements CommentFetcher) and run the approve verb
// for each well-formed comment from a configured approver. Non-allowed
// authors are ignored silently; comments on anything but a ready_to_merge
// candidate are ignored by construction. Returns the number of approves
// processed. The comment is consumed (ticket_comment_log) BEFORE the verb
// runs, so a second identical comment — or a pagination overlap — can never
// double-merge.
func CommentApprovalsOnce(ctx context.Context, pool *pgxpool.Pool, prov TicketProvider, teamID string, ca CommentApprover) (int, error) {
	fetcher, ok := prov.(CommentFetcher)
	if !ok {
		return 0, nil
	}

	// Candidates: pipeline tasks in ready_to_merge with a ticket mapping.
	rows, err := pool.Query(ctx, `
		SELECT m.issue_id, m.task_id
		FROM   ticket_issue_map m
		JOIN   tasks t ON t.id = m.task_id
		WHERE  t.status = 'ready_to_merge'
	`)
	if err != nil {
		return 0, fmt.Errorf("pipeline: comment approvals select: %w", err)
	}
	defer rows.Close()
	candidates := map[string]string{} // issue id → task id
	for rows.Next() {
		var issueID, taskID string
		if err := rows.Scan(&issueID, &taskID); err != nil {
			return 0, fmt.Errorf("pipeline: comment approvals scan: %w", err)
		}
		candidates[issueID] = taskID
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pipeline: comment approvals rows: %w", err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	issueIDs := make([]string, 0, len(candidates))
	for id := range candidates {
		issueIDs = append(issueIDs, id)
	}
	comments, err := fetcher.RecentComments(ctx, issueIDs, time.Now().Add(-commentApproveWindow))
	if err != nil {
		return 0, fmt.Errorf("pipeline: comment approvals fetch: %w", err)
	}

	processed := 0
	for _, c := range comments {
		if !IsApproveComment(c.Body) {
			continue
		}
		taskID, ok := candidates[c.IssueID]
		if !ok {
			continue // stale comment on an issue whose task moved on
		}
		if !IsAllowedApprover(c.Author, ca.Approvers) {
			// Silent per spec — log at debug-ish detail only.
			log.Printf("pipeline: comment %s on task %s ignored (author not in approvers)", c.ID, taskID)
			continue
		}
		var claimed string
		err := pool.QueryRow(ctx, `
			INSERT INTO ticket_comment_log (comment_id, task_id, action, actor)
			VALUES ($1, $2, 'approve', $3)
			ON CONFLICT (comment_id) DO NOTHING
			RETURNING comment_id
		`, c.ID, taskID, c.Author).Scan(&claimed)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // already consumed — never double-merge
		}
		if err != nil {
			return processed, fmt.Errorf("pipeline: consuming comment %s: %w", c.ID, err)
		}
		if ca.Approve == nil {
			processed++
			continue
		}
		out, err := ca.Approve(ctx, taskID, c.Author)
		if err != nil {
			// The comment stays consumed: the attempt happened, and the
			// operator can approve again (new comment or CLI). Logging the
			// failure here; the merge flow itself already notified when it
			// recorded its own outcome.
			log.Printf("pipeline: comment approve %s (task %s by %s) failed: %v", c.ID, taskID, c.Author, err)
			continue
		}
		processed++
		log.Printf("pipeline: comment approve %s → task %s (status was %s, ran=%v, by %s)",
			c.ID, taskID, out.Status, out.Ran, c.Author)
	}
	return processed, nil
}

// RunCommentApprovals polls for approve comments at the sync cadence (the
// same 10 s pass family as sync/dispatch) until ctx is cancelled. Per-pass
// errors are logged, never fatal. Providers without comment support log
// once and exit.
func RunCommentApprovals(ctx context.Context, pool *pgxpool.Pool, prov TicketProvider, teamID string, interval time.Duration, ca CommentApprover) error {
	if _, ok := prov.(CommentFetcher); !ok {
		log.Printf("pipeline: comment approvals: provider %T has no comment support — skipping", prov)
		return nil
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	log.Printf("pipeline: comment approvals polling every %s (%d approver(s) configured)", interval, len(ca.Approvers))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n, err := CommentApprovalsOnce(ctx, pool, prov, teamID, ca); err != nil {
			log.Printf("pipeline: comment approvals tick: %v", err)
		} else if n > 0 {
			log.Printf("pipeline: comment approvals: processed %d approve comment(s)", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
