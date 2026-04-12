package bot

import (
	"testing"

	"github.com/maquinista-labs/maquinista/internal/config"
)

func TestIsAuthorized(t *testing.T) {
	b := &Bot{
		config: &config.Config{
			AllowedUsers:  []int64{100, 200},
			AllowedGroups: []int64{-100123},
		},
	}

	tests := []struct {
		name   string
		userID int64
		chatID int64
		want   bool
	}{
		{"allowed user, private chat", 100, 100, true},
		{"allowed user, allowed group", 100, -100123, true},
		{"allowed user, disallowed group", 100, -100999, false},
		{"disallowed user", 999, 999, false},
		{"allowed user 2", 200, 200, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.isAuthorized(tt.userID, tt.chatID)
			if got != tt.want {
				t.Errorf("isAuthorized(%d, %d) = %v, want %v", tt.userID, tt.chatID, got, tt.want)
			}
		})
	}
}

func TestIsAuthorized_NoGroupRestriction(t *testing.T) {
	b := &Bot{
		config: &config.Config{
			AllowedUsers:  []int64{100},
			AllowedGroups: nil, // empty = allow all
		},
	}

	if !b.isAuthorized(100, -100999) {
		t.Error("empty AllowedGroups should allow all groups")
	}
}
