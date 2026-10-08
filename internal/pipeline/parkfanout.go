// Park fan-out (MAQ-34). Every needs-human park already posts a 🆘 note to
// the Pipeline topic, but whoever reads the PR or the Linear board got no
// pointer back to HOW to approve. This file is the one code path every park
// site reuses to fan its park out beyond Telegram:
//
//   - one comment on the task's open PR (via gh), and
//   - one comment on the mapped Linear issue (when a ticket_issue_map row
//     exists and the provider can comment),
//
// each stating that human review is needed and quoting the exact approval
// paths (PR-comment verb, CLI approve, Approvals-topic button).
//
// Exactly-once per park EPISODE rides two guards stacked:
//
//  1. call placement — every caller invokes this from the applied branch of
//     its guarded park transition (the same single-winner UPDATE that makes
//     the 🆘 exactly once), so a watchdog tick over an already-parked task
//     never even reaches here; and
//  2. the episode marker — claimParkFanout anchors the fan-out on the
//     newest non-fanout task_context row (the park's own verdict row) and
//     records that anchor as a 'parkfanout' marker row. A second call for
//     the same parked task computes the same anchor, loses the claim, and
//     posts nothing; a RE-park writes a fresh verdict row, the anchor
//     moves, and the new episode fans out again.
//
// Posting is best-effort on both surfaces (the notifyf contract: a failed
// comment is logged, never allowed to fail the park transition it reports).
// A PR without an open PR (no pr_url, merged/closed) degrades to the issue
// comment alone — ErrNoOpenPR is the expected, silent shape.

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ghPoster widens a GhRunner to the PR-comment surface when the concrete
// runner also implements it (gh.Runner does: PRState + PRPostCommentURL).
// Test stubs implementing only GhRunner degrade to no PR comment — the
// fan-out's other legs are unaffected.
func ghPoster(g GhRunner) PRCommentPoster {
	if p, ok := g.(PRCommentPoster); ok {
		return p
	}
	return nil
}

// parkFanoutKind is the task_context kind stamping fan-out dedup markers.
const parkFanoutKind = "parkfanout"

// parkFanout carries the two surfaces a park fans out to (MAQ-34). The zero
// value is legal: nil gh skips the PR comment, nil prov skips the issue
// comment — the Telegram 🆘 always fires from the call site as before.
// Built once per loop (RunDispatch, ProcessMergeGH) from already-wired deps.
type parkFanout struct {
	gh   PRCommentPoster
	prov TicketProvider
}

// notify fans one park out: PR comment + issue comment, deduped per park
// episode. summary is the one-line reason (the site's own 🆘 wording,
// shortened); the approval-paths block is appended here so every surface
// quotes the same paths.
func (f parkFanout) notify(ctx context.Context, pool *pgxpool.Pool, taskID, summary string) {
	if f.gh == nil && f.prov == nil {
		return // no fan-out surfaces wired — Telegram note only
	}
	NotifyParkFanout(ctx, pool, f.gh, f.prov, taskID, summary)
}

// NotifyParkFanout is the exported form (taskscheduler park paths call it
// through a cmd-wired closure). See the file comment for the dedup and
// error contracts.
func NotifyParkFanout(ctx context.Context, pool *pgxpool.Pool, poster PRCommentPoster, prov TicketProvider, taskID, summary string) {
	if poster == nil && prov == nil {
		return
	}
	owner, err := claimParkFanout(ctx, pool, taskID)
	if err != nil {
		log.Printf("pipeline: park fanout %s: claim: %v", taskID, err)
		return
	}
	if !owner {
		return // already fanned out for this park episode
	}
	body := parkFanoutBody(ctx, pool, taskID, summary)

	if poster != nil {
		if _, url, err := PostPRComment(ctx, pool, poster, taskID, body); err != nil {
			if !errors.Is(err, ErrNoOpenPR) {
				log.Printf("pipeline: park fanout %s: PR comment: %v", taskID, err)
			} // else: no open PR — the issue comment alone carries the paths
		} else {
			log.Printf("pipeline: park fanout %s: PR comment posted: %s", taskID, url)
		}
	}
	if prov != nil {
		commenter, ok := prov.(IssueCommenter)
		if !ok {
			return // provider cannot comment (surface stays Telegram-only)
		}
		var issueID *string
		if err := pool.QueryRow(ctx,
			`SELECT issue_id FROM ticket_issue_map WHERE task_id = $1`, taskID).Scan(&issueID); err != nil {
			log.Printf("pipeline: park fanout %s: issue lookup: %v", taskID, err)
			return
		}
		if issueID == nil || *issueID == "" {
			return // unmapped task — no board surface to comment on
		}
		if err := commenter.CommentOnIssue(ctx, *issueID, body); err != nil {
			log.Printf("pipeline: park fanout %s: issue comment: %v", taskID, err)
			return
		}
		log.Printf("pipeline: park fanout %s: issue comment posted on %s", taskID, *issueID)
	}
}

// claimParkFanout reports — and claims — the right to fan out the task's
// CURRENT park episode, in one statement. The anchor is the newest
// non-marker task_context row (every park writes its verdict/observation
// row inside the park tx, so the park row IS the anchor); the INSERT of a
// 'parkfanout' marker carrying that anchor is the claim. A second call for
// the same episode computes the same anchor and inserts nothing; a re-park
// moves the anchor and the fresh episode wins.
func claimParkFanout(ctx context.Context, pool *pgxpool.Pool, taskID string) (bool, error) {
	var claimed int
	if err := pool.QueryRow(ctx, `
		WITH anchor AS (
			SELECT COALESCE(MAX(id), 0) AS id
			FROM   task_context
			WHERE  task_id = $1 AND kind <> $2
		), claim AS (
			INSERT INTO task_context (task_id, kind, content)
			SELECT $1, $2, anchor.id::text FROM anchor
			WHERE NOT EXISTS (
				SELECT 1 FROM task_context m
				WHERE m.task_id = $1 AND m.kind = $2 AND m.content = anchor.id::text
			)
			RETURNING 1
		)
		SELECT count(*) FROM claim
	`, taskID, parkFanoutKind).Scan(&claimed); err != nil {
		return false, fmt.Errorf("pipeline: park fanout claim: %w", err)
	}
	return claimed > 0, nil
}

// parkFanoutBody renders the comment both surfaces carry: the park reason
// plus the exact approval paths (MAQ-34 body: "comment `approve` on the PR,
// or ./maquinista approve <task-uuid> --by <approver> (repo checkout, .env
// sourced), or the Approve button on the Telegram card in the Approvals
// topic").
func parkFanoutBody(ctx context.Context, pool *pgxpool.Pool, taskID, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🆘 %s\n\n", TaskTitle(ctx, pool, taskID))
	if summary != "" {
		b.WriteString(strings.TrimSpace(summary))
		b.WriteString("\n\n")
	}
	b.WriteString("This task is parked pending human review. Ways to approve:\n")
	fmt.Fprintf(&b, "- comment `approve` on the PR, or\n")
	fmt.Fprintf(&b, "- `./maquinista approve %s --by %s` (repo checkout, `.env` sourced), or\n",
		taskID, parkApproverHint())
	b.WriteString("- the Approve button on the Telegram card in the Approvals topic.")
	return b.String()
}

// parkApproverHint resolves the identity quoted in the CLI approval example:
// the first non-email entry of MAQUINISTA_TICKETS_APPROVERS (an approver
// handle, e.g. otaviocarvalho — emails work as --by identities too but a
// handle reads better in an instruction), else the first entry verbatim,
// else a placeholder. Resolved per call: park fan-outs are rare and the
// env is process-fixed.
func parkApproverHint() string {
	for _, a := range strings.Split(os.Getenv("MAQUINISTA_TICKETS_APPROVERS"), ",") {
		if a = strings.TrimSpace(a); a != "" && !strings.Contains(a, "@") {
			return a
		}
	}
	for _, a := range strings.Split(os.Getenv("MAQUINISTA_TICKETS_APPROVERS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			return a
		}
	}
	return "<your-approver-id>"
}
