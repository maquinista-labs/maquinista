package gh

import (
	"testing"
	"time"
)

func TestParseComments(t *testing.T) {
	got, err := parseComments([]byte(`{"comments":[
		{"author":{"login":"alice","is_bot":false},"body":"please add tests","createdAt":"2026-10-03T09:00:00Z"},
		{"author":{"login":"ci-bot[bot]"},"body":"coverage 91%","createdAt":"2026-10-03T09:05:00Z"},
		{"author":{"login":"deploy-app","__typename":"Bot"},"body":"deployed","createdAt":"2026-10-03T09:06:00Z"},
		{"author":{"login":"bob","is_bot":false},"body":"nit: gofmt","createdAt":"2026-10-03T09:10:00Z"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0].Author != "alice" || got[0].IsBot || got[0].Body != "please add tests" {
		t.Errorf("comment 0 = %+v", got[0])
	}
	want, err := time.Parse(time.RFC3339, "2026-10-03T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].CreatedAt.Equal(want) {
		t.Errorf("createdAt = %v, want %v", got[0].CreatedAt, want)
	}
	// All three bot spellings must be flagged.
	for i := 1; i <= 2; i++ {
		if !got[i].IsBot {
			t.Errorf("comment %d (%s) not flagged as bot", i, got[i].Author)
		}
	}
	if got[3].IsBot {
		t.Errorf("comment 3 (bob) flagged as bot")
	}
}

func TestParseComments_Empty(t *testing.T) {
	got, err := parseComments([]byte(`{"comments":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
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
