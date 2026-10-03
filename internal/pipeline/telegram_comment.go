// Telegram reply → PR comment (MAQ-24): a plain reply to a pipeline
// notification in the Telegram Pipeline topic is posted verbatim as a
// comment on that task's open PR. The comment is authored by the gh CLI
// account (a human login) and carries no `[review round N]` marker, so
// MAQ-16's human-comment weighing folds it into the next review round
// unchanged — this file only creates it, the reviewer machinery consumes it.
//
// Mechanics:
//   - target resolution: notifications stamp their task id into the outbox
//     content (`task_id` key, NotifyTask). A reply resolves via
//     channel_deliveries.external_msg_id (the Telegram message id the
//     dispatcher recorded for the notification) → agent_outbox → task_id.
//     Notification prose is never parsed.
//   - exactly-once: telegram_pr_comments (migration 039) claims the reply's
//     (chat_id, message_id) BEFORE posting — a retried/redelivered update of
//     the same reply message loses the INSERT race and never double-posts.
//   - guard: the task must have an OPEN PR (ErrNoOpenPR otherwise) — a task
//     without a PR, or with a merged/closed one, gets one graceful reply and
//     nothing is posted.
//   - verbs stay MAQ-11's: the in-topic `approve <ref>` intercept runs before
//     this surface (handlers.go ordering), and `maquinista <verb>`-shaped
//     replies are refused here — posting them would arm the GitHub comment-
//     command surface (MAQ-12) from chat text.
package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoOpenPR: the task has no PR URL, an unparseable (non-GitHub) one, or
// the PR is not open. The caller replies gracefully; nothing is posted.
var ErrNoOpenPR = errors.New("no open PR for task")

// PRCommentPoster is the slice of the GitHub surface the Telegram-reply path
// needs (gh.Runner in production, a fake in tests). Kept separate from
// CommentSource (the MAQ-12 poller) so the two surfaces evolve independently.
type PRCommentPoster interface {
	// PRState returns the PR's state: "OPEN", "CLOSED" or "MERGED".
	PRState(ctx context.Context, pr int) (string, error)
	// PRPostCommentURL posts body as a new PR comment and returns the
	// comment's URL (the delivery confirmation quoted back into the topic).
	PRPostCommentURL(ctx context.Context, pr int, body string) (string, error)
}

// PostPRComment posts body as a comment on the task's open PR and returns
// the PR number plus the comment URL. Guards in order: pr_url present and a
// GitHub pull URL, PR state OPEN (a merged/closed PR is ErrNoOpenPR too —
// commenting on one can never reach a review round), then the post itself.
func PostPRComment(ctx context.Context, pool *pgxpool.Pool, poster PRCommentPoster, taskID, body string) (int, string, error) {
	var prURL *string
	if err := pool.QueryRow(ctx, `SELECT pr_url FROM tasks WHERE id = $1`, taskID).Scan(&prURL); err != nil {
		return 0, "", fmt.Errorf("pipeline: pr comment: loading task %s: %w", taskID, err)
	}
	if prURL == nil || *prURL == "" {
		return 0, "", fmt.Errorf("%w: task %s has no pr_url", ErrNoOpenPR, taskID)
	}
	pr, err := prFromPullURL(*prURL)
	if err != nil {
		return 0, "", fmt.Errorf("%w: task %s: %v", ErrNoOpenPR, taskID, err)
	}
	state, err := poster.PRState(ctx, pr)
	if err != nil {
		return 0, "", fmt.Errorf("pipeline: pr comment: PR #%d state: %w", pr, err)
	}
	if state != "OPEN" {
		return 0, "", fmt.Errorf("%w: PR #%d of task %s is %s", ErrNoOpenPR, pr, taskID, state)
	}
	url, err := poster.PRPostCommentURL(ctx, pr, body)
	if err != nil {
		return pr, "", fmt.Errorf("pipeline: pr comment on PR #%d: %w", pr, err)
	}
	return pr, url, nil
}

// TaskForTelegramReply resolves the task a replied-to pipeline notification
// refers to. ok=false when the message is not a delivered pipeline
// notification (or predates task stamping) — the caller falls through to the
// routing ladder, exactly as before this surface existed.
func TaskForTelegramReply(ctx context.Context, pool *pgxpool.Pool, chatID int64, replyToMsgID int) (taskID string, ok bool, err error) {
	var id *string
	err = pool.QueryRow(ctx, `
		SELECT o.content->>'task_id'
		FROM   channel_deliveries d
		JOIN   agent_outbox o ON o.id = d.outbox_id
		WHERE  d.channel = 'telegram'
		  AND  d.chat_id = $1
		  AND  d.external_msg_id = $2
		  AND  o.agent_id = $3
		LIMIT 1
	`, chatID, replyToMsgID, NotifyAgentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("pipeline: telegram reply lookup (chat %d msg %d): %w", chatID, replyToMsgID, err)
	}
	if id == nil || *id == "" {
		return "", false, nil // delivered before NotifyTask stamped the task
	}
	return *id, true, nil
}

// ShortTaskRef is the display form of a task id for replies/hints: long
// enough to resolve unambiguously, short enough to type on a phone.
func ShortTaskRef(id string) string { return shortTaskID(id) }

// Dispositions recorded in telegram_pr_comments.
const (
	TgDispPending = "pending"
	TgDispOK      = "ok"
	TgDispNoOp    = "no_op"
	TgDispError   = "error"
)

// ClaimTelegramComment records the processed-reply memory: the INSERT (PK =
// (chat_id, message_id), ON CONFLICT DO NOTHING) IS the claim. Returns false
// when this reply was already handled — a retried or redelivered delivery
// must post at most once.
func ClaimTelegramComment(ctx context.Context, pool *pgxpool.Pool, chatID int64, msgID int, taskID string) (bool, error) {
	tag, err := pool.Exec(ctx, `
		INSERT INTO telegram_pr_comments (chat_id, message_id, task_id)
		VALUES ($1, $2, NULLIF($3, ''))
		ON CONFLICT (chat_id, message_id) DO NOTHING
	`, chatID, msgID, taskID)
	if err != nil {
		return false, fmt.Errorf("pipeline: claiming telegram reply (chat %d msg %d): %w", chatID, msgID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetTelegramCommentDisposition records what the reply-comment attempt did.
// pr < 0 clears the column (NULL — no PR resolved).
func SetTelegramCommentDisposition(ctx context.Context, pool *pgxpool.Pool, chatID int64, msgID int, disposition, detail, commentURL string, pr int) error {
	var prArg any
	if pr >= 0 {
		prArg = pr
	}
	_, err := pool.Exec(ctx, `
		UPDATE telegram_pr_comments
		SET    disposition = $3, detail = $4, comment_url = $5, pr = $6
		WHERE  chat_id = $1 AND message_id = $2
	`, chatID, msgID, disposition, detail, commentURL, prArg)
	if err != nil {
		return fmt.Errorf("pipeline: telegram reply disposition (chat %d msg %d): %w", chatID, msgID, err)
	}
	return nil
}
