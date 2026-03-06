package state

import (
	"fmt"
	"sync"
)

// WindowState holds session info for a bound window.
type WindowState struct {
	SessionID  string `json:"session_id"`
	CWD        string `json:"cwd"`
	WindowName string `json:"window_name"`
}

// UserThread identifies a user+thread binding.
type UserThread struct {
	UserID   string
	ThreadID string
}

// WorktreeInfo holds worktree metadata for a thread.
type WorktreeInfo struct {
	WorktreeDir  string `json:"worktree_dir"`
	Branch       string `json:"branch"`
	RepoRoot     string `json:"repo_root"`
	BaseBranch   string `json:"base_branch"`
	TaskID       string `json:"task_id,omitempty"`
	IsMergeTopic bool   `json:"is_merge_topic,omitempty"`
}

// State is the main application state, persisted as state.json.
type State struct {
	mu                 sync.RWMutex
	ThreadBindings     map[string]map[string]string `json:"thread_bindings"`      // user_id → thread_id → window_id
	WindowStates       map[string]WindowState       `json:"window_states"`        // window_id → state
	WindowDisplayNames map[string]string            `json:"window_display_names"` // window_id → display_name
	UserWindowOffsets  map[string]map[string]int64  `json:"user_window_offsets"`  // user_id → window_id → byte_offset
	GroupChatIDs       map[string]int64             `json:"group_chat_ids"`       // "user_id:thread_id" → chat_id
	ProjectBindings    map[string]string            `json:"project_bindings"`     // thread_id → project_id
	WorktreeBindings   map[string]WorktreeInfo      `json:"worktree_bindings"`    // thread_id → worktree info
}

// NewState creates a new empty state.
func NewState() *State {
	return &State{
		ThreadBindings:     make(map[string]map[string]string),
		WindowStates:       make(map[string]WindowState),
		WindowDisplayNames: make(map[string]string),
		UserWindowOffsets:  make(map[string]map[string]int64),
		GroupChatIDs:       make(map[string]int64),
		ProjectBindings:    make(map[string]string),
		WorktreeBindings:   make(map[string]WorktreeInfo),
	}
}

// Load reads state from a JSON file. Returns empty state if file doesn't exist.
func Load(path string) (*State, error) {
	s := NewState()
	if err := loadJSON(path, s); err != nil {
		return nil, err
	}
	// Ensure all maps are initialized after loading
	if s.ThreadBindings == nil {
		s.ThreadBindings = make(map[string]map[string]string)
	}
	if s.WindowStates == nil {
		s.WindowStates = make(map[string]WindowState)
	}
	if s.WindowDisplayNames == nil {
		s.WindowDisplayNames = make(map[string]string)
	}
	if s.UserWindowOffsets == nil {
		s.UserWindowOffsets = make(map[string]map[string]int64)
	}
	if s.GroupChatIDs == nil {
		s.GroupChatIDs = make(map[string]int64)
	}
	if s.ProjectBindings == nil {
		s.ProjectBindings = make(map[string]string)
	}
	if s.WorktreeBindings == nil {
		s.WorktreeBindings = make(map[string]WorktreeInfo)
	}
	return s, nil
}

// Save writes state to a JSON file atomically.
func (s *State) Save(path string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return atomicWriteJSON(path, s)
}

// BindThread binds a thread to a window for a user.
func (s *State) BindThread(userID, threadID, windowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ThreadBindings[userID] == nil {
		s.ThreadBindings[userID] = make(map[string]string)
	}
	s.ThreadBindings[userID][threadID] = windowID
}

// UnbindThread removes a thread binding for a user.
func (s *State) UnbindThread(userID, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.ThreadBindings[userID]; m != nil {
		delete(m, threadID)
		if len(m) == 0 {
			delete(s.ThreadBindings, userID)
		}
	}
}

// GetWindowForThread returns the window ID bound to a thread, if any.
func (s *State) GetWindowForThread(userID, threadID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m := s.ThreadBindings[userID]; m != nil {
		wid, ok := m[threadID]
		return wid, ok
	}
	return "", false
}

// FindUsersForWindow returns all (userID, threadID) pairs bound to a window.
func (s *State) FindUsersForWindow(windowID string) []UserThread {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []UserThread
	for uid, threads := range s.ThreadBindings {
		for tid, wid := range threads {
			if wid == windowID {
				result = append(result, UserThread{UserID: uid, ThreadID: tid})
			}
		}
	}
	return result
}

// SetWindowState sets the state for a window.
func (s *State) SetWindowState(windowID string, ws WindowState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WindowStates[windowID] = ws
}

// GetWindowState returns the state for a window.
func (s *State) GetWindowState(windowID string) (WindowState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ws, ok := s.WindowStates[windowID]
	return ws, ok
}

// RemoveWindowState removes all state for a window.
func (s *State) RemoveWindowState(windowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.WindowStates, windowID)
	delete(s.WindowDisplayNames, windowID)
	// Remove window from all user offsets
	for uid := range s.UserWindowOffsets {
		delete(s.UserWindowOffsets[uid], windowID)
		if len(s.UserWindowOffsets[uid]) == 0 {
			delete(s.UserWindowOffsets, uid)
		}
	}
}

// SetGroupChatID stores the group chat ID for a user+thread.
func (s *State) SetGroupChatID(userID, threadID string, chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s:%s", userID, threadID)
	s.GroupChatIDs[key] = chatID
}

// GetGroupChatID returns the group chat ID for a user+thread.
func (s *State) GetGroupChatID(userID, threadID string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := fmt.Sprintf("%s:%s", userID, threadID)
	id, ok := s.GroupChatIDs[key]
	return id, ok
}

// RemoveGroupChatID removes the group chat ID for a user+thread.
func (s *State) RemoveGroupChatID(userID, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s:%s", userID, threadID)
	delete(s.GroupChatIDs, key)
}

// BindProject binds a thread to a Minuano project.
func (s *State) BindProject(threadID, projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ProjectBindings[threadID] = projectID
}

// GetProject returns the Minuano project for a thread.
func (s *State) GetProject(threadID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.ProjectBindings[threadID]
	return p, ok
}

// RemoveProject removes the project binding for a thread.
func (s *State) RemoveProject(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ProjectBindings, threadID)
}

// SetWindowDisplayName sets the display name for a window.
func (s *State) SetWindowDisplayName(windowID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WindowDisplayNames[windowID] = name
}

// GetWindowDisplayName returns the display name for a window.
func (s *State) GetWindowDisplayName(windowID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.WindowDisplayNames[windowID]
	return n, ok
}

// SetUserWindowOffset sets the byte offset for a user+window.
func (s *State) SetUserWindowOffset(userID, windowID string, offset int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.UserWindowOffsets[userID] == nil {
		s.UserWindowOffsets[userID] = make(map[string]int64)
	}
	s.UserWindowOffsets[userID][windowID] = offset
}

// GetUserWindowOffset returns the byte offset for a user+window.
func (s *State) GetUserWindowOffset(userID, windowID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m := s.UserWindowOffsets[userID]; m != nil {
		return m[windowID]
	}
	return 0
}

// AllUserIDs returns all user IDs that have thread bindings.
func (s *State) AllUserIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.ThreadBindings))
	for uid := range s.ThreadBindings {
		ids = append(ids, uid)
	}
	return ids
}

// AllBoundWindowIDs returns all window IDs that are currently bound to any thread.
func (s *State) AllBoundWindowIDs() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]bool)
	for _, threads := range s.ThreadBindings {
		for _, wid := range threads {
			result[wid] = true
		}
	}
	return result
}

// SetWorktreeInfo stores worktree metadata for a thread.
func (s *State) SetWorktreeInfo(threadID string, wi WorktreeInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WorktreeBindings[threadID] = wi
}

// GetWorktreeInfo returns the worktree info for a thread.
func (s *State) GetWorktreeInfo(threadID string) (WorktreeInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	wi, ok := s.WorktreeBindings[threadID]
	return wi, ok
}

// RemoveWorktreeInfo removes worktree metadata for a thread.
func (s *State) RemoveWorktreeInfo(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.WorktreeBindings, threadID)
}

// AllWorktreeThreadIDs returns all thread IDs that have worktree bindings.
func (s *State) AllWorktreeThreadIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.WorktreeBindings))
	for tid := range s.WorktreeBindings {
		ids = append(ids, tid)
	}
	return ids
}
