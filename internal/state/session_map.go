package state

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
)

// SessionMapEntry holds info written by the hook for each tmux window.
type SessionMapEntry struct {
	SessionID  string `json:"session_id"`
	CWD        string `json:"cwd"`
	WindowName string `json:"window_name"`
}

// LoadSessionMap reads session_map.json.
func LoadSessionMap(path string) (map[string]SessionMapEntry, error) {
	data := make(map[string]SessionMapEntry)
	if err := loadJSON(path, &data); err != nil {
		return nil, err
	}
	if data == nil {
		data = make(map[string]SessionMapEntry)
	}
	return data, nil
}

// WriteSessionMap writes session_map.json with file locking (flock).
func WriteSessionMap(path string, data map[string]SessionMapEntry) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("opening lock file: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquiring flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return atomicWriteJSON(path, data)
}

// ReadModifyWriteSessionMap reads, modifies, and writes session_map.json with flock.
func ReadModifyWriteSessionMap(path string, modify func(map[string]SessionMapEntry)) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("opening lock file: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquiring flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data := make(map[string]SessionMapEntry)
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("parsing session map: %w", err)
		}
	}

	modify(data)

	return atomicWriteJSON(path, data)
}

// RemoveSessionMapEntry removes an entry by key from session_map.json with flock.
func RemoveSessionMapEntry(path, key string) error {
	return ReadModifyWriteSessionMap(path, func(data map[string]SessionMapEntry) {
		delete(data, key)
	})
}
