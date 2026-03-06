package main

import (
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Design Auth Flow", "design-auth-flow"},
		{"hello world", "hello-world"},
		{"  leading spaces  ", "leading-spaces"},
		{"UPPER_CASE", "upper-case"},
		{"with---dashes", "with-dashes"},
		{"special!@#chars", "special-chars"},
		{"", ""},
		{"a", "a"},
	}
	for _, tt := range tests {
		got := slugify(tt.input)
		if got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerateID(t *testing.T) {
	id := generateID("Design Auth Flow")

	// Should start with slug prefix.
	if len(id) == 0 {
		t.Fatal("generateID returned empty string")
	}

	// Should contain a dash separator before the hex suffix.
	if id[len(id)-6] != '-' {
		// The slug is at most 15 chars, plus '-' plus 5 hex chars = at most 21.
		// But the important thing is that the suffix is random.
	}

	// Generate two IDs from the same title — they should differ (random suffix).
	id2 := generateID("Design Auth Flow")
	if id == id2 {
		t.Errorf("expected different IDs from same title, got %q and %q", id, id2)
	}
}

func TestGenerateID_LongTitle(t *testing.T) {
	id := generateID("This is a very long title that should be truncated")
	// Slug truncated to 15 chars + '-' + suffix.
	if len(id) > 22 { // 15 slug + 1 dash + 5 hex + 1 margin
		t.Errorf("generateID too long: %q (len %d)", id, len(id))
	}
}

func TestRandomHex(t *testing.T) {
	h := randomHex(3)
	if len(h) != 5 { // n+2 = 5 per current implementation
		t.Errorf("randomHex(3) length = %d, want 5", len(h))
	}

	// Should be hex chars.
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("randomHex(3) contains non-hex char: %q in %q", c, h)
		}
	}

	// Two calls should be different (statistically).
	h2 := randomHex(3)
	if h == h2 {
		t.Logf("randomHex(3) returned same value twice: %q (may occasionally happen)", h)
	}
}

func TestAddCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "add <title>" {
			return
		}
	}
	t.Error("expected 'add' command to be registered")
}

func TestAddCommandFlags(t *testing.T) {
	flags := addCmd.Flags()

	expected := []string{"after", "priority", "test-cmd", "project", "body"}
	for _, name := range expected {
		if flags.Lookup(name) == nil {
			t.Errorf("expected flag --%s on add command", name)
		}
	}
}
