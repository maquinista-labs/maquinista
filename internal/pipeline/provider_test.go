package pipeline

import (
	"context"
	"testing"
)

func TestColumn_String(t *testing.T) {
	want := map[Column]string{
		ColInProgress:       "In Progress",
		ColInReview:         "In Review",
		ColChangesRequested: "Changes Requested",
		ColNeedsHuman:       "Needs Human",
		ColDone:             "Done",
	}
	for col, name := range want {
		if got := col.String(); got != name {
			t.Errorf("Column(%d).String() = %q, want %q", col, got, name)
		}
		if c, ok := columnFromName(name); !ok || c != col {
			t.Errorf("columnFromName(%q) = (%v, %v), want (%v, true)", name, c, ok, col)
		}
	}
	if got := Column(99).String(); got != "" {
		t.Errorf("unknown column String() = %q, want empty", got)
	}
	if _, ok := columnFromName("Code Review"); ok {
		t.Error("non-canonical name resolved; want false")
	}
}

func TestNewProvider(t *testing.T) {
	for _, name := range []string{"linear", ""} {
		p, err := NewProvider(name, "k")
		if err != nil || p == nil {
			t.Errorf("NewProvider(%q): provider=%v err=%v, want a provider and nil error", name, p, err)
		}
	}
	if _, err := NewProvider("jira", "k"); err == nil {
		t.Error("unknown provider name accepted; want an error naming the supported set")
	}
}

// updCall records one SetIssueColumn invocation.
type updCall struct {
	issueID, columnID string
}

// linkCall records one AddIssueLink invocation.
type linkCall struct {
	issueID, url string
}

// fakeTickets is the in-memory TicketProvider used by bridge and sync tests.
type fakeTickets struct {
	todo    []Issue
	todoErr error
	cols    map[Column]string // canonical column -> provider column id
	colsErr error
	updates []updCall
	updErr  error
	links   []linkCall
	linkErr error
}

func (f *fakeTickets) IntakeIssues(ctx context.Context, teamID string) ([]Issue, error) {
	return f.todo, f.todoErr
}

func (f *fakeTickets) Columns(ctx context.Context, teamID string) (map[Column]string, error) {
	return f.cols, f.colsErr
}

func (f *fakeTickets) SetIssueColumn(ctx context.Context, issueID, columnID string) error {
	f.updates = append(f.updates, updCall{issueID: issueID, columnID: columnID})
	return f.updErr
}

func (f *fakeTickets) AddIssueLink(ctx context.Context, issueID, url string) error {
	f.links = append(f.links, linkCall{issueID: issueID, url: url})
	return f.linkErr
}

// fullCols is the canonical → provider-id map a working board returns.
func fullCols() map[Column]string {
	return map[Column]string{
		ColInProgress:       "s-ip",
		ColInReview:         "s-ir",
		ColChangesRequested: "s-cr",
		ColNeedsHuman:       "s-nh",
		ColDone:             "s-d",
	}
}
