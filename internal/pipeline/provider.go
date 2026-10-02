// Package pipeline implements the ticket-system ↔ maquinista bridge
// (ADR-0005, ADR-0006): intake (ticket-system issues → task rows) and sync
// (Postgres task state transitions → board, with retry). Agents never talk
// to the ticket system directly — the determinism boundary is the tasks
// state machine. Provider-neutral by design: ticket-system specifics live in
// the provider implementation (linear.go for Linear), the core below speaks
// only TicketProvider + canonical Columns.
package pipeline

import (
	"context"
	"fmt"
)

// Column is a canonical pipeline column (ADR-0006): the provider-free name
// stored in ticket_issue_map.pending_state / last_synced_state. Providers
// map these to their own column identifiers via TicketProvider.Columns.
type Column int

const (
	ColInProgress Column = iota
	ColInReview
	ColChangesRequested
	ColNeedsHuman
	ColReadyToMerge
	ColDone
)

// String returns the canonical column name. These are the stored values, so
// they are stable API: existing rows in ticket_issue_map depend on them.
func (c Column) String() string {
	switch c {
	case ColInProgress:
		return "In Progress"
	case ColInReview:
		return "In Review"
	case ColChangesRequested:
		return "Changes Requested"
	case ColNeedsHuman:
		return "Needs Human"
	case ColReadyToMerge:
		return "Ready to Merge"
	case ColDone:
		return "Done"
	default:
		return ""
	}
}

// columnFromName resolves a stored canonical name back to its Column.
func columnFromName(name string) (Column, bool) {
	for c := ColInProgress; c <= ColDone; c++ {
		if c.String() == name {
			return c, true
		}
	}
	return 0, false
}

// Issue is the slice of a ticket-system issue the bridge needs. Key is the
// human-facing key (e.g. "MAQ-2"); ID is the provider's stable identifier —
// the ticket_issue_map primary key, so the INSERT is the claim.
type Issue struct {
	ID          string
	Key         string
	Title       string
	Description string
	URL         string
}

// TicketProvider abstracts the ticket/kanban system (ADR-0006). Adding a new
// tracker means one implementation of this interface plus one NewProvider
// case; the core bridge, sync, schema and config never change.
type TicketProvider interface {
	// IntakeIssues returns the issues waiting for the pipeline in teamID —
	// the intake queue (for Linear: workflow state "Todo" + label "pipeline").
	IntakeIssues(ctx context.Context, teamID string) ([]Issue, error)

	// Columns resolves canonical columns to provider column IDs for teamID.
	// Canonical columns missing on the board are absent from the map; the
	// sync loop backs off when asked to push one, mirroring a renamed column.
	Columns(ctx context.Context, teamID string) (map[Column]string, error)

	// SetIssueColumn moves an issue to the column with the given provider ID.
	SetIssueColumn(ctx context.Context, issueID, columnID string) error

	// AddIssueLink posts a link (the task's PR URL) onto the issue — for
	// Linear, a comment. Exactly-once delivery is the CALLER's job: the
	// sync loop dedups on ticket_issue_map.pr_url_synced, so the provider
	// implementation must be safe to call again after a failure.
	AddIssueLink(ctx context.Context, issueID, url string) error
}

// NewProvider resolves a provider by name (MAQUINISTA_TICKETS_PROVIDER).
func NewProvider(name, apiKey string) (TicketProvider, error) {
	if name == "" {
		name = "linear"
	}
	switch name {
	case "linear":
		return newLinearProvider(apiKey), nil
	default:
		return nil, fmt.Errorf("pipeline: unknown ticket provider %q (supported: linear)", name)
	}
}
