package bot

import (
	"log"
	"path/filepath"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// ReconcileState cleans up stale bindings by checking against live tmux windows.
// Called on startup to handle bot restarts where windows may have died.
func (b *Bot) ReconcileState() int {
	return b.reconcileState()
}

func (b *Bot) reconcileState() int {
	session := b.config.TmuxSessionName

	// Build map of live windows: windowID → Window
	windows, err := tmux.ListWindows(session)
	if err != nil {
		log.Printf("Recovery: cannot list windows: %v", err)
		return 0
	}

	liveIDs := make(map[string]bool)
	nameToID := make(map[string]string) // window_name → window_id
	for _, w := range windows {
		liveIDs[w.ID] = true
		nameToID[w.Name] = w.ID
	}

	// Track cleanup stats
	var dropped, reresolved int

	// Check each persisted window state
	b.mu.Lock()
	defer b.mu.Unlock()

	windowIDs := b.state.AllBoundWindowIDs()
	for windowID := range windowIDs {
		if liveIDs[windowID] {
			continue // alive, no action needed
		}

		// Try to re-resolve by matching display name against live window names
		displayName, hasName := b.state.GetWindowDisplayName(windowID)
		if hasName && displayName != "" {
			if newID, ok := nameToID[displayName]; ok && newID != windowID {
				// Re-resolved: update all references
				reResolveWindow(b.state, windowID, newID)
				reresolved++
				continue
			}
		}

		// Unresolvable: clean up everything for this window
		cleanupDeadWindow(b, windowID)
		dropped++
	}

	// Clean up stale project bindings for threads with no binding
	cleanStaleProjects(b.state)

	// Clean up stale session_map entries
	b.cleanStaleSessionMap(liveIDs)

	if dropped > 0 || reresolved > 0 {
		b.saveStateUnlocked()
	}

	total := 0
	for range b.state.AllBoundWindowIDs() {
		total++
	}

	log.Printf("Recovery: %d live bindings, %d re-resolved, %d dropped",
		total, reresolved, dropped)

	return total
}

// reResolveWindow updates all references from oldID to newID.
func reResolveWindow(s *state.State, oldID, newID string) {
	// Save values that RemoveWindowState will delete
	savedWS, hasWS := s.GetWindowState(oldID)
	savedName, hasName := s.GetWindowDisplayName(oldID)
	savedRunner := s.GetWindowRunner(oldID)
	savedActive, hasActive := s.GetActiveThread(oldID)

	// Save offsets before removal
	savedOffsets := make(map[string]int64)
	for _, userID := range s.AllUserIDs() {
		offset := s.GetUserWindowOffset(userID, oldID)
		if offset > 0 {
			savedOffsets[userID] = offset
		}
	}

	// Update thread bindings
	users := s.FindUsersForWindow(oldID)
	for _, ut := range users {
		s.UnbindThread(ut.UserID, ut.ThreadID)
		s.BindThread(ut.UserID, ut.ThreadID, newID)
	}

	// Remove old window state (this also removes display name, runner, and offsets)
	s.RemoveWindowState(oldID)

	// Restore to new ID
	if hasWS {
		s.SetWindowState(newID, savedWS)
	}
	if hasName {
		s.SetWindowDisplayName(newID, savedName)
	}
	s.SetWindowRunner(newID, savedRunner)
	for userID, offset := range savedOffsets {
		s.SetUserWindowOffset(userID, newID, offset)
	}
	if hasActive {
		s.SetActiveThread(newID, savedActive.UserID, savedActive.ThreadID)
	}
}

// handleDeadWindow detects a dead tmux window, cleans up state, and auto-recreates
// a new session in the same directory. Returns true if recovery was attempted.
// pendingText is optionally sent to the new session after creation.
func (b *Bot) handleDeadWindow(msg *tgbotapi.Message, windowID, pendingText string) bool {
	userID := strconv.FormatInt(msg.From.ID, 10)
	threadID := strconv.Itoa(getThreadID(msg))
	chatID := msg.Chat.ID
	threadIDInt := getThreadID(msg)

	// Check if binding still exists — the status poller may have already cleaned up
	if _, bound := b.state.GetWindowForThread(userID, threadID); !bound {
		log.Printf("Dead window %s: already cleaned up (race with poller), treating as unbound", windowID)
		b.reply(chatID, threadIDInt,
			"Agent session is gone. Restart it with `AGENT_ID=<id> claude` and resend.")
		return true
	}

	// Ensure the whole tmux session still exists (handles full session death)
	if err := tmux.EnsureSession(b.config.TmuxSessionName); err != nil {
		log.Printf("Error re-creating tmux session: %v", err)
	}

	// Save info we need before cleanup
	var cwd string
	var projectBinding string
	if ws, ok := b.state.GetWindowState(windowID); ok {
		cwd = ws.CWD
	}
	if proj, ok := b.state.GetProject(threadID); ok {
		projectBinding = proj
	}

	// Save GroupChatIDs for all users on this window (needed for re-binding)
	type chatIDEntry struct {
		userID   string
		threadID string
		chatID   int64
	}
	var savedChatIDs []chatIDEntry
	for _, ut := range b.state.FindUsersForWindow(windowID) {
		if cid, ok := b.state.GetGroupChatID(ut.UserID, ut.ThreadID); ok {
			savedChatIDs = append(savedChatIDs, chatIDEntry{ut.UserID, ut.ThreadID, cid})
		}
	}

	// Clean up all stale state for the dead window
	cleanupDeadWindow(b, windowID)

	// Clean up stale UI states that reference the dead window
	cancelBashCapture(msg.From.ID, threadIDInt)
	clearInteractiveUI(msg.From.ID, threadIDInt)
	screenshotStatesMu.Lock()
	delete(screenshotStates, screenshotKey(msg.From.ID, threadIDInt))
	screenshotStatesMu.Unlock()

	// Restore GroupChatIDs (cleanupDeadWindow removes them but we need them for new binding)
	for _, entry := range savedChatIDs {
		b.state.SetGroupChatID(entry.userID, entry.threadID, entry.chatID)
	}
	// Always ensure current user's chat ID is set (may not have been in savedChatIDs)
	b.state.SetGroupChatID(userID, threadID, chatID)
	b.saveState()

	if cwd == "" {
		log.Printf("Dead window %s: no CWD, cannot auto-restart", windowID)
		b.reply(chatID, threadIDInt,
			"Session died. Restart the agent with `AGENT_ID=<id> claude` and resend.")
		return true
	}

	// Auto-recreate in the same directory
	log.Printf("Dead window %s: auto-recreating in %s", windowID, cwd)
	b.reply(chatID, threadIDInt, "Session died. Restarting...")

	result, err := b.createWindowForDir(cwd, msg.From.ID, chatID, threadIDInt)
	if err != nil {
		log.Printf("Error auto-recreating window in %s: %v", cwd, err)
		b.reply(chatID, threadIDInt, "Failed to restart. Send a message to try again.")
		return true
	}

	// Restore project binding
	if projectBinding != "" {
		b.state.BindProject(threadID, projectBinding)
		b.saveState()
	}

	// Send pending text to new session
	if pendingText != "" {
		if err := tmux.SendKeysWithDelay(b.config.TmuxSessionName, result.WindowID, pendingText, 500); err != nil {
			log.Printf("Error sending pending text after recovery: %v", err)
		}
	}

	return true
}

// cleanupDeadWindow removes all state for a dead window.
// Idempotent — safe to call multiple times or concurrently.
func cleanupDeadWindow(b *Bot, windowID string) {
	// Find and unbind all threads
	users := b.state.FindUsersForWindow(windowID)
	for _, ut := range users {
		b.state.UnbindThread(ut.UserID, ut.ThreadID)
		b.state.RemoveGroupChatID(ut.UserID, ut.ThreadID)
	}

	// Remove window state and display name
	b.state.RemoveWindowState(windowID)

	// Remove monitor state and session_map entries
	sessionMapPath := filepath.Join(b.config.MaquinistaDir, "session_map.json")
	sm, err := state.LoadSessionMap(sessionMapPath)
	if err == nil {
		for key := range sm {
			if windowIDFromKey(key) == windowID {
				if b.monitorState != nil {
					b.monitorState.RemoveSession(key)
				}
				state.RemoveSessionMapEntry(sessionMapPath, key)
			}
		}
	}
}

// cleanStaleProjects removes project bindings for threads that have no bindings.
func cleanStaleProjects(s *state.State) {
	// Collect all thread IDs that have active bindings
	activeThreads := make(map[string]bool)
	for _, userID := range s.AllUserIDs() {
		// Check all threads for this user
		users := s.FindUsersForWindow("") // this won't work, need different approach
		for _, ut := range users {
			if ut.UserID == userID {
				activeThreads[ut.ThreadID] = true
			}
		}
	}

	// Note: We can't easily iterate ProjectBindings without exposing internals.
	// For now, project bindings are cleaned via handleTopicClose and are
	// not critical enough for startup cleanup.
}

// cleanStaleSessionMap removes session_map entries for dead windows.
func (b *Bot) cleanStaleSessionMap(liveIDs map[string]bool) {
	sessionMapPath := filepath.Join(b.config.MaquinistaDir, "session_map.json")
	sm, err := state.LoadSessionMap(sessionMapPath)
	if err != nil {
		return
	}

	var toRemove []string
	for key := range sm {
		wid := windowIDFromKey(key)
		if !liveIDs[wid] {
			toRemove = append(toRemove, key)
		}
	}

	for _, key := range toRemove {
		state.RemoveSessionMapEntry(sessionMapPath, key)
	}
}

// saveStateUnlocked saves state (caller must hold b.mu).
func (b *Bot) saveStateUnlocked() {
	path := filepath.Join(b.config.MaquinistaDir, "state.json")
	if err := b.state.Save(path); err != nil {
		log.Printf("Error saving state: %v", err)
	}
}
