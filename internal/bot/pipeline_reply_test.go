package bot

// Tests for the Pipeline-topic reply-comment intercept (MAQ-24): the
// bot-side decision logic — which messages the surface may claim. DB-backed
// resolution/claim/posting is covered by the pipeline package
// (telegram_comment_test.go); these pin the fall-through contract.

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func pipelineReplyMsg(text string, replyTo *tgbotapi.Message) *tgbotapi.Message {
	return &tgbotapi.Message{
		Text:           text,
		ReplyToMessage: replyTo,
		Chat:           &tgbotapi.Chat{ID: -1001234},
		From:           &tgbotapi.User{ID: 100, UserName: "otavio"},
	}
}

// A reply to a pipeline notification is only claimed in the Pipeline topic;
// the gate fails closed without a pool (no DATABASE_URL) — a false would
// strand the routing ladder, a true would hijack it.
func TestPipelineReply_FailsClosedOnTopicGate(t *testing.T) {
	b := newTestBot(t)
	msg := pipelineReplyMsg("looks good otherwise", &tgbotapi.Message{MessageID: 7})
	if b.handlePipelineReplyText(msg) {
		t.Fatal("nil pool must fail closed")
	}
}

// The surface only ever claims replies (msg.ReplyToMessage set) with text;
// anything else falls through to the routing ladder untouched.
func TestPipelineReply_IgnoresNonRepliesAndEmptyText(t *testing.T) {
	b := newTestBot(t)

	if b.handlePipelineReplyText(nil) {
		t.Error("nil message must not be claimed")
	}
	// Plain text (no reply) in the pipeline topic: ladder's business.
	if b.handlePipelineReplyText(pipelineReplyMsg("hello?", nil)) {
		t.Error("non-reply text must not be claimed")
	}
	// Empty / whitespace-only reply: nothing to post.
	if b.handlePipelineReplyText(pipelineReplyMsg("   ", &tgbotapi.Message{MessageID: 7})) {
		t.Error("empty reply must not be claimed")
	}
}
