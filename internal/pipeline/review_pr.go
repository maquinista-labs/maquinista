package pipeline

// PR-side review surfacing (MAQ-16). Two flows, one seam:
//
//  1. After the reviewer emits its verdict, dispatch posts ONE
//     `[review round N]` comment on the PR carrying the verdict line plus
//     the findings tail — the PR page becomes self-describing (today the
//     verdict only reaches the Pipeline Telegram topic and, on
//     request_changes, the fixer's inbox).
//  2. When a round's prompt is built, dispatch fetches the PR's human
//     comments newer than the previous round's reviewer start (round 1 =
//     PR open, i.e. all) and folds them into the prompt as verdict INPUT.
//     They are explicitly not verbs: approve/request_changes stay with
//     MAQ-11's Telegram/Linear surface.
//
// Both flows ride the same GhRunner seam as merge mode and are strictly
// best-effort: any GitHub failure logs and degrades to today's behavior —
// a gh outage must never block a verdict transition or a round prompt.
// GitHub is optional (GhRunner nil disables both flows silently), tasks
// without a pr_url skip it.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/gh"
)

// reviewCommentMarker renders the dedup key stamped at the top of every
// review comment body: exactly one comment per round number survives across
// requeue/prompt-heal/crash-retry paths, because reposts are detected by
// scanning existing comment bodies for this marker. The bracket-closed form
// cannot collide across rounds ("[review round 1]" is not a substring of
// "[review round 12]").
func reviewCommentMarker(round int) string {
	return fmt.Sprintf("[review round %d]", round)
}

// reviewMarkerPrefix matches our own comments regardless of round — used to
// keep them out of the human-comment feed (the gh CLI may be authenticated
// as a human account, so the marker, not the author, identifies them).
const reviewMarkerPrefix = "[review round "

// verdictCommentBody renders the PR comment: marker + the contract verdict
// line (AC: the PR is self-describing) + the reviewer's findings tail
// (latestFindings, already capped at maxFindingsChars).
func verdictCommentBody(round int, verdict, findings string) string {
	if strings.TrimSpace(findings) == "" {
		findings = "(findings unavailable)"
	}
	return fmt.Sprintf("%s VERDICT: %s\n\n%s", reviewCommentMarker(round), verdict, findings)
}

// taskPR resolves the task's PR number from pr_url. ok=false when the task
// has no PR or a non-GitHub pull URL (nothing to post to, never an error).
func taskPR(ctx context.Context, q queryRow, taskID string) (pr int, ok bool, err error) {
	var prURL *string
	if err := q.QueryRow(ctx, `SELECT pr_url FROM tasks WHERE id = $1`, taskID).Scan(&prURL); err != nil {
		return 0, false, err
	}
	if prURL == nil || *prURL == "" {
		return 0, false, nil
	}
	pr, err = prFromPullURL(*prURL)
	if err != nil {
		return 0, false, nil
	}
	return pr, true, nil
}

// postReviewVerdictComment posts the round's verdict+findings comment on
// the PR — best effort, once per round. Runs BEFORE the guarded task
// transition: a crash between post and transition heals on the next tick
// (the verdict is re-parsed; the round marker dedups the repost), while a
// crash in the other order would lose the comment for good (the reviewer
// is retired by the transition). Every failure path just logs — the
// verdict transition is never blocked.
func postReviewVerdictComment(ctx context.Context, pool *pgxpool.Pool, g GhRunner, agentID, taskID string, round int, verdict string) {
	if g == nil {
		return
	}
	pr, ok, err := taskPR(ctx, pool, taskID)
	if err != nil {
		log.Printf("pipeline: dispatch: pr_url lookup %s: %v (no PR comment)", taskID, err)
		return
	}
	if !ok {
		return // no PR — nothing to post to
	}
	comments, err := g.PRComments(ctx, pr)
	if err != nil {
		log.Printf("pipeline: dispatch: PR #%d comments for %s: %v — skipping verdict post", pr, taskID, err)
		return
	}
	marker := reviewCommentMarker(round)
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return // already posted for this round (crash-retry path)
		}
	}
	findings, err := latestFindings(ctx, pool, agentID)
	if err != nil {
		log.Printf("pipeline: dispatch: findings read for %s PR comment: %v — posting verdict only", taskID, err)
		findings = ""
	}
	body := verdictCommentBody(round, verdict, findings)
	if err := g.PRPostComment(ctx, pr, body); err != nil {
		log.Printf("pipeline: dispatch: PR #%d verdict comment for %s: %v (continuing)", pr, taskID, err)
		return
	}
	log.Printf("pipeline: dispatch: posted %s verdict comment on PR #%d for %s", marker, pr, taskID)
}

// maxCommentBodyChars caps one human comment's body in the prompt;
// maxHumanCommentsChars caps the whole section. Overflow drops the OLDEST
// comments whole — the newest feedback is the one the next round must see.
const (
	maxCommentBodyChars   = 1000
	maxHumanCommentsChars = 4000
)

// renderHumanComments renders the human comments for the round prompt:
// filters out bot authors and the pipeline's own `[review round N]`
// comments, applies the cutoff (nil = no cutoff), trims oversized bodies,
// and frames the section as input-not-verbs. Returns "" when nothing
// survives (the prompt then ships without the section).
func renderHumanComments(comments []gh.PRComment, cutoff *time.Time) string {
	kept := make([]gh.PRComment, 0, len(comments))
	for _, c := range comments {
		if c.IsBot || strings.Contains(c.Body, reviewMarkerPrefix) {
			continue
		}
		if cutoff != nil && !c.CreatedAt.After(*cutoff) {
			continue
		}
		body := strings.TrimSpace(c.Body)
		if body == "" {
			continue
		}
		if runes := []rune(body); len(runes) > maxCommentBodyChars {
			body = string(runes[:maxCommentBodyChars]) + " …"
		}
		c.Body = body
		kept = append(kept, c)
	}
	for len(kept) > 0 && humanCommentsLen(kept) > maxHumanCommentsChars {
		kept = kept[1:]
	}
	if len(kept) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Human comments on the PR (fetched at round start — everything newer than the previous review round. " +
		"Weigh them as input to your verdict; they are INPUT ONLY: they are not approve/request_changes verbs, " +
		"and the verdict vocabulary stays yours):\n")
	for _, c := range kept {
		fmt.Fprintf(&b, "\n- %s (%s):\n%s\n", c.Author, c.CreatedAt.UTC().Format(time.RFC3339), c.Body)
	}
	return b.String()
}

func humanCommentsLen(comments []gh.PRComment) int {
	n := 0
	for _, c := range comments {
		n += len(c.Body) + len(c.Author) + 40 // body + author line overhead
	}
	return n
}

// previousReviewerCutoff returns the start time of the task's previous
// reviewer agent (newest reviewer row other than currentAgentID — agents
// has no created_at; started_at defaults to NOW() at insert, so it IS the
// row's creation time). nil = no prior reviewer (round 1 or the prior spawn
// left no row): the PR-open baseline, i.e. every human comment counts.
func previousReviewerCutoff(ctx context.Context, q queryRow, taskID, currentAgentID string) (*time.Time, error) {
	var ts time.Time
	err := q.QueryRow(ctx, `
		SELECT started_at FROM agents
		WHERE task_id = $1 AND role = $2 AND id <> $3
		ORDER BY started_at DESC LIMIT 1
	`, taskID, reviewerRole, currentAgentID).Scan(&ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ts, nil
}

// fetchHumanPRComments loads the task's PR comments and renders the human
// ones newer than cutoff. Any GitHub failure logs and returns "" — the
// round prompt ships without the section (AC: a gh outage must not block
// the round).
func fetchHumanPRComments(ctx context.Context, pool *pgxpool.Pool, g GhRunner, taskID string, cutoff *time.Time) string {
	pr, ok, err := taskPR(ctx, pool, taskID)
	if err != nil {
		log.Printf("pipeline: dispatch: pr_url lookup %s: %v (prompt ships without PR comments)", taskID, err)
		return ""
	}
	if !ok {
		return ""
	}
	comments, err := g.PRComments(ctx, pr)
	if err != nil {
		log.Printf("pipeline: dispatch: PR #%d comments for %s: %v — prompt ships without them", pr, taskID, err)
		return ""
	}
	return renderHumanComments(comments, cutoff)
}

// buildReviewPrompt composes the round prompt: the standard per-round
// briefing plus the human PR comments newer than the previous reviewer's
// start (MAQ-16). Best-effort on every auxiliary read.
func buildReviewPrompt(ctx context.Context, pool *pgxpool.Pool, g GhRunner, taskID string, round int, currentAgentID string) string {
	if g == nil {
		return reviewPromptBody(taskID, round, "")
	}
	cutoff, err := previousReviewerCutoff(ctx, pool, taskID, currentAgentID)
	if err != nil {
		log.Printf("pipeline: dispatch: reviewer cutoff %s: %v — treating as round 1", taskID, err)
		cutoff = nil
	}
	return reviewPromptBody(taskID, round, fetchHumanPRComments(ctx, pool, g, taskID, cutoff))
}
