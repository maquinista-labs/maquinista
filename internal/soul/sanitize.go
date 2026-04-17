package soul

import (
	"regexp"
	"strings"
	"unicode"
)

// Finding is one hit from ScanForInjection — a potentially adversarial
// pattern in a soul import / edit / template. Severity drives CLI
// behavior: warn → require --force, block → abort.
type Finding struct {
	Pattern  string
	Offset   int
	Severity string // "warn" | "block"
	Excerpt  string // ~60 chars around the match
}

// Severity constants (strings so cobra flags can compare cleanly).
const (
	SeverityWarn  = "warn"
	SeverityBlock = "block"
)

// overrideRegexps are classic prompt-injection phrasings. Case-insensitive.
// Source: hermes-agent prompt_builder.py:36-73 + local survey.
var overrideRegexps = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+(?:previous|prior|above)\s+(?:instructions?|messages?|prompts?)`),
	regexp.MustCompile(`(?i)disregard\s+(?:all|any)\s+(?:previous|prior|above)`),
	regexp.MustCompile(`(?i)(?:system\s+prompt|your\s+instructions)\s+(?:override|replacement|reset)`),
	regexp.MustCompile(`(?i)you\s+are\s+now\s+[A-Z][^.\n]{0,80}`),
	regexp.MustCompile(`(?i)forget\s+(?:everything|all|prior)`),
	regexp.MustCompile(`(?i)from\s+now\s+on[, ]\s*you\s+(?:will|must|should)`),
}

// invisibleRunes that almost never belong in legitimate prompts but do
// wind up in adversarial content (bidi overrides, zero-width joiners).
var invisibleRunes = map[rune]string{
	'\u202e': "right-to-left override",
	'\u202d': "left-to-right override",
	'\u200b': "zero-width space",
	'\u200c': "zero-width non-joiner",
	'\u200d': "zero-width joiner",
	'\u2060': "word joiner",
	'\ufeff': "zero-width no-break space (BOM)",
}

// exfilRegexp captures shell commands that might be exfiltration
// attempts. Go's regex dialect doesn't support lookaheads, so the host
// filter (localhost / 127.*) runs in Go in postfilter().
var exfilRegexp = regexp.MustCompile(`(?i)\b(?:curl|wget)\s+(?:-[a-zA-Z]+\s+)*https?://([^\s"/]+)`)

// isExfilHost returns true when the captured host is NOT a loopback
// address. Loopback curls are legitimate health checks and should not
// be flagged.
func isExfilHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.Index(host, ":"); i >= 0 { // strip port
		host = host[:i]
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return false
	}
	if strings.HasPrefix(host, "127.") {
		return false
	}
	if host == "::1" || host == "0.0.0.0" {
		return false
	}
	return true
}

// ScanForInjection inspects body for adversarial patterns. Returns an
// empty slice when clean. Severity:
//
//   - block: invisible bidi / whitespace flood — always reject.
//   - warn:  known prompt-override phrasings, curl-to-external exfil —
//     require --force to import.
//
// Ordering of findings is offset-ascending.
func ScanForInjection(body string) []Finding {
	var out []Finding

	for _, rx := range overrideRegexps {
		for _, m := range rx.FindAllStringIndex(body, -1) {
			out = append(out, Finding{
				Pattern:  rx.String(),
				Offset:   m[0],
				Severity: SeverityWarn,
				Excerpt:  excerpt(body, m[0], m[1]),
			})
		}
	}

	for i, r := range body {
		if label, bad := invisibleRunes[r]; bad {
			out = append(out, Finding{
				Pattern:  label,
				Offset:   i,
				Severity: SeverityBlock,
				Excerpt:  excerpt(body, i, i+1),
			})
		}
	}

	for _, m := range exfilRegexp.FindAllStringSubmatchIndex(body, -1) {
		host := body[m[2]:m[3]]
		if !isExfilHost(host) {
			continue
		}
		out = append(out, Finding{
			Pattern:  "curl/wget to external host",
			Offset:   m[0],
			Severity: SeverityWarn,
			Excerpt:  excerpt(body, m[0], m[1]),
		})
	}

	if n := whitespaceRunLen(body); n > 10_000 {
		out = append(out, Finding{
			Pattern:  "whitespace flood",
			Offset:   0,
			Severity: SeverityBlock,
			Excerpt:  "(suppressed — " + itoa(n) + " whitespace chars)",
		})
	}

	return out
}

func excerpt(s string, start, end int) string {
	const pad = 30
	a := start - pad
	if a < 0 {
		a = 0
	}
	b := end + pad
	if b > len(s) {
		b = len(s)
	}
	snippet := strings.ReplaceAll(s[a:b], "\n", " ")
	return strings.TrimSpace(snippet)
}

func whitespaceRunLen(s string) int {
	best, run := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 0
		}
	}
	return best
}

// HasBlockingFindings is a shortcut for the CLI: true if any finding
// has severity=block.
func HasBlockingFindings(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}

// itoa avoids the strconv import for this single call site.
func itoa(n int) string {
	return strings.TrimLeft(strings.Map(func(r rune) rune { return r }, strings.Repeat("0", 0))+fmtInt(n), "")
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
