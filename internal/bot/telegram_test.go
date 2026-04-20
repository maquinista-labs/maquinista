package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestExtractForumFields_ThreadID(t *testing.T) {
	raw := []byte(`{"message": {"message_id": 100, "message_thread_id": 42, "chat": {"id": 123}, "date": 0}}`)
	extractForumFields(raw)

	msg := &tgbotapi.Message{MessageID: 100}
	threadID := getThreadID(msg)
	if threadID != 42 {
		t.Errorf("getThreadID = %d, want 42", threadID)
	}

	// Cleanup
	threadCacheMu.Lock()
	delete(threadIDCache, 100)
	threadCacheMu.Unlock()
}

func TestExtractForumFields_NoThread(t *testing.T) {
	raw := []byte(`{"message": {"message_id": 101, "chat": {"id": 123}, "date": 0}}`)
	extractForumFields(raw)

	msg := &tgbotapi.Message{MessageID: 101}
	threadID := getThreadID(msg)
	if threadID != 0 {
		t.Errorf("getThreadID = %d, want 0", threadID)
	}
}

func TestExtractForumFields_TopicClosed(t *testing.T) {
	raw := []byte(`{"message": {"message_id": 102, "chat": {"id": 123}, "forum_topic_closed": {}, "date": 0}}`)
	extractForumFields(raw)

	msg := &tgbotapi.Message{MessageID: 102}
	if !isForumTopicClosed(msg) {
		t.Error("should detect forum topic closed")
	}

	// Cleanup
	threadCacheMu.Lock()
	delete(topicClosedSet, 102)
	threadCacheMu.Unlock()
}

func TestIsForumTopicClosed_Normal(t *testing.T) {
	msg := &tgbotapi.Message{MessageID: 999}
	if isForumTopicClosed(msg) {
		t.Error("should not detect forum topic closed on normal message")
	}
}

func TestCleanupCache(t *testing.T) {
	threadCacheMu.Lock()
	threadIDCache[10] = 1
	threadIDCache[20] = 2
	threadIDCache[30] = 3
	topicClosedSet[15] = true
	threadCacheMu.Unlock()

	cleanupCache(25) // remove entries below 25

	threadCacheMu.RLock()
	defer threadCacheMu.RUnlock()

	if _, ok := threadIDCache[10]; ok {
		t.Error("10 should be cleaned up")
	}
	if _, ok := threadIDCache[20]; ok {
		t.Error("20 should be cleaned up")
	}
	if _, ok := threadIDCache[30]; !ok {
		t.Error("30 should still be present")
	}
	if _, ok := topicClosedSet[15]; ok {
		t.Error("15 should be cleaned up")
	}

	// Cleanup remaining
	delete(threadIDCache, 30)
}

func TestGetThreadID_NilMessage(t *testing.T) {
	if getThreadID(nil) != 0 {
		t.Error("nil message should return 0")
	}
}

func TestIsForumTopicClosed_NilMessage(t *testing.T) {
	if isForumTopicClosed(nil) {
		t.Error("nil message should return false")
	}
}

func TestSlugifyTopicName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"inbox", "inbox"},
		{"Inbox", "inbox"},
		{"My Coder", "my-coder"},
		{"🔥 fire agent", "fire-agent"},
		{"  --spaces--  ", "spaces"},
		{"t-800", "t-800"},   // t- prefix: SetHandle rejects it; slugify leaves as-is
		{"a", ""},            // too short
		{"", ""},
		{"hello/world.bot", "hello-world-bot"},
		{"-leading-dash", "leading-dash"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaXXX", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, // 32 chars
	}
	for _, c := range cases {
		got := slugifyTopicName(c.in)
		if got != c.want {
			t.Errorf("slugifyTopicName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
