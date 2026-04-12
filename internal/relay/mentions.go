package relay

import (
	"strings"
	"unicode"
)

// Mention is one parsed `[@agent_id: text]` reference.
type Mention struct {
	AgentID string
	Text    string
}

// ParseMentions extracts `[@agent_id: text]` references from s. The text
// portion may contain nested square brackets as long as they balance. An
// opening `[@` preceded by an odd number of backslashes is treated as
// escaped and skipped. Malformed tokens (missing `:`, empty agent_id,
// unterminated text) are silently discarded.
func ParseMentions(s string) []Mention {
	var out []Mention
	i := 0
	for i < len(s) {
		// Find next "[@".
		j := strings.Index(s[i:], "[@")
		if j < 0 {
			break
		}
		start := i + j
		// Escape check: count preceding backslashes.
		k := start - 1
		bs := 0
		for k >= 0 && s[k] == '\\' {
			bs++
			k--
		}
		if bs%2 == 1 {
			i = start + 2
			continue
		}
		// Parse agent_id: [A-Za-z0-9_-]+
		p := start + 2
		idStart := p
		for p < len(s) {
			c := s[p]
			if c == '-' || c == '_' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) {
				p++
				continue
			}
			break
		}
		if p == idStart {
			i = start + 2
			continue
		}
		agentID := s[idStart:p]
		if p >= len(s) || s[p] != ':' {
			i = start + 2
			continue
		}
		p++ // skip ':'
		// Skip a single leading space for readability.
		if p < len(s) && s[p] == ' ' {
			p++
		}
		// Collect text up to matching ']', tracking nested brackets.
		textStart := p
		depth := 1
		for p < len(s) && depth > 0 {
			switch s[p] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					continue
				}
			}
			if depth > 0 {
				p++
			}
		}
		if depth != 0 {
			// unterminated
			break
		}
		text := s[textStart:p]
		out = append(out, Mention{AgentID: agentID, Text: strings.TrimRight(text, " ")})
		i = p + 1
	}
	return out
}
