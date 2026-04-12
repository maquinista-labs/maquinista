package bot

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/listener"
	"github.com/maquinista-labs/maquinista/internal/bridge"
)

// QueueHandler updates a live pinned status board in the #queue topic.
type QueueHandler struct {
	bot             *Bot
	pinnedMessageID int
	mu              sync.Mutex
	debounceTimer   *time.Timer
}

// NewQueueHandler creates a queue handler wired to the bot.
func NewQueueHandler(b *Bot) *QueueHandler {
	return &QueueHandler{bot: b}
}

// HandleTaskUpdate is called by the EventRouter on any task status change.
func (h *QueueHandler) HandleTaskUpdate(ev listener.TaskEvent) {
	topicID := h.bot.config.QueueTopicID
	if topicID == 0 {
		return
	}
	if ev.ProjectID == "" {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	projectID := ev.ProjectID

	// Debounce: coalesce events within 2s.
	if h.debounceTimer != nil {
		h.debounceTimer.Stop()
	}
	h.debounceTimer = time.AfterFunc(2*time.Second, func() {
		h.updateBoard(projectID)
	})
}

func (h *QueueHandler) updateBoard(projectID string) {
	topicID := int(h.bot.config.QueueTopicID)
	chatID := h.bot.findChatIDForTopic(topicID)
	if chatID == 0 {
		log.Printf("queue: no chat ID for queue topic %d", topicID)
		return
	}

	tasks, err := h.bot.minuanoBridge.Status(projectID)
	if err != nil {
		log.Printf("queue: error fetching status for %s: %v", projectID, err)
		return
	}

	text := formatStatusBoard(projectID, tasks)

	h.mu.Lock()
	pinnedID := h.pinnedMessageID
	h.mu.Unlock()

	if pinnedID != 0 {
		edit := tgbotapi.NewEditMessageText(chatID, pinnedID, text)
		if _, err := h.bot.api.Send(edit); err != nil {
			log.Printf("queue: error editing pinned message: %v", err)
			h.sendAndPin(chatID, topicID, text)
		}
	} else {
		h.sendAndPin(chatID, topicID, text)
	}
}

func (h *QueueHandler) sendAndPin(chatID int64, topicID int, text string) {
	sent, err := h.bot.sendMessageInThread(chatID, topicID, text)
	if err != nil {
		log.Printf("queue: error sending status board: %v", err)
		return
	}

	h.mu.Lock()
	h.pinnedMessageID = sent.MessageID
	h.mu.Unlock()

	pin := tgbotapi.PinChatMessageConfig{
		ChatID:              chatID,
		MessageID:           sent.MessageID,
		DisableNotification: true,
	}
	if _, err := h.bot.api.Request(pin); err != nil {
		log.Printf("queue: error pinning message: %v", err)
	}
}

func statusEmoji(status string) string {
	switch status {
	case "draft":
		return "\u25cc"
	case "ready":
		return "\u2b1c"
	case "claimed":
		return "\U0001f504"
	case "done":
		return "\u2705"
	case "failed":
		return "\u274c"
	case "pending_approval":
		return "\U0001f514"
	case "rejected":
		return "\U0001f6ab"
	case "pending":
		return "\u25cb"
	default:
		return "?"
	}
}

func formatStatusBoard(projectID string, tasks []bridge.Task) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Project: %s", projectID))
	lines = append(lines, "")

	counts := make(map[string]int)
	for _, t := range tasks {
		counts[t.Status]++
	}

	var summary []string
	for _, s := range []string{"done", "claimed", "ready", "pending", "draft", "pending_approval", "rejected", "failed"} {
		if c := counts[s]; c > 0 {
			summary = append(summary, fmt.Sprintf("%s %s: %d", statusEmoji(s), s, c))
		}
	}
	lines = append(lines, strings.Join(summary, " | "))
	lines = append(lines, "")

	for _, t := range tasks {
		id := t.ID
		if len(id) > 20 {
			id = id[:20]
		}
		lines = append(lines, fmt.Sprintf("%s %s  %s", statusEmoji(t.Status), id, t.Title))
	}

	return strings.Join(lines, "\n")
}
