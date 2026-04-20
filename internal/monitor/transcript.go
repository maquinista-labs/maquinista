package monitor

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// UsageData holds token counts extracted from an assistant message.
// Populated for claude / openclaude runner JSONL entries only.
type UsageData struct {
	Model        string
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	Timestamp    time.Time
}

// Entry represents a parsed JSONL transcript entry.
type Entry struct {
	Type      string         // "user", "assistant", "summary"
	Blocks    []ContentBlock // parsed content blocks
	RawData   json.RawMessage
	Usage     *UsageData     // non-nil for assistant entries that carry usage
	Timestamp time.Time      // from the JSONL "timestamp" field; zero if absent
}

// ContentBlock represents a single content block within an entry.
type ContentBlock struct {
	Type      string // "text", "tool_use", "tool_result", "thinking"
	Text      string // for text/thinking blocks
	ToolName  string // for tool_use
	ToolInput string // for tool_use (summary of input)
	ToolUseID string // for tool_use and tool_result
	Content   string // for tool_result
	IsError   bool   // for tool_result
}

// PendingTool tracks a tool_use block awaiting its tool_result.
type PendingTool struct {
	ToolUseID string
	ToolName  string
	Input     string
	Summary   string
}

// Regex patterns for filtering system content from user text.
var reSystemTags = regexp.MustCompile(`<(?:bash-input|bash-stdout|bash-stderr|local-command-caveat|system-reminder)[^>]*>[\s\S]*?</(?:bash-input|bash-stdout|bash-stderr|local-command-caveat|system-reminder)>`)
var reCommandName = regexp.MustCompile(`<command-name>(.*?)</command-name>`)

// ParseLine parses a single JSONL line into an Entry.
// Returns nil for unrecognized or ignorable entries.
func ParseLine(line []byte) (*Entry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}

	typeBytes, ok := raw["type"]
	if !ok {
		return nil, nil
	}

	var entryType string
	if err := json.Unmarshal(typeBytes, &entryType); err != nil {
		return nil, nil
	}

	switch entryType {
	case "user", "assistant":
		return parseMessageEntry(entryType, raw)
	case "summary":
		return parseSummaryEntry(raw)
	default:
		return nil, nil
	}
}

func parseMessageEntry(entryType string, raw map[string]json.RawMessage) (*Entry, error) {
	msgBytes, ok := raw["message"]
	if !ok {
		return &Entry{Type: entryType}, nil
	}

	var msg struct {
		Content json.RawMessage `json:"content"`
		Model   string          `json:"model"`
		Usage   *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		return &Entry{Type: entryType}, nil
	}

	blocks := parseContentBlocks(msg.Content)

	var usage *UsageData
	if entryType == "assistant" && msg.Usage != nil && msg.Model != "" {
		ts := time.Now()
		if tsBytes, ok := raw["timestamp"]; ok {
			var tsStr string
			if err := json.Unmarshal(tsBytes, &tsStr); err == nil {
				if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
					ts = t
				}
			}
		}
		usage = &UsageData{
			Model:        msg.Model,
			InputTokens:  msg.Usage.InputTokens,
			OutputTokens: msg.Usage.OutputTokens,
			CacheRead:    msg.Usage.CacheReadInputTokens,
			CacheWrite:   msg.Usage.CacheCreationInputTokens,
			Timestamp:    ts,
		}
	}

	// Parse entry-level timestamp for staleness detection in the monitor.
	var entryTS time.Time
	if tsBytes, ok := raw["timestamp"]; ok {
		var tsStr string
		if err := json.Unmarshal(tsBytes, &tsStr); err == nil {
			if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
				entryTS = t
			}
		}
	}

	rawData, _ := json.Marshal(raw)
	return &Entry{
		Type:      entryType,
		Blocks:    blocks,
		RawData:   rawData,
		Usage:     usage,
		Timestamp: entryTS,
	}, nil
}

func parseSummaryEntry(raw map[string]json.RawMessage) (*Entry, error) {
	rawData, _ := json.Marshal(raw)
	return &Entry{
		Type:    "summary",
		RawData: rawData,
	}, nil
}

// parseContentBlocks parses the content array from a message.
func parseContentBlocks(contentJSON json.RawMessage) []ContentBlock {
	if contentJSON == nil {
		return nil
	}

	// Try as string first (simple text message)
	var textContent string
	if err := json.Unmarshal(contentJSON, &textContent); err == nil {
		if textContent != "" {
			return []ContentBlock{{Type: "text", Text: textContent}}
		}
		return nil
	}

	// Parse as array of content blocks
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentJSON, &blocks); err != nil {
		return nil
	}

	var result []ContentBlock
	for _, blockJSON := range blocks {
		var blockType struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(blockJSON, &blockType); err != nil {
			continue
		}

		switch blockType.Type {
		case "text":
			result = append(result, parseTextBlock(blockJSON))
		case "tool_use":
			result = append(result, parseToolUseBlock(blockJSON))
		case "tool_result":
			result = append(result, parseToolResultBlock(blockJSON))
		case "thinking":
			result = append(result, parseThinkingBlock(blockJSON))
		}
	}
	return result
}

func parseTextBlock(data json.RawMessage) ContentBlock {
	var block struct {
		Text string `json:"text"`
	}
	json.Unmarshal(data, &block)
	return ContentBlock{
		Type: "text",
		Text: block.Text,
	}
}

func parseToolUseBlock(data json.RawMessage) ContentBlock {
	var block struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	json.Unmarshal(data, &block)

	input := ExtractToolInput(block.Name, block.Input)

	return ContentBlock{
		Type:      "tool_use",
		ToolName:  block.Name,
		ToolInput: input,
		ToolUseID: block.ID,
	}
}

func parseToolResultBlock(data json.RawMessage) ContentBlock {
	var block struct {
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	json.Unmarshal(data, &block)

	content := extractToolResultText(block.Content)

	return ContentBlock{
		Type:      "tool_result",
		ToolUseID: block.ToolUseID,
		Content:   content,
		IsError:   block.IsError,
	}
}

func parseThinkingBlock(data json.RawMessage) ContentBlock {
	var block struct {
		Thinking string `json:"thinking"`
	}
	json.Unmarshal(data, &block)
	return ContentBlock{
		Type: "thinking",
		Text: block.Thinking,
	}
}

// extractToolResultText extracts text from tool_result content.
func extractToolResultText(contentJSON json.RawMessage) string {
	if contentJSON == nil {
		return ""
	}

	// Try as string
	var text string
	if err := json.Unmarshal(contentJSON, &text); err == nil {
		return text
	}

	// Try as array of content blocks
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentJSON, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, blockJSON := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(blockJSON, &block); err != nil {
			continue
		}
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ParseEntries processes a list of entries with tool pairing.
// pending is the carry-over map from previous poll cycles.
// When tool_use and tool_result appear in the same batch, the tool_use is
// suppressed and only the combined tool_result is emitted (saves an API call).
func ParseEntries(entries []*Entry, pending map[string]PendingTool) []ParsedEntry {
	var result []ParsedEntry
	// Track tool_use entries added in this batch so we can suppress them
	// if their tool_result also arrives in the same batch.
	batchToolUseIdx := make(map[string]int) // toolUseID → index in result

	for _, entry := range entries {
		if entry == nil {
			continue
		}

		// Emit a synthetic usage entry so monitor.poll() can call CaptureTurn.
		// Kept out of the block loop so it's always emitted exactly once per
		// assistant message, regardless of content block count.
		if entry.Usage != nil {
			result = append(result, ParsedEntry{
				Role:        "assistant",
				ContentType: "usage",
				Usage:       entry.Usage,
			})
		}

		for _, block := range entry.Blocks {
			switch block.Type {
			case "text":
				text := cleanText(block.Text)
				if text != "" {
					result = append(result, ParsedEntry{
						Role:        entry.Type,
						ContentType: "text",
						Text:        text,
						Timestamp:   entry.Timestamp,
					})
				}

			case "tool_use":
				summary := FormatToolUseSummary(block.ToolName, block.ToolInput)
				pending[block.ToolUseID] = PendingTool{
					ToolUseID: block.ToolUseID,
					ToolName:  block.ToolName,
					Input:     block.ToolInput,
					Summary:   summary,
				}
				idx := len(result)
				result = append(result, ParsedEntry{
					Role:        "assistant",
					ContentType: "tool_use",
					Text:        summary,
					ToolUseID:   block.ToolUseID,
					ToolName:    block.ToolName,
					Timestamp:   entry.Timestamp,
				})
				batchToolUseIdx[block.ToolUseID] = idx

			case "tool_result":
				pe := ParsedEntry{
					Role:        "user",
					ContentType: "tool_result",
					ToolUseID:   block.ToolUseID,
					Timestamp:   entry.Timestamp,
				}

				if pt, ok := pending[block.ToolUseID]; ok {
					pe.ToolName = pt.ToolName
					pe.ToolInput = pt.Input
					pe.Text = block.Content
					delete(pending, block.ToolUseID)
				} else {
					// No matching tool_use (e.g. after restart) — send with generic label
					pe.ToolName = "unknown"
					pe.Text = block.Content
				}
				pe.IsError = block.IsError

				// If tool_use was in same batch, suppress it — the combined
				// tool_result message will contain all the info.
				if idx, ok := batchToolUseIdx[block.ToolUseID]; ok {
					result[idx].ContentType = "" // mark for removal
					delete(batchToolUseIdx, block.ToolUseID)
				}

				result = append(result, pe)

			case "thinking":
				if block.Text != "" {
					result = append(result, ParsedEntry{
						Role:        "assistant",
						ContentType: "thinking",
						Text:        block.Text,
						Timestamp:   entry.Timestamp,
					})
				}
			}
		}
	}

	// Remove suppressed entries (same-batch tool_use that got paired)
	filtered := result[:0]
	for _, r := range result {
		if r.ContentType != "" {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// ParsedEntry is a display-ready parsed entry for the message queue.
// ContentType "usage" is internal — monitor.poll() consumes it for cost
// capture and strips it before Telegram / outbox routing.
type ParsedEntry struct {
	Role        string     // "user", "assistant"
	ContentType string     // "text", "tool_use", "tool_result", "thinking", "usage"
	Text        string
	ToolUseID   string
	ToolName    string
	ToolInput   string     // tool input summary (for tool_result combined display)
	IsError     bool
	Usage       *UsageData // set when ContentType == "usage"
	Timestamp   time.Time  // from the JSONL entry; zero if unavailable
}

// FormatToolUseSummary formats a tool_use into a summary line.
func FormatToolUseSummary(name, input string) string {
	if input != "" {
		return "**" + name + "**(" + input + ")"
	}
	return "**" + name + "**()"
}

// cleanText strips system tags from text content.
func cleanText(text string) string {
	cleaned := reSystemTags.ReplaceAllString(text, "")
	return strings.TrimSpace(cleaned)
}

// jsonString extracts a string value from a JSON raw message.
func jsonString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
