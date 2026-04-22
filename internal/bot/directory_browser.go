package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

const dirsPerPage = 6

// BrowseState holds per-user directory browser state.
type BrowseState struct {
	CurrentPath string
	Page        int
	Dirs        []string // cached subdirectory names for index-based callbacks
	PendingText string
	MessageID   int
	ChatID      int64
	ThreadID    int
}

// showDirectoryBrowser sends the directory browser keyboard to the user.
func (b *Bot) showDirectoryBrowser(chatID int64, threadID int, userID int64, pendingText string) {
	home, _ := os.UserHomeDir()
	startPath := home

	text, keyboard, dirs := buildDirectoryBrowser(startPath, 0)

	msg, err := b.sendMessageWithKeyboard(chatID, threadID, text, keyboard)
	if err != nil {
		log.Printf("Error sending directory browser: %v", err)
		return
	}

	b.mu.Lock()
	b.browseStates[userID] = &BrowseState{
		CurrentPath: startPath,
		Page:        0,
		Dirs:        dirs,
		PendingText: pendingText,
		MessageID:   msg.MessageID,
		ChatID:      chatID,
		ThreadID:    threadID,
	}
	b.mu.Unlock()
}

// buildDirectoryBrowser builds the inline keyboard for directory browsing.
// Returns the display text, keyboard markup, and cached subdirectory names.
func buildDirectoryBrowser(currentPath string, page int) (string, tgbotapi.InlineKeyboardMarkup, []string) {
	entries, err := os.ReadDir(currentPath)
	if err != nil {
		return fmt.Sprintf("Error reading %s", currentPath), tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Cancel", "dir_cancel"),
			),
		), nil
	}

	// Filter to non-hidden directories, sorted
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	totalPages := (len(dirs) + dirsPerPage - 1) / dirsPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	if page < 0 {
		page = 0
	}

	// Build keyboard rows
	var rows [][]tgbotapi.InlineKeyboardButton

	// Directory buttons (2 per row)
	start := page * dirsPerPage
	end := start + dirsPerPage
	if end > len(dirs) {
		end = len(dirs)
	}

	for i := start; i < end; i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(
			truncateName(dirs[i], 13),
			fmt.Sprintf("dir_sel:%d", i),
		))
		if i+1 < end {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(
				truncateName(dirs[i+1], 13),
				fmt.Sprintf("dir_sel:%d", i+1),
			))
		}
		rows = append(rows, row)
	}

	// Pagination row
	if totalPages > 1 {
		var paginationRow []tgbotapi.InlineKeyboardButton
		if page > 0 {
			paginationRow = append(paginationRow, tgbotapi.NewInlineKeyboardButtonData(
				"\u25C0", fmt.Sprintf("dir_page:%d", page-1),
			))
		}
		paginationRow = append(paginationRow, tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("%d/%d", page+1, totalPages),
			"dir_noop",
		))
		if page < totalPages-1 {
			paginationRow = append(paginationRow, tgbotapi.NewInlineKeyboardButtonData(
				"\u25B6", fmt.Sprintf("dir_page:%d", page+1),
			))
		}
		rows = append(rows, paginationRow)
	}

	// Action row: .. | Select | Cancel
	actionRow := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("..", "dir_up"),
		tgbotapi.NewInlineKeyboardButtonData("Select", "dir_confirm"),
		tgbotapi.NewInlineKeyboardButtonData("Cancel", "dir_cancel"),
	}
	rows = append(rows, actionRow)

	displayPath := shortenPath(currentPath)
	text := fmt.Sprintf("Select directory:\n%s", displayPath)

	return text, tgbotapi.NewInlineKeyboardMarkup(rows...), dirs
}

// processDirectoryCallback handles directory browser callback queries.
func (b *Bot) processDirectoryCallback(cq *tgbotapi.CallbackQuery) {
	userID := cq.From.ID
	data := cq.Data

	b.mu.RLock()
	bs, ok := b.browseStates[userID]
	b.mu.RUnlock()

	if !ok {
		return
	}

	// Verify topic match
	threadID := getThreadID(cq.Message)
	if threadID != bs.ThreadID {
		return
	}

	switch {
	case strings.HasPrefix(data, "dir_sel:"):
		b.handleDirSelect(cq, bs, userID)
	case strings.HasPrefix(data, "dir_page:"):
		b.handleDirPage(cq, bs, userID)
	case data == "dir_up":
		b.handleDirUp(cq, bs, userID)
	case data == "dir_confirm":
		b.handleDirConfirm(cq, bs, userID)
	case data == "dir_cancel":
		b.handleDirCancel(cq, bs, userID)
	case data == "dir_noop":
		// Do nothing — page indicator button
	}
}

func (b *Bot) handleDirSelect(cq *tgbotapi.CallbackQuery, bs *BrowseState, userID int64) {
	idxStr := strings.TrimPrefix(cq.Data, "dir_sel:")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(bs.Dirs) {
		return
	}

	newPath := filepath.Join(bs.CurrentPath, bs.Dirs[idx])
	info, err := os.Stat(newPath)
	if err != nil || !info.IsDir() {
		return
	}

	text, keyboard, dirs := buildDirectoryBrowser(newPath, 0)
	b.editMessageWithKeyboard(bs.ChatID, bs.MessageID, text, keyboard)

	b.mu.Lock()
	bs.CurrentPath = newPath
	bs.Page = 0
	bs.Dirs = dirs
	b.mu.Unlock()
}

func (b *Bot) handleDirPage(cq *tgbotapi.CallbackQuery, bs *BrowseState, userID int64) {
	pageStr := strings.TrimPrefix(cq.Data, "dir_page:")
	page, err := strconv.Atoi(pageStr)
	if err != nil {
		return
	}

	text, keyboard, dirs := buildDirectoryBrowser(bs.CurrentPath, page)
	b.editMessageWithKeyboard(bs.ChatID, bs.MessageID, text, keyboard)

	b.mu.Lock()
	bs.Page = page
	bs.Dirs = dirs
	b.mu.Unlock()
}

func (b *Bot) handleDirUp(cq *tgbotapi.CallbackQuery, bs *BrowseState, userID int64) {
	parent := filepath.Dir(bs.CurrentPath)
	if parent == bs.CurrentPath {
		return // already at root
	}

	text, keyboard, dirs := buildDirectoryBrowser(parent, 0)
	b.editMessageWithKeyboard(bs.ChatID, bs.MessageID, text, keyboard)

	b.mu.Lock()
	bs.CurrentPath = parent
	bs.Page = 0
	bs.Dirs = dirs
	b.mu.Unlock()
}

// createWindowResult holds the result of creating a new tmux window for a directory.
type createWindowResult struct {
	WindowID   string
	WindowName string
}

// createWindowForDir creates a new tmux window in the given directory, waits for the
// session_map entry, binds the thread, and renames the topic. Returns the result or error.
func (b *Bot) createWindowForDir(dir string, userID int64, chatID int64, threadID int) (*createWindowResult, error) {
	// Build Minuano environment if configured
	env := b.buildMinuanoEnv(filepath.Base(dir))

	// Build the agent command from the configured runner
	agentCmd := b.config.ClaudeCommand // fallback
	if r := b.DefaultRunner(); r != nil {
		agentCmd = r.LaunchCommand(runner.Config{})
	}

	// Create new tmux window
	windowID, err := tmux.NewWindow(b.config.TmuxSessionName, "", dir, agentCmd, env)
	if err != nil {
		return nil, fmt.Errorf("creating window: %w", err)
	}

	// Kill the placeholder _init window now that we have a real window
	tmux.CleanupInitWindow(b.config.TmuxSessionName)

	windowName := filepath.Base(dir)

	r := b.DefaultRunner()
	hasHook := r != nil && r.HasSessionHook()

	// Upsert the agents row first — that's the session-map equivalent now.
	// session_id stays NULL until the hook / monitor discovers it.
	pool := b.getPool()
	if pool != nil {
		upsertCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := pool.Exec(upsertCtx, `
			INSERT INTO agents
				(id, tmux_session, tmux_window, role, status, runner_type,
				 cwd, window_name, started_at, last_seen, stop_requested)
			VALUES ($1, $2, $3, 'user', 'running', $4, $5, $6, NOW(), NOW(), FALSE)
			ON CONFLICT (id) DO UPDATE SET
				tmux_session   = EXCLUDED.tmux_session,
				tmux_window    = EXCLUDED.tmux_window,
				status         = 'running',
				cwd            = EXCLUDED.cwd,
				window_name    = EXCLUDED.window_name,
				last_seen      = NOW(),
				stop_requested = FALSE
		`, windowID, b.config.TmuxSessionName, windowID, b.config.DefaultRunner, dir, windowName)
		cancel()
		if err != nil {
			log.Printf("directory_browser: agents upsert: %v", err)
		}
	}

	if hasHook {
		// Runner writes session_id via an external hook — poll the DB until
		// the agents row picks up the session_id.
		resolved := ""
		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			if pool == nil {
				break
			}
			lookupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			sm, err := state.LoadSessionMap(lookupCtx, pool)
			cancel()
			if err != nil {
				continue
			}
			for key, entry := range sm {
				if strings.HasSuffix(key, ":"+windowID) && entry.SessionID != "" {
					resolved = key
					b.state.SetWindowState(windowID, state.WindowState{
						SessionID:  entry.SessionID,
						CWD:        entry.CWD,
						WindowName: entry.WindowName,
					})
					b.state.SetWindowDisplayName(windowID, entry.WindowName)
					break
				}
			}
			if resolved != "" {
				break
			}
		}
		if resolved == "" {
			log.Printf("Hook did not populate agents.session_id for %s, trying fallback discovery...", windowID)
			if sid := discoverSessionID(dir); sid != "" && pool != nil {
				upCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, _ = pool.Exec(upCtx, `
					UPDATE agents SET session_id=$1, last_seen=NOW()
					WHERE id=$2
				`, sid, windowID)
				cancel()
				log.Printf("Fallback: discovered session %s for window %s", sid, windowID)
			} else {
				log.Printf("Warning: could not discover session for window %s", windowID)
			}
		}

		tmux.WaitForReady(b.config.TmuxSessionName, windowID, 15*time.Second)
	} else {
		// Hookless runner (OpenCode) — the agents row is already populated
		// with cwd + window_name; the TranscriptSource discovers
		// session_id from its own DB and UPDATEs agents.session_id.
		b.state.SetWindowState(windowID, state.WindowState{
			CWD:        dir,
			WindowName: windowName,
		})
		b.state.SetWindowDisplayName(windowID, windowName)

		time.Sleep(3 * time.Second)
	}

	// Bind thread to window
	userIDStr := strconv.FormatInt(userID, 10)
	threadIDStr := strconv.Itoa(threadID)
	b.state.BindThread(userIDStr, threadIDStr, windowID)
	// Track which runner owns this window for transcript source routing
	if r := b.DefaultRunner(); r != nil {
		b.state.SetWindowRunner(windowID, r.Name())
	} else {
		b.state.SetWindowRunner(windowID, "claude")
	}
	b.saveState()

	// Use topic name as display name (fall back to directory base name)
	displayName := getTopicName(threadID)
	if displayName == "" {
		displayName = filepath.Base(dir)
		if dn, ok := b.state.GetWindowDisplayName(windowID); ok {
			displayName = dn
		}
	}

	return &createWindowResult{WindowID: windowID, WindowName: displayName}, nil
}

func (b *Bot) handleDirConfirm(cq *tgbotapi.CallbackQuery, bs *BrowseState, userID int64) {
	selectedPath := bs.CurrentPath
	pendingText := bs.PendingText
	chatID := bs.ChatID
	threadID := bs.ThreadID

	// Clear browse state
	b.mu.Lock()
	delete(b.browseStates, userID)
	b.mu.Unlock()

	// Edit message to show progress
	b.editMessageText(chatID, bs.MessageID, fmt.Sprintf("Creating session in %s...", shortenPath(selectedPath)))

	result, err := b.createWindowForDir(selectedPath, userID, chatID, threadID)
	if err != nil {
		log.Printf("Error creating window: %v", err)
		b.editMessageText(chatID, bs.MessageID, "Error: failed to create session.")
		return
	}

	// Update the browser message
	b.editMessageText(chatID, bs.MessageID, fmt.Sprintf("Bound to: %s", result.WindowName))

	// Send pending text through the inbox so the per-agent sidecar delivers it
	// with the same guarantees as a normal user turn. Fall back to direct pty
	// write when no DB pool is available.
	if pendingText != "" {
		extMsgID := fmt.Sprintf("dirbrowse:%d:%d", bs.ChatID, bs.MessageID)
		if !b.enqueueInboxText(result.WindowID, bs.ChatID, userID, bs.ThreadID, extMsgID, pendingText) {
			if err := tmux.SendKeysWithDelay(b.config.TmuxSessionName, result.WindowID, pendingText, 500); err != nil {
				log.Printf("Error sending pending text: %v", err)
			}
		}
	}
}

func (b *Bot) handleDirCancel(cq *tgbotapi.CallbackQuery, bs *BrowseState, userID int64) {
	b.mu.Lock()
	delete(b.browseStates, userID)
	b.mu.Unlock()

	b.editMessageText(bs.ChatID, bs.MessageID, "Cancelled.")
}

// renameForumTopic renames a Telegram forum topic.
func (b *Bot) renameForumTopic(chatID int64, threadID int, name string) {
	if threadID == 0 {
		return
	}
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_thread_id", threadID)
	params.AddNonEmpty("name", name)
	if _, err := b.api.MakeRequest("editForumTopic", params); err != nil {
		log.Printf("Error renaming topic: %v", err)
	}
}

// discoverSessionID finds the most recent Claude Code session for a directory
// by scanning the sessions-index.json in the corresponding project directory.
func discoverSessionID(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	// Convert cwd to Claude's project dir name: /home/otavio/code → -home-otavio-code
	projectDirName := strings.ReplaceAll(cwd, "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", projectDirName)

	// Find the most recently modified JSONL file
	matches, err := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}

	var newest string
	var newestTime time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = m
		}
	}

	if newest == "" {
		return ""
	}

	// Extract session ID from filename (UUID.jsonl)
	base := filepath.Base(newest)
	return strings.TrimSuffix(base, ".jsonl")
}

// truncateName truncates a name to maxLen chars, adding ellipsis if needed.
func truncateName(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	return name[:maxLen-1] + "\u2026"
}

// shortenPath replaces the home directory with ~ in a path.
func shortenPath(path string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}
