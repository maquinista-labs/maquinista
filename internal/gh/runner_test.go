package gh

import (
	"errors"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/pipeline"
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

// MAQ-24: the reply-comment path quotes the URL gh prints for the new
// comment back into the Pipeline topic.
func TestParseUpdateBranchErr(t *testing.T) {
	// 422: base cannot merge into the branch cleanly — a deterministic
	// conflict, not infrastructure (MAQ-26).
	if err := parseUpdateBranchErr("gh: Merge conflict (HTTP 422)"); !errors.Is(err, pipeline.ErrMergeUpConflict) {
		t.Errorf("422 stderr → %v, want ErrMergeUpConflict", err)
	}
	// 409: the branch moved under expected_head_sha.
	if err := parseUpdateBranchErr("gh: Expected head sha to be abc (HTTP 409)"); !errors.Is(err, pipeline.ErrMergeUpRace) {
		t.Errorf("409 stderr → %v, want ErrMergeUpRace", err)
	}
	// Anything else is infrastructure — unclassified.
	if err := parseUpdateBranchErr("gh: auth expired (HTTP 401)"); err != nil {
		t.Errorf("401 stderr → %v, want nil (infra)", err)
	}
	if err := parseUpdateBranchErr(""); err != nil {
		t.Errorf("empty stderr → %v, want nil (infra)", err)
	}
}

func TestParsePRCommentURL(t *testing.T) {
	got, err := parsePRCommentURL([]byte("https://github.com/o/r/pull/7#issuecomment-123456\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://github.com/o/r/pull/7#issuecomment-123456" {
		t.Errorf("url = %q", got)
	}
	if _, err := parsePRCommentURL([]byte("")); err == nil {
		t.Error("empty output must error")
	}
	if _, err := parsePRCommentURL([]byte("some unexpected banner")); err == nil {
		t.Error("output without a comment url must error")
	}
}
