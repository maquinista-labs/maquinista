package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CatchupPayload holds the formatted catch-up message and a flag indicating
// whether there's anything to report.
type CatchupPayload struct {
	Text     string
	HasDelta bool
}

// BuildCatchup queries for memory/soul changes written after `since` for the
// given agent. Returns a CatchupPayload whose Text is a human-readable
// summary suitable for injecting as a system inbox row. Returns
// HasDelta=false (and empty Text) when nothing changed.
//
// Called by reconcile_agents.go after a resume spawn so the agent learns
// about archival passages and block edits that accumulated while the daemon
// was down.
func BuildCatchup(ctx context.Context, q Querier, agentID string, since time.Time) (*CatchupPayload, error) {
	var sections []string

	// New archival passages.
	newMems, err := listSince(ctx, q, agentID, since)
	if err != nil {
		return nil, fmt.Errorf("catchup memories: %w", err)
	}
	if len(newMems) > 0 {
		var sb strings.Builder
		sb.WriteString("## New memories\n")
		for _, m := range newMems {
			sb.WriteString(fmt.Sprintf("- [%s/%s] %s: %s\n", m.Dimension, m.Category, m.Title, truncate(m.Body, 200)))
		}
		sections = append(sections, sb.String())
	}

	// Updated core blocks.
	updatedBlocks, err := blocksSince(ctx, q, agentID, since)
	if err != nil {
		return nil, fmt.Errorf("catchup blocks: %w", err)
	}
	if len(updatedBlocks) > 0 {
		var sb strings.Builder
		sb.WriteString("## Memory blocks updated\n")
		for _, b := range updatedBlocks {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", b.Label, truncate(b.Value, 200)))
		}
		sections = append(sections, sb.String())
	}

	// Soul edits (core_truths, boundaries, vibe, continuity).
	soulSection, err := soulSince(ctx, q, agentID, since)
	if err != nil {
		return nil, fmt.Errorf("catchup soul: %w", err)
	}
	if soulSection != "" {
		sections = append(sections, soulSection)
	}

	if len(sections) == 0 {
		return &CatchupPayload{HasDelta: false}, nil
	}

	sinceStr := since.UTC().Format("2006-01-02 15:04 UTC")
	text := fmt.Sprintf("Catching you up on changes since %s:\n\n%s",
		sinceStr, strings.Join(sections, "\n"))
	return &CatchupPayload{Text: text, HasDelta: true}, nil
}

func listSince(ctx context.Context, q Querier, agentID string, since time.Time) ([]Memory, error) {
	rows, err := q.Query(ctx, `
		SELECT id, dimension, tier, category, title, body, source,
		       COALESCE(source_ref,''), tags, pinned, score,
		       created_at, updated_at, expires_at
		FROM agent_memories
		WHERE agent_id = $1
		  AND created_at > $2
		  AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY pinned DESC, created_at DESC
		LIMIT 50
	`, agentID, since)
	if err != nil {
		return nil, err
	}
	return scanMemoryRows(rows, agentID)
}

func blocksSince(ctx context.Context, q Querier, agentID string, since time.Time) ([]Block, error) {
	rows, err := q.Query(ctx, `
		SELECT id, label, COALESCE(value,''), char_limit, read_only,
		       COALESCE(description,''), version, updated_at
		FROM agent_blocks
		WHERE agent_id = $1
		  AND updated_at > $2
		ORDER BY label
	`, agentID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Block
	for rows.Next() {
		b := Block{AgentID: agentID}
		if err := rows.Scan(&b.ID, &b.Label, &b.Value, &b.CharLimit,
			&b.ReadOnly, &b.Description, &b.Version, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// soulSince returns a "## Soul updated" section if the agent_souls row was
// modified after since, listing only the non-empty narrative fields that
// changed. Returns "" when the soul is absent or untouched.
func soulSince(ctx context.Context, q Querier, agentID string, since time.Time) (string, error) {
	var updatedAt time.Time
	var coreTruths, boundaries, vibe, continuity string
	err := q.QueryRow(ctx, `
		SELECT updated_at,
		       COALESCE(core_truths,''), COALESCE(boundaries,''),
		       COALESCE(vibe,''), COALESCE(continuity,'')
		FROM agent_souls WHERE agent_id = $1 AND updated_at > $2
	`, agentID, since).Scan(&updatedAt, &coreTruths, &boundaries, &vibe, &continuity)
	if err != nil {
		// ErrNoRows means soul untouched or absent — not an error.
		return "", nil
	}

	type field struct{ name, value string }
	fields := []field{
		{"core_truths", coreTruths},
		{"boundaries", boundaries},
		{"vibe", vibe},
		{"continuity", continuity},
	}
	var sb strings.Builder
	sb.WriteString("## Soul updated\n")
	any := false
	for _, f := range fields {
		if f.value != "" {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", f.name, truncate(f.value, 200)))
			any = true
		}
	}
	if !any {
		return "", nil
	}
	return sb.String(), nil
}

func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
