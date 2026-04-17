package memory

import (
	"context"
	"regexp"
	"strings"
)

// AutoFlush detects "remember that I…" / "please remember …" patterns
// in an inbound message (user → agent) and upserts an archival passage
// in the `user` dimension. Runs best-effort: a scan failure is logged
// by the caller but doesn't block the turn.
//
// Covers Phase 4 of plans/active/agent-memory-db.md. Heuristic, not
// exhaustive — the agent is still free to call the explicit memory
// tool for anything it wants curated deliberately.
//
// Returns the id of the inserted memory row (0 when no match fires).

// autoFlushPatterns are case-insensitive "teach me something about
// yourself" heuristics. Capture group #1 is the fact body.
var autoFlushPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:please\s+)?remember\s+(?:that\s+)?(.{3,}?)\s*$`),
	regexp.MustCompile(`(?i)take\s+(?:this|a)\s+note[:\-]?\s*(.{3,}?)\s*$`),
	regexp.MustCompile(`(?i)i\s+prefer\s+(.{3,}?)\s*$`),
	regexp.MustCompile(`(?i)don'?t\s+forget\s+(?:that\s+)?(.{3,}?)\s*$`),
}

// AutoFlush scans `text` (one user message's raw content) and, on the
// first matching pattern, writes an agent_memories row with
// dimension='user', tier='long_term', category='preference',
// source='auto_flush'. Returns (memoryID, extractedFact, matched).
// extractedFact is the cleaned body ready to be surfaced back to the
// user so they can see what got remembered.
func AutoFlush(ctx context.Context, q Querier, agentID, text string) (int64, string, bool) {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return 0, "", false
	}
	// Only scan the first sentence-ish window so very long transcripts
	// don't fire on incidental "remember …" substrings.
	head := cleaned
	if len(head) > 400 {
		head = head[:400]
	}

	for _, rx := range autoFlushPatterns {
		m := rx.FindStringSubmatch(head)
		if m == nil {
			continue
		}
		fact := strings.TrimSpace(m[1])
		fact = strings.TrimRight(fact, ".!?")
		if fact == "" {
			continue
		}
		title := summarizeTitle(fact)
		id, err := Remember(ctx, q, Memory{
			AgentID:   agentID,
			Dimension: "user",
			Tier:      "long_term",
			Category:  "preference",
			Title:     title,
			Body:      fact,
			Source:    "auto_flush",
		})
		if err != nil {
			return 0, "", false
		}
		return id, fact, true
	}
	return 0, "", false
}

// summarizeTitle makes a ≤120-char title from a fact body. The archival
// schema checks title length at validation time.
func summarizeTitle(body string) string {
	body = strings.TrimSpace(body)
	// First 80 chars or up to a natural break.
	limit := 80
	if len(body) < limit {
		limit = len(body)
	}
	if i := strings.IndexAny(body[:limit], ".\n"); i > 10 {
		limit = i
	}
	title := strings.TrimSpace(body[:limit])
	if len(title) > 120 {
		title = title[:120]
	}
	return title
}
