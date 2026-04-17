package monitor

import (
	"strings"
	"unicode/utf8"
)

// MonitorProfile holds runner-specific TUI parsing parameters. Each
// AgentRunner returns its own profile; the shared StripPaneChrome /
// ExtractStatusLine / IsInteractiveUI helpers can be called with a
// profile (the *For variants) or without (uses the Claude default for
// backward compatibility). See plans/active/opencode-integration.md OC-01.
type MonitorProfile struct {
	// SpinnerChars are the unicode runes that can appear at the start of
	// a status line to indicate the agent is working.
	SpinnerChars string
	// SeparatorRunes are the rune set that makes up a chrome separator
	// (Claude draws ─/━ rows; OpenCode uses a build-status bar and needs
	// no separator match at all — leave empty for "never match").
	SeparatorRunes []rune
	// MinSeparatorLen is the minimum rune count that counts as a separator.
	MinSeparatorLen int
	// UIPatterns are the interactive-prompt markers ExtractInteractiveContent
	// scans for. nil = the runner has no interactive UI patterns yet.
	UIPatterns []UIPattern
}

// ClaudeProfile returns the Claude Code TUI parsing parameters that have
// been hardcoded into this package since day one. Kept as the default so
// existing callers (and tests) keep working when they don't pass an
// explicit profile.
func ClaudeProfile() MonitorProfile {
	return MonitorProfile{
		SpinnerChars:    spinnerChars,
		SeparatorRunes:  []rune{'─', '━'},
		MinSeparatorLen: 20,
		UIPatterns:      uiPatterns,
	}
}

// OpenCodeProfile returns the OpenCode TUI parsing parameters, derived
// from live observation in OpenCode v1.3.14 (OC-06 in
// plans/active/opencode-integration.md).
//
// Observed layout (captured with `opencode --model opencode/big-pickle`):
//
//	┃  Ask anything... "Fix a TODO in the codebase"
//	┃
//	┃  Build  Big Pickle OpenCode Zen
//	╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
//	                              tab agents  ctrl+p commands
//
// There's no spinner character in the input area; busy state is signaled
// in the bottom "Build" status bar ("Build 12s"). The chrome line above
// the hints is made of '▀' (Unicode upper half block) with a leading
// '╹'. No permission-prompt / plan-mode UIs observed in non-interactive
// probing; patterns stay empty until an operator captures one.
func OpenCodeProfile() MonitorProfile {
	return MonitorProfile{
		SpinnerChars:    "", // no input-area spinner; status lives in the "Build" bar
		SeparatorRunes:  []rune{'▀', '╹'},
		MinSeparatorLen: 30,
		UIPatterns:      nil, // no interactive prompts known yet
	}
}

// Spinner characters used by Claude Code's status line.
const spinnerChars = "·✻✽✶✳✢"

// StripPaneChrome removes Claude Code's bottom chrome (separator, prompt,
// status bar) from captured pane text. Returns the text above the
// separator. Uses ClaudeProfile; for other runners call StripPaneChromeFor.
func StripPaneChrome(paneText string) string {
	return StripPaneChromeFor(paneText, ClaudeProfile())
}

// StripPaneChromeFor is the profile-aware variant. When profile.SeparatorRunes
// is empty the function returns the input unchanged (runners without a
// chrome separator have no "above the separator" to extract).
func StripPaneChromeFor(paneText string, profile MonitorProfile) string {
	if len(profile.SeparatorRunes) == 0 {
		return paneText
	}
	lines := strings.Split(paneText, "\n")
	sepIdx := findChromeSeparatorFor(lines, profile)
	if sepIdx < 0 {
		return paneText
	}
	return strings.Join(lines[:sepIdx], "\n")
}

// ExtractStatusLine detects Claude's spinner/status from the terminal
// output. Returns the status text and whether a status was found. Uses
// ClaudeProfile; for other runners call ExtractStatusLineFor.
func ExtractStatusLine(paneText string) (string, bool) {
	return ExtractStatusLineFor(paneText, ClaudeProfile())
}

// ExtractStatusLineFor is the profile-aware variant. It looks for the
// profile's separator, then searches a few lines above for one whose first
// rune is in the profile's SpinnerChars. Returns ("", false) when the
// profile has no separator runes or no spinner chars.
func ExtractStatusLineFor(paneText string, profile MonitorProfile) (string, bool) {
	if len(profile.SeparatorRunes) == 0 || profile.SpinnerChars == "" {
		return "", false
	}
	lines := strings.Split(paneText, "\n")
	sepIdx := findChromeSeparatorFor(lines, profile)
	if sepIdx < 0 {
		return "", false
	}

	// Check lines above separator (skip blanks, up to 5 lines above)
	minIdx := sepIdx - 5
	if minIdx < 0 {
		minIdx = -1
	}
	for i := sepIdx - 1; i > minIdx; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		r, size := utf8.DecodeRuneInString(line)
		if strings.ContainsRune(profile.SpinnerChars, r) {
			return strings.TrimSpace(line[size:]), true
		}
		// First non-empty non-spinner line → no status
		return "", false
	}
	return "", false
}

// findChromeSeparator finds the line index of the topmost chrome separator
// (a line of ─ chars) in the last 10 lines. Uses ClaudeProfile.
func findChromeSeparator(lines []string) int {
	return findChromeSeparatorFor(lines, ClaudeProfile())
}

func findChromeSeparatorFor(lines []string, profile MonitorProfile) int {
	if len(profile.SeparatorRunes) == 0 {
		return -1
	}
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	for i := start; i < len(lines); i++ {
		if isChromeSeparatorFor(lines[i], profile) {
			return i
		}
	}
	return -1
}

// isChromeSeparator checks if a line is a chrome separator under the Claude profile.
func isChromeSeparator(line string) bool {
	return isChromeSeparatorFor(line, ClaudeProfile())
}

func isChromeSeparatorFor(line string, profile MonitorProfile) bool {
	if len(profile.SeparatorRunes) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if utf8.RuneCountInString(trimmed) < profile.MinSeparatorLen {
		return false
	}
	for _, r := range trimmed {
		ok := false
		for _, sr := range profile.SeparatorRunes {
			if r == sr {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// extractAfterSpinner extracts the text after the first spinner character.
func extractAfterSpinner(line string) string {
	for i, r := range line {
		if strings.ContainsRune(spinnerChars, r) {
			rest := strings.TrimSpace(line[i+utf8.RuneLen(r):])
			return rest
		}
	}
	return ""
}

// UIPattern defines markers for detecting interactive UI elements.
type UIPattern struct {
	Name       string
	TopMarkers []string
	BotMarkers []string // empty = use last non-empty line
}

// UIContent holds extracted interactive content.
type UIContent struct {
	Name    string
	Content string
}

var uiPatterns = []UIPattern{
	{
		Name:       "ExitPlanMode",
		TopMarkers: []string{"Would you like to proceed?", "Claude has written up a plan"},
		BotMarkers: []string{"ctrl-g to edit", "Esc to"},
	},
	{
		Name:       "AskUserQuestion_multi",
		TopMarkers: []string{"← "},
		BotMarkers: nil, // last non-empty line
	},
	{
		Name:       "AskUserQuestion_single",
		TopMarkers: []string{"☐", "✔", "☒"},
		BotMarkers: []string{"Enter to select"},
	},
	{
		Name:       "PermissionPrompt",
		TopMarkers: []string{"Do you want to proceed?"},
		BotMarkers: []string{"Esc to cancel"},
	},
	{
		Name:       "RestoreCheckpoint",
		TopMarkers: []string{"Restore the code"},
		BotMarkers: []string{"Enter to continue"},
	},
	{
		Name:       "Settings",
		TopMarkers: []string{"Settings:"},
		BotMarkers: []string{"Esc to cancel", "Type to filter"},
	},
}

// IsInteractiveUI returns true if the pane text contains an interactive UI
// prompt under the Claude profile.
func IsInteractiveUI(paneText string) bool {
	return IsInteractiveUIFor(paneText, ClaudeProfile())
}

func IsInteractiveUIFor(paneText string, profile MonitorProfile) bool {
	_, ok := ExtractInteractiveContentFor(paneText, profile)
	return ok
}

// ExtractInteractiveContent extracts the interactive UI content from pane
// text under the Claude profile. Returns the UI content and true if found.
func ExtractInteractiveContent(paneText string) (UIContent, bool) {
	return ExtractInteractiveContentFor(paneText, ClaudeProfile())
}

func ExtractInteractiveContentFor(paneText string, profile MonitorProfile) (UIContent, bool) {
	if len(profile.UIPatterns) == 0 {
		return UIContent{}, false
	}
	stripped := StripPaneChromeFor(paneText, profile)
	lines := strings.Split(stripped, "\n")

	for _, pattern := range profile.UIPatterns {
		content, ok := tryExtract(lines, pattern)
		if ok {
			return content, true
		}
	}
	return UIContent{}, false
}

func tryExtract(lines []string, pattern UIPattern) (UIContent, bool) {
	// Find top marker
	topIdx := -1
	for i, line := range lines {
		for _, marker := range pattern.TopMarkers {
			if strings.Contains(line, marker) {
				topIdx = i
				break
			}
		}
		if topIdx >= 0 {
			break
		}
	}

	if topIdx < 0 {
		return UIContent{}, false
	}

	// Find bottom marker
	botIdx := -1
	if len(pattern.BotMarkers) == 0 {
		// Use last non-empty line
		for i := len(lines) - 1; i > topIdx; i-- {
			if strings.TrimSpace(lines[i]) != "" {
				botIdx = i
				break
			}
		}
	} else {
		for i := topIdx + 1; i < len(lines); i++ {
			for _, marker := range pattern.BotMarkers {
				if strings.Contains(lines[i], marker) {
					botIdx = i
					break
				}
			}
			if botIdx >= 0 {
				break
			}
		}
	}

	if botIdx < 0 {
		return UIContent{}, false
	}

	// Extract content between markers
	content := strings.Join(lines[topIdx:botIdx+1], "\n")
	return UIContent{
		Name:    pattern.Name,
		Content: content,
	}, true
}

// ExtractBashOutput extracts ! command output from a captured tmux pane.
// Searches from the bottom for the "! <command>" echo line, then returns
// that line and everything below it. Returns empty string if not found.
func ExtractBashOutput(paneText, command string) string {
	stripped := StripPaneChrome(paneText)
	lines := strings.Split(stripped, "\n")

	// Match on the first 10 chars of the command to handle terminal truncation
	matchPrefix := command
	if len(matchPrefix) > 10 {
		matchPrefix = matchPrefix[:10]
	}

	// Search from bottom for the "! <command>" echo line
	cmdIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "! "+matchPrefix) || strings.HasPrefix(trimmed, "!"+matchPrefix) {
			cmdIdx = i
			break
		}
	}

	if cmdIdx < 0 {
		return ""
	}

	// Include the command echo line and everything after
	output := lines[cmdIdx:]

	// Strip trailing empty lines
	for len(output) > 0 && strings.TrimSpace(output[len(output)-1]) == "" {
		output = output[:len(output)-1]
	}

	if len(output) == 0 {
		return ""
	}

	return strings.Join(output, "\n")
}

// ShortenSeparators replaces long ─ lines with a shorter version for display.
func ShortenSeparators(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if isChromeSeparator(line) {
			lines[i] = "─────"
		}
	}
	return strings.Join(lines, "\n")
}
