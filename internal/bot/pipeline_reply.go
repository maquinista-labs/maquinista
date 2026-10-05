package bot

// Pipeline-topic reply comments (MAQ-24): a plain reply to a pipeline
// notification in the Pipeline topic is posted verbatim as a comment on that
// task's open PR, and the comment URL is quoted back into the topic as the
// delivery confirmation. The comment is authored by the gh CLI account with
// no `[review round N]` marker, so the reviewer's human-comment weighing
// (MAQ-16) folds it into the next review round — nothing new on that side.
//
// Ordering: this intercept sits AFTER the approve-verb arm (MAQ-11) —
// `approve <ref>` merges, it never becomes a comment — and BEFORE the
// routing ladder (the synthetic pipeline agent has no sidecar; the ladder
// would strand the message). Replies that are not answers to a pipeline
// notification fall through untouched, exactly as before.
//
// Authorization is the stock isAuthorized gate (ALLOWED_USERS) applied in
// handleUpdate. Exactly-once is the telegram_pr_comments claim
// (pipeline.ClaimTelegramComment): a retried/redelivered reply posts once.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
)

// PipelineReplyFunc posts text as a verbatim comment on the task's open PR
// and returns the PR number plus the comment URL. Injected by cmd_start,
// which owns the gh wiring; a nil func makes replies answer "not wired".
// pipeline.ErrNoOpenPR (task without an open PR) is the documented graceful
// case: one informative reply, nothing posted.
type PipelineReplyFunc func(ctx context.Context, taskID, text string) (pr int, commentURL string, err error)

// SetPipelineReplyFunc injects the reply-comment backend. Call before Run.
func (b *Bot) SetPipelineReplyFunc(fn PipelineReplyFunc) {
	b.pipelineReplyFn = fn
}

// handlePipelineReplyText intercepts a non-verb reply to a pipeline
// notification in the Pipeline topic and lands it on the task's PR. Returns
// false when the message isn't a reply, the thread isn't the Pipeline topic,
// or the replied-to message isn't a pipeline notification — the caller falls
// through to the normal routing ladder (this surface must never hijack a
// regular agent conversation).
func (b *Bot) handlePipelineReplyText(msg *tgbotapi.Message) bool {
	if msg == nil || msg.ReplyToMessage == nil || strings.TrimSpace(msg.Text) == "" {
		return false
	}
	if !b.isPipelineTopic(b.getPool(), strconv.Itoa(getThreadID(msg))) {
		return false
	}
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	replyTo := msg.ReplyToMessage.MessageID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	taskID, ok, err := pipeline.TaskForTelegramReply(ctx, b.getPool(), chatID, replyTo)
	if err != nil {
		log.Printf("pipeline reply: notification lookup (chat %d, reply-to %d): %v", chatID, replyTo, err)
		return false // DB blip: fall through like the topic gate does
	}
	if !ok {
		return false // not a pipeline notification — ladder handles it
	}

	claimed, err := pipeline.ClaimTelegramComment(ctx, b.getPool(), chatID, replyTo, taskID)
	if err != nil {
		log.Printf("pipeline reply: claim (chat %d, msg %d): %v", chatID, replyTo, err)
		b.reply(chatID, threadID, "⚠️ Could not process the reply (DB error) — nothing was posted. Send it again to retry.")
		return true
	}
	if !claimed {
		log.Printf("pipeline reply: duplicate delivery (chat %d, msg %d) — already handled", chatID, replyTo)
		return true // posted once; a redelivered update stops here
	}

	text := strings.TrimSpace(msg.Text)

	// `maquinista <verb>`-shaped replies never become PR comments: posting
	// them would arm the GitHub comment-command surface (MAQ-12) from chat
	// text. The in-topic approve verb was already intercepted upstream.
	if _, _, isCmd := pipeline.ParseCommentCommand(text); isCmd {
		_ = pipeline.SetTelegramCommentDisposition(ctx, b.getPool(), chatID, replyTo,
			pipeline.TgDispNoOp, "maquinista verb reply — not posted", "", -1)
		b.reply(chatID, threadID,
			"ℹ️ `maquinista …` replies are not posted to the PR (they would run as PR commands). Reply with plain feedback, or use `approve <task-ref>` to merge.")
		return true
	}

	if b.pipelineReplyFn == nil {
		_ = pipeline.SetTelegramCommentDisposition(ctx, b.getPool(), chatID, replyTo,
			pipeline.TgDispError, "reply backend not wired", "", -1)
		b.reply(chatID, threadID, "Reply-to-PR is not wired (no gh backend in this process).")
		return true
	}

	// gh round-trips take seconds (state probe + post) — run async so the
	// update loop never stalls; the outcome lands in this topic either way.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		pr, url, err := b.pipelineReplyFn(ctx, taskID, text)
		switch {
		case errors.Is(err, pipeline.ErrNoOpenPR):
			_ = pipeline.SetTelegramCommentDisposition(ctx, b.getPool(), chatID, replyTo,
				pipeline.TgDispNoOp, err.Error(), "", -1)
			b.reply(chatID, threadID,
				fmt.Sprintf("ℹ️ %v — nothing posted. Open (or re-open) the PR for %s and reply here again.", err, pipeline.ShortTaskRef(taskID)))
		case err != nil:
			_ = pipeline.SetTelegramCommentDisposition(ctx, b.getPool(), chatID, replyTo,
				pipeline.TgDispError, err.Error(), "", -1)
			b.reply(chatID, threadID, fmt.Sprintf("⚠️ Failed to post the reply as a PR comment: %v", err))
		default:
			_ = pipeline.SetTelegramCommentDisposition(ctx, b.getPool(), chatID, replyTo,
				pipeline.TgDispOK, "", url, pr)
			// Delivery confirmation: the comment URL quoted back.
			b.reply(chatID, threadID, fmt.Sprintf("💬 Posted to PR #%d: %s\nThe next review round will weigh it.", pr, url))
		}
	}()
	return true
}
