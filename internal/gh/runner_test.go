package gh

import (
	"testing"
	"time"
)

func TestParseComments(t *testing.T) {
	got, err := parseComments([]byte(`[
		{"id":11,"user":{"login":"alice","type":"User"},"body":"please add tests","created_at":"2026-10-01T09:00:00Z"},
		{"id":12,"user":{"login":"ci-bot[bot]","type":"User"},"body":"coverage 91%","created_at":"2026-10-03T09:05:00Z"},
		{"id":13,"user":{"login":"deploy-app","type":"Bot"},"body":"deployed","created_at":"2026-10-03T09:06:00Z"},
		{"id":14,"user":{"login":"bob","type":"User"},"body":"nit: gofmt","created_at":"2026-10-03T09:10:00Z"}
	]`), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0].ID != 11 || got[0].Author != "alice" || got[0].IsBot || got[0].Body != "please add tests" {
		t.Errorf("comment 0 = %+v", got[0])
	}
	want, err := time.Parse(time.RFC3339, "2026-10-01T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].CreatedAt.Equal(want) {
		t.Errorf("createdAt = %v, want %v", got[0].CreatedAt, want)
	}
	// Both bot spellings must be flagged (login suffix and REST type).
	for i := 1; i <= 2; i++ {
		if !got[i].IsBot {
			t.Errorf("comment %d (%s) not flagged as bot", i, got[i].Author)
		}
	}
	if got[3].IsBot {
		t.Errorf("comment 3 (bob) flagged as bot")
	}
}

func TestParseComments_SinceWindow(t *testing.T) {
	// Oldest-first REST payload; since=10-03 keeps only the later two.
	got, err := parseComments([]byte(`[
		{"id":11,"user":{"login":"alice","type":"User"},"body":"old","created_at":"2026-10-01T09:00:00Z"},
		{"id":12,"user":{"login":"ci-bot[bot]","type":"User"},"body":"mid","created_at":"2026-10-03T09:05:00Z"},
		{"id":13,"user":{"login":"bob","type":"User"},"body":"new","created_at":"2026-10-03T09:10:00Z"}
	]`), mustTime(t, "2026-10-03T09:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 12 || got[1].Body != "new" {
		t.Fatalf("since window = %+v, want ids [12 13]", got)
	}
}

func TestParseComments_Empty(t *testing.T) {
	got, err := parseComments([]byte(`[]`), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{"no checks", `{"statusCheckRollup":[]}`, "none"},
		{"all green", `{"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}`, "green"},
		{"neutral skipped count as green", `{"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"SKIPPED"}]}`, "green"},
		{"in progress", `{"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"IN_PROGRESS"}]}`, "pending"},
		{"failure wins over pending", `{"statusCheckRollup":[{"status":"IN_PROGRESS"},{"status":"COMPLETED","conclusion":"FAILURE"}]}`, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChecks([]byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("parseChecks = %q, want %q", got, tc.want)
			}
		})
	}
}
