package bot

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/listener"
)

// ApprovalHandler posts pending_approval tasks to the #approvals topic with inline keyboards.
type ApprovalHandler struct {
	bot *Bot
}

// NewApprovalHandler creates an approval handler wired to the bot.
func NewApprovalHandler(b *Bot) *ApprovalHandler {
	return &ApprovalHandler{bot: b}
}

// HandlePendingApproval is called by the EventRouter when a task transitions to pending_approval.
func (h *ApprovalHandler) HandlePendingApproval(ev listener.TaskEvent) {
	topicID := h.bot.config.ApprovalsTopicID
	if topicID == 0 {
		log.Printf("approval: TRAMUNTANA_APPROVALS_TOPIC_ID not configured, skipping task %s", ev.TaskID)
		return
	}

	// Fetch full task details.
	detail, err := h.bot.minuanoBridge.Show(ev.TaskID)
	if err != nil {
		log.Printf("approval: failed to fetch task %s: %v", ev.TaskID, err)
		return
	}

	// Format message.
	project := ""
	if detail.Task.ProjectID != nil {
		project = *detail.Task.ProjectID
	}

	body := detail.Task.Body
	if len(body) > 300 {
		body = body[:300] + "..."
	}

	text := fmt.Sprintf("Approval required\n\n%s\nProject: %s\n\n%s",
		detail.Task.Title, project, body)

	// Build inline keyboard.
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Approve", "approval_approve:"+ev.TaskID),
			tgbotapi.NewInlineKeyboardButtonData("Revise", "approval_revise:"+ev.TaskID),
			tgbotapi.NewInlineKeyboardButtonData("Reject", "approval_reject:"+ev.TaskID),
		),
	)

	// Find the chat ID for the approvals topic.
	chatID := h.bot.findChatIDForTopic(int(topicID))
	if chatID == 0 {
		log.Printf("approval: no chat ID found for approvals topic %d", topicID)
		return
	}

	if _, err := h.bot.sendMessageWithKeyboard(chatID, int(topicID), text, kb); err != nil {
		log.Printf("approval: failed to send approval message for %s: %v", ev.TaskID, err)
	}
}

// processApprovalCallback handles approval inline keyboard callbacks.
func (b *Bot) processApprovalCallback(cq *tgbotapi.CallbackQuery) {
	parts := strings.SplitN(cq.Data, ":", 2)
	if len(parts) < 2 {
		return
	}
	action, taskID := parts[0], parts[1]

	switch action {
	case "approval_approve":
		userID := strconv.FormatInt(cq.From.ID, 10)
		_, err := b.minuanoBridge.Run("approve", taskID, "--by", userID)
		if err != nil {
			b.answerCallback(cq.ID, fmt.Sprintf("Error: %v", err))
			return
		}

		username := cq.From.UserName
		if username == "" {
			username = cq.From.FirstName
		}

		// Edit the original message.
		if cq.Message != nil {
			edit := tgbotapi.NewEditMessageText(
				cq.Message.Chat.ID,
				cq.Message.MessageID,
				fmt.Sprintf("Approved by %s. Task is now ready.\n\nTask: %s", username, taskID),
			)
			b.api.Send(edit)
		}
		b.answerCallback(cq.ID, "Approved")

	case "approval_reject":
		// Send a prompt asking for optional rejection reason.
		if cq.Message != nil {
			kb := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("Skip (no reason)", "approval_reject_final:"+taskID+":"),
				),
			)
			reply := tgbotapi.NewEditMessageTextAndMarkup(
				cq.Message.Chat.ID,
				cq.Message.MessageID,
				fmt.Sprintf("Rejecting task %s.\nReply with a reason, or tap Skip:", taskID),
				kb,
			)
			b.api.Send(reply)
		}
		b.setPendingInput(cq.From.ID, "approval_reject_reason:"+taskID,
			cq.Message.Chat.ID, getThreadIDFromCallback(cq))

	case "approval_reject_final":
		// Parse: approval_reject_final:<taskID>:<reason>
		subParts := strings.SplitN(taskID, ":", 2)
		actualTaskID := subParts[0]
		reason := ""
		if len(subParts) > 1 {
			reason = subParts[1]
		}

		args := []string{"reject", actualTaskID}
		if reason != "" {
			args = append(args, "--reason", reason)
		}
		_, err := b.minuanoBridge.Run(args...)
		if err != nil {
			b.answerCallback(cq.ID, fmt.Sprintf("Error: %v", err))
			return
		}

		if cq.Message != nil {
			msg := fmt.Sprintf("Rejected. Task: %s", actualTaskID)
			if reason != "" {
				msg += "\nReason: " + reason
			}
			edit := tgbotapi.NewEditMessageText(
				cq.Message.Chat.ID,
				cq.Message.MessageID,
				msg,
			)
			b.api.Send(edit)
		}
		b.answerCallback(cq.ID, "Rejected")

	case "approval_revise":
		b.answerCallback(cq.ID, "Revise: send your feedback in this topic.")
		// Set pending input to route next message to planner
		if cq.Message != nil {
			b.setPendingInput(cq.From.ID, "approval_revise:"+taskID,
				cq.Message.Chat.ID, getThreadIDFromCallback(cq))
		}
	}
}

// getThreadIDFromCallback extracts thread ID from a callback query message.
func getThreadIDFromCallback(cq *tgbotapi.CallbackQuery) int {
	if cq.Message != nil {
		return getThreadID(cq.Message)
	}
	return 0
}

// findChatIDForTopic finds the chat ID associated with a topic.
// It looks through stored group chat IDs in state.
func (b *Bot) findChatIDForTopic(topicID int) int64 {
	// Try to find from existing state bindings.
	for _, userID := range b.state.AllUserIDs() {
		threadIDStr := strconv.Itoa(topicID)
		if chatID, ok := b.state.GetGroupChatID(userID, threadIDStr); ok && chatID != 0 {
			return chatID
		}
	}
	// Fallback: use first allowed group
	if len(b.config.AllowedGroups) > 0 {
		return b.config.AllowedGroups[0]
	}
	return 0
}
