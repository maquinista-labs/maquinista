package bot

// Tests for the Pipeline-topic approve verb (MAQ-11): the in-topic text
// regex, the fail-closed topic gate, and audit actor naming. DB-backed
// interception is covered by the pipeline package (exactly-once pass);
// these pin the bot-side decision logic.

import (
	"context"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestApproveTextRe(t *testing.T) {
	cases := []struct {
		text   string
		wantID string
	}{
		{"approve 5dcb9d88", "5dcb9d88"},
		{"Approve 5dcb9d88-a145-46b2-a3de-504a1528bbfe", "5dcb9d88-a145-46b2-a3de-504a1528bbfe"},
		{"/approve 5dcb9d88", "5dcb9d88"},
		{"  approve   tv-approv  ", "tv-approv"},
	}
	for _, c := range cases {
		// The interception path trims before matching — mirror it.
		m := approveTextRe.FindStringSubmatch(strings.TrimSpace(c.text))
		if m == nil {
			t.Errorf("approveTextRe(%q) did not match", c.text)
			continue
		}
		if m[1] != c.wantID {
			t.Errorf("approveTextRe(%q) ref = %q, want %q", c.text, m[1], c.wantID)
		}
	}
	negatives := []string{
		"",
		"approve",
		"approved 5dcb9d88",
		"approve 5dcb9d88 now",
		"please approve 5dcb9d88",
		"disapprove 5dcb9d88",
	}
	for _, text := range negatives {
		if m := approveTextRe.FindStringSubmatch(text); m != nil {
			t.Errorf("approveTextRe(%q) matched %v, want no match", text, m)
		}
	}
}

func TestIsPipelineTopic_FailsClosed(t *testing.T) {
	b := newTestBot(t)
	// Nil pool (no DATABASE_URL): the gate must report false, never panic —
	// a false would strand the routing ladder, a true would hijack it.
	if b.isPipelineTopic(nil, "123") {
		t.Fatal("nil pool must fail closed")
	}
	// Degenerate thread ids can't be a pipeline topic.
	if b.isPipelineTopic(nil, "") || b.isPipelineTopic(nil, "0") {
		t.Fatal("empty/thread-0 must fail closed")
	}
}

func TestHandlePipelineApproveText_NoMatchFallsThrough(t *testing.T) {
	b := newTestBot(t)
	// Prose that merely contains the word must fall through to the routing
	// ladder even without any pool — the regex gate fires first.
	msg := &tgbotapi.Message{Text: "please approve this approach"}
	if b.handlePipelineApproveText(msg) {
		t.Fatal("prose must not be intercepted")
	}
	// Empty message (non-text updates) falls through.
	if b.handlePipelineApproveText(&tgbotapi.Message{}) {
		t.Fatal("empty message must not be intercepted")
	}
}

func TestActorName(t *testing.T) {
	if got := actorName(&tgbotapi.User{ID: 42, UserName: "op"}); got != "@op" {
		t.Errorf("actorName = %q, want @op", got)
	}
	if got := actorName(&tgbotapi.User{ID: 42, FirstName: "Ada", LastName: "L"}); got != "Ada L" {
		t.Errorf("actorName = %q, want Ada L", got)
	}
	if got := actorName(&tgbotapi.User{ID: 42}); got != "42" {
		t.Errorf("actorName = %q, want 42", got)
	}
	if got := actorName(nil); got != "unknown" {
		t.Errorf("actorName(nil) = %q, want unknown", got)
	}
}

func TestSetApproveFunc(t *testing.T) {
	b := newTestBot(t)
	if b.approveFn != nil {
		t.Fatal("approveFn must default to nil (verb replies 'not wired')")
	}
	called := false
	b.SetApproveFunc(func(ctx context.Context, taskRef, actor string) (bool, string, error) {
		called = true
		return true, "ready_to_merge", nil
	})
	if b.approveFn == nil {
		t.Fatal("SetApproveFunc did not store the fn")
	}
	if _, _, err := b.approveFn(context.Background(), "x", "y"); err != nil || !called {
		t.Fatalf("approveFn call = (called %v, err %v)", called, err)
	}
}
