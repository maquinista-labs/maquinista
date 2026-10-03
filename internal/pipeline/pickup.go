package pipeline

// PR pickup markers (MAQ-25): while a reviewer or fixer round is
// mid-flight the PR page used to be silent — activity only showed up when
// the round ENDED (the MAQ-16 verdict comment). Dispatch now posts a
// one-line pickup comment on the task's open PR at SPAWN time, so the PR
// conversation reads as a timeline:
//
//	🔁 [MAQ-25] review round 2 started
//	🔧 [MAQ-25] fixer round 2 picked this up - 1) tests missing
//	🚀 [MAQ-25] merge gate running
//
// Contract (task ACs): exactly one comment per spawn (round-scoped dedup
// needle scans the PR's existing comments — repost paths stay no-ops);
// every failure only logs (a dead GitHub path never blocks or fails the
// spawn); tasks without an open PR skip the surface entirely.
//
// All three legs ride the MAQ-16 transport (GhRunner.PRComments +
// PRPostComment) and post with no transaction open.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// issueKeyFromTitle matches the Linear tag a claimed task's title carries
// ("[MAQ-25] Agents post...") — the keyless fallback for taskIssueKey.
var issueKeyFromTitle = regexp.MustCompile(`^\[([A-Za-z]+-\d+)\]`)

// pickupMarker renders the [MAQ-n] tag from the task's Linear issue key;
// keyless tasks (no ticket_issue_map row, no title tag) ship without it.
func pickupMarker(key string) string {
	if key == "" {
		return ""
	}
	return " [" + key + "]"
}

// reviewPickupBody renders the reviewer round's pickup line.
func reviewPickupBody(key string, round int) string {
	return fmt.Sprintf("🔁%s review round %d started", pickupMarker(key), round)
}

// fixerPickupBody renders the fixer episode's pickup line; reason (already
// one line — fixPickupReason) names what the round is picking up.
func fixerPickupBody(key string, round int, reason string) string {
	return fmt.Sprintf("🔧%s fixer round %d picked this up - %s", pickupMarker(key), round, reason)
}

// mergePickupBody renders the merge leg's pickup line.
func mergePickupBody(key string) string {
	return fmt.Sprintf("🚀%s merge gate running", pickupMarker(key))
}

// Round-scoped dedup needles. Bracket-free fragments of the bodies above,
// chosen so no needle is a substring of another round's comment
// ("round 2 started" does not occur inside "round 12 started") nor of the
// MAQ-16 verdict marker ("[review round 2]" carries no "started").
const mergePickupNeedle = "merge gate running"

func reviewPickupNeedle(round int) string {
	return fmt.Sprintf("review round %d started", round)
}

func fixerPickupNeedle(round int) string {
	return fmt.Sprintf("fixer round %d picked this up", round)
}

// pickupMarkers identifies the pipeline's own PR comments for the
// human-comment feed: emoji + structural fragment pairs from the bodies
// above. The gh CLI may be authenticated as a human account, so the marker
// — not the author — is what identifies our comments (same stance as
// MAQ-16's reviewMarkerPrefix).
var pickupMarkers = []struct{ emoji, head, tail string }{
	{"🔁", "review round ", " started"},
	{"🔧", "fixer round ", " picked this up"},
	{"🚀", mergePickupNeedle, ""},
}

// isOwnPRComment reports whether body was posted by the pipeline itself:
// the MAQ-16 verdict comments by their [review round N] marker, the MAQ-25
// pickup markers by emoji + fragment. Used to keep them out of the next
// round's human-comment feed.
func isOwnPRComment(body string) bool {
	if strings.Contains(body, reviewMarkerPrefix) {
		return true
	}
	for _, m := range pickupMarkers {
		if !strings.Contains(body, m.emoji) {
			continue
		}
		if i := strings.Index(body, m.head); i >= 0 && strings.Contains(body[i+len(m.head):], m.tail) {
			return true
		}
	}
	return false
}

// maxPickupReasonChars caps the fixer pickup's one-line reason.
const maxPickupReasonChars = 120

// fixPickupReason distills the reviewer's findings into the fixer pickup's
// one-line reason: the first line of the findings message that carries
// substance (blank and VERDICT lines skipped), tail-capped. Any failure
// yields "" and the body falls back to the generic reason.
func fixPickupReason(ctx context.Context, pool *pgxpool.Pool, reviewerAgent string) string {
	findings, err := latestFindings(ctx, pool, reviewerAgent)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(findings, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "VERDICT:") {
			continue
		}
		if runes := []rune(line); len(runes) > maxPickupReasonChars {
			line = string(runes[:maxPickupReasonChars]) + " …"
		}
		return line
	}
	return ""
}

// genericPickupReason is the fixer pickup's fallback reason.
const genericPickupReason = "addressing review findings"

// taskIssueKey returns the task's Linear issue key for the [MAQ-n] tag:
// ticket_issue_map.issue_key first (canonical — the bridge writes it at
// claim time), the title's [KEY-n] prefix as fallback, "" when neither
// yields one. Best-effort: a lookup failure degrades to the keyless form.
func taskIssueKey(ctx context.Context, q queryRow, taskID string) string {
	var key string
	if err := q.QueryRow(ctx,
		`SELECT issue_key FROM ticket_issue_map WHERE task_id = $1 LIMIT 1`, taskID).Scan(&key); err == nil && key != "" {
		return key
	}
	var title string
	if err := q.QueryRow(ctx,
		`SELECT title FROM tasks WHERE id = $1`, taskID).Scan(&title); err != nil || title == "" {
		return ""
	}
	if m := issueKeyFromTitle.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return ""
}

// postPickupComment posts the one-line pickup marker on the task's open PR
// — best effort, once per dedup needle. Any GitHub failure (list, post) or
// missing PR logs (or stays silent) and returns: the caller's spawn/record
// path is never blocked or failed by this. The needle scan of existing
// comments keeps crash-retry and requeue paths at exactly one comment.
func postPickupComment(ctx context.Context, pool *pgxpool.Pool, g GhRunner, taskID, needle, body string) {
	if g == nil {
		return
	}
	pr, ok, err := taskPR(ctx, pool, taskID)
	if err != nil {
		log.Printf("pipeline: dispatch: pr_url lookup %s: %v (no pickup comment)", taskID, err)
		return
	}
	if !ok {
		return // no PR — nothing to post to (AC 5: no comment, no error)
	}
	comments, err := g.PRComments(ctx, pr, time.Time{})
	if err != nil {
		log.Printf("pipeline: dispatch: PR #%d comments for %s: %v — skipping pickup post", pr, taskID, err)
		return
	}
	for _, c := range comments {
		if strings.Contains(c.Body, needle) {
			return // already posted for this round/leg (crash-retry path)
		}
	}
	if err := g.PRPostComment(ctx, pr, body); err != nil {
		log.Printf("pipeline: dispatch: PR #%d pickup comment for %s: %v (continuing)", pr, taskID, err)
		return
	}
	log.Printf("pipeline: dispatch: posted pickup marker on PR #%d for %s: %s", pr, taskID, body)
}
