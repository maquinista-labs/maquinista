package bot

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/monitor"
	"github.com/maquinista-labs/maquinista/internal/state"
)

const entriesPerPage = 10

// handleHistoryCommand shows paginated session transcript.
func (b *Bot) handleHistoryCommand(msg *tgbotapi.Message) {
	windowID, bound := b.resolveWindow(msg)
	if !bound {
		b.reply(msg.Chat.ID, getThreadID(msg), "No session bound to this topic.")
		return
	}

	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	jsonlPath := b.findJSONLForWindow(windowID)
	if jsonlPath == "" {
		b.reply(chatID, threadID, "No session transcript found.")
		return
	}

	entries := readAllEntries(jsonlPath)
	if len(entries) == 0 {
		b.reply(chatID, threadID, "Session transcript is empty.")
		return
	}

	// Show last page by default
	totalPages := (len(entries) + entriesPerPage - 1) / entriesPerPage
	page := totalPages - 1

	text := formatHistoryPage(entries, page, windowID)
	keyboard := buildHistoryKeyboard(windowID, page, totalPages)

	if keyboard != nil {
		b.sendMessageWithKeyboard(chatID, threadID, text, *keyboard)
	} else {
		b.reply(chatID, threadID, text)
	}
}

// handleHistoryCB handles history pagination callbacks.
func (b *Bot) handleHistoryCB(cq *tgbotapi.CallbackQuery) {
	page, windowID, ok := parseHistCallbackData(cq.Data)
	if !ok {
		return
	}

	jsonlPath := b.findJSONLForWindow(windowID)
	if jsonlPath == "" {
		return
	}

	entries := readAllEntries(jsonlPath)
	if len(entries) == 0 {
		return
	}

	totalPages := (len(entries) + entriesPerPage - 1) / entriesPerPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	text := formatHistoryPage(entries, page, windowID)
	keyboard := buildHistoryKeyboard(windowID, page, totalPages)

	chatID := cq.Message.Chat.ID
	messageID := cq.Message.MessageID

	if keyboard != nil {
		b.editMessageWithKeyboard(chatID, messageID, text, *keyboard)
	} else {
		b.editMessageText(chatID, messageID, text)
	}
}

// findJSONLForWindow finds the JSONL transcript file for a window.
func (b *Bot) findJSONLForWindow(windowID string) string {
	pool := b.getPool()
	if pool == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sm, err := state.LoadSessionMap(ctx, pool)
	if err != nil {
		return ""
	}

	// Find session entry matching this window
	var sessionID string
	for key, entry := range sm {
		if windowIDFromKey(key) == windowID {
			sessionID = entry.SessionID
			break
		}
	}
	if sessionID == "" {
		return ""
	}

	// Check monitor state for cached path
	if b.monitorState != nil {
		for _, key := range b.monitorState.AllKeys() {
			tracked, ok := b.monitorState.GetTracked(key)
			if ok && tracked.SessionID == sessionID && tracked.FilePath != "" {
				if _, err := os.Stat(tracked.FilePath); err == nil {
					return tracked.FilePath
				}
			}
		}
	}

	// Fallback: scan ~/.claude/projects/ for matching JSONL
	claudeDir := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	dirEntries, err := os.ReadDir(claudeDir)
	if err != nil {
		return ""
	}

	for _, dir := range dirEntries {
		if !dir.IsDir() {
			continue
		}
		projectDir := filepath.Join(claudeDir, dir.Name())
		jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
		if _, err := os.Stat(jsonlPath); err == nil {
			return jsonlPath
		}
	}

	return ""
}

// readAllEntries reads and parses all entries from a JSONL file.
func readAllEntries(path string) []historyEntry {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("Error opening JSONL file %s: %v", path, err)
		return nil
	}
	defer f.Close()

	var entries []historyEntry
	pending := make(map[string]monitor.PendingTool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		entry, err := monitor.ParseLine(line)
		if err != nil || entry == nil {
			continue
		}

		parsed := monitor.ParseEntries([]*monitor.Entry{entry}, pending)
		for _, pe := range parsed {
			entries = append(entries, historyEntry{
				Role:        pe.Role,
				ContentType: pe.ContentType,
				Text:        pe.Text,
				ToolName:    pe.ToolName,
				IsError:     pe.IsError,
			})
		}
	}

	return entries
}

// historyEntry is a simplified entry for history display.
type historyEntry struct {
	Role        string
	ContentType string
	Text        string
	ToolName    string
	IsError     bool
}

// formatHistoryPage formats a page of history entries.
func formatHistoryPage(entries []historyEntry, page int, windowID string) string {
	totalPages := (len(entries) + entriesPerPage - 1) / entriesPerPage

	start := page * entriesPerPage
	end := start + entriesPerPage
	if end > len(entries) {
		end = len(entries)
	}

	pageEntries := entries[start:end]

	var lines []string
	lines = append(lines, fmt.Sprintf("History [%s] — Page %d/%d (%d entries)",
		windowID, page+1, totalPages, len(entries)))
	lines = append(lines, "")

	for _, entry := range pageEntries {
		lines = append(lines, formatHistoryEntry(entry))
	}

	return strings.Join(lines, "\n")
}

// formatHistoryEntry formats a single history entry concisely.
func formatHistoryEntry(entry historyEntry) string {
	switch entry.ContentType {
	case "text":
		prefix := ">"
		if entry.Role == "user" {
			prefix = "You:"
		}
		text := truncateText(entry.Text, 100)
		return prefix + " " + text

	case "tool_use":
		return "Tool: " + truncateText(entry.Text, 80)

	case "tool_result":
		lineCount := strings.Count(entry.Text, "\n") + 1
		if entry.IsError {
			return fmt.Sprintf("Result [%s]: ERROR (%d lines)", entry.ToolName, lineCount)
		}
		return fmt.Sprintf("Result [%s]: %d lines", entry.ToolName, lineCount)

	case "thinking":
		text := truncateText(entry.Text, 60)
		return "Thinking: " + text

	default:
		return truncateText(entry.Text, 100)
	}
}

// truncateText truncates text to maxLen characters, appending "..." if truncated.
func truncateText(text string, maxLen int) string {
	// Take first line only
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	runes := []rune(text)
	if len(runes) > maxLen {
		return string(runes[:maxLen]) + "..."
	}
	return text
}

// buildHistoryKeyboard builds pagination keyboard for history.
func buildHistoryKeyboard(windowID string, page, totalPages int) *tgbotapi.InlineKeyboardMarkup {
	if totalPages <= 1 {
		return nil
	}

	var buttons []tgbotapi.InlineKeyboardButton

	if page > 0 {
		data := formatHistCallback(page-1, windowID)
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("Older", data))
	}

	pageLabel := fmt.Sprintf("%d/%d", page+1, totalPages)
	buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(pageLabel, "noop"))

	if page < totalPages-1 {
		data := formatHistCallback(page+1, windowID)
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("Newer", data))
	}

	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(buttons...),
	)
	return &kb
}

// formatHistCallback builds a callback data string for history pagination.
func formatHistCallback(page int, windowID string) string {
	data := fmt.Sprintf("hist_%d:%s", page, windowID)
	if len(data) > 64 {
		data = data[:64]
	}
	return data
}

// parseHistCallbackData parses history callback data "hist_<page>:<windowID>".
func parseHistCallbackData(data string) (page int, windowID string, ok bool) {
	if !strings.HasPrefix(data, "hist_") {
		return 0, "", false
	}
	rest := data[5:]
	colonIdx := strings.Index(rest, ":")
	if colonIdx < 0 {
		return 0, "", false
	}
	p, err := strconv.Atoi(rest[:colonIdx])
	if err != nil {
		return 0, "", false
	}
	return p, rest[colonIdx+1:], true
}

