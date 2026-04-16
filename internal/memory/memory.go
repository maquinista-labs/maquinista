// Package memory owns the three-layer agent memory surface described in
// plans/active/agent-memory-db.md: core blocks (always in-context),
// archival passages (retrieved on demand), and recall (inbox/outbox, not
// owned by this package).
//
// All storage is Postgres per §0 of maquinista-v2.md. No markdown, no
// JSON on disk.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the minimal pgx surface this package uses.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrNotFound is returned by Get / load helpers when the row is absent.
var ErrNotFound = errors.New("memory: not found")

// ErrBlockCharLimit is returned when an edit would push a block's value
// past its char_limit. Letta-style curation pressure: the agent has to
// prune before appending more.
var ErrBlockCharLimit = errors.New("memory: block char_limit exceeded")

// ====================== CORE BLOCKS ===================================

// Block mirrors agent_blocks.
type Block struct {
	ID          int64
	AgentID     string
	Label       string
	Value       string
	CharLimit   int
	ReadOnly    bool
	Description string
	Version     int
	UpdatedAt   time.Time
}

// DefaultBlocks are the block labels seeded for every new agent. Matches
// the Letta block set plus one extra (`task-note`) used by the planner.
var DefaultBlocks = []struct {
	Label       string
	Description string
	CharLimit   int
}{
	{"persona", "Your self-notes: environment quirks, tool conventions, preferences you've learned.", 2200},
	{"human", "Facts about the operator you're talking to: preferences, workflow, style.", 2200},
	{"task-note", "Scratchpad for the current turn. Auto-cleared when conversation_id changes.", 1000},
}

// SeedDefaultBlocks creates the default core blocks for an agent.
// Idempotent via the (agent_id, label) unique constraint.
// seedPersona (optional) is used as the initial value of the persona
// block — callers pass the soul's core_truths here.
func SeedDefaultBlocks(ctx context.Context, q Querier, agentID, seedPersona string) error {
	for _, b := range DefaultBlocks {
		value := ""
		if b.Label == "persona" {
			value = seedPersona
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO agent_blocks (agent_id, label, value, char_limit, description)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (agent_id, label) DO NOTHING
		`, agentID, b.Label, value, b.CharLimit, b.Description); err != nil {
			return fmt.Errorf("seed block %s: %w", b.Label, err)
		}
	}
	return nil
}

// LoadBlock reads one block for an agent.
func LoadBlock(ctx context.Context, q Querier, agentID, label string) (*Block, error) {
	b := &Block{AgentID: agentID, Label: label}
	err := q.QueryRow(ctx, `
		SELECT id, COALESCE(value,''), char_limit, read_only,
		       COALESCE(description,''), version, updated_at
		FROM agent_blocks WHERE agent_id = $1 AND label = $2
	`, agentID, label).Scan(&b.ID, &b.Value, &b.CharLimit, &b.ReadOnly,
		&b.Description, &b.Version, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load block %s/%s: %w", agentID, label, err)
	}
	return b, nil
}

// LoadAllBlocks returns all blocks for an agent, ordered by label.
func LoadAllBlocks(ctx context.Context, q Querier, agentID string) ([]Block, error) {
	rows, err := q.Query(ctx, `
		SELECT id, label, COALESCE(value,''), char_limit, read_only,
		       COALESCE(description,''), version, updated_at
		FROM agent_blocks WHERE agent_id = $1
		ORDER BY label
	`, agentID)
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

// AppendBlock appends content to a block's current value, enforcing
// char_limit. Returns ErrBlockCharLimit when the result would exceed the
// cap; the agent must prune and retry.
func AppendBlock(ctx context.Context, q Querier, agentID, label, content string) (string, error) {
	b, err := LoadBlock(ctx, q, agentID, label)
	if err != nil {
		return "", err
	}
	if b.ReadOnly {
		return "", fmt.Errorf("block %s is read_only", label)
	}
	joined := b.Value
	if joined != "" && !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	joined += content
	if len(joined) > b.CharLimit {
		return "", fmt.Errorf("%w: %s would be %d > %d chars", ErrBlockCharLimit, label, len(joined), b.CharLimit)
	}
	if _, err := q.Exec(ctx, `
		UPDATE agent_blocks SET value=$1, version=version+1, updated_at=NOW()
		WHERE agent_id=$2 AND label=$3
	`, joined, agentID, label); err != nil {
		return "", err
	}
	return joined, nil
}

// ReplaceBlock does an exact-match replacement inside a block's value.
// Mirrors Letta's core_memory_replace: fails if oldContent isn't found
// verbatim, so the agent can't silently corrupt state.
func ReplaceBlock(ctx context.Context, q Querier, agentID, label, oldContent, newContent string) (string, error) {
	b, err := LoadBlock(ctx, q, agentID, label)
	if err != nil {
		return "", err
	}
	if b.ReadOnly {
		return "", fmt.Errorf("block %s is read_only", label)
	}
	if !strings.Contains(b.Value, oldContent) {
		return "", fmt.Errorf("old content not found in block %s", label)
	}
	updated := strings.Replace(b.Value, oldContent, newContent, 1)
	if len(updated) > b.CharLimit {
		return "", fmt.Errorf("%w: %s would be %d > %d chars", ErrBlockCharLimit, label, len(updated), b.CharLimit)
	}
	if _, err := q.Exec(ctx, `
		UPDATE agent_blocks SET value=$1, version=version+1, updated_at=NOW()
		WHERE agent_id=$2 AND label=$3
	`, updated, agentID, label); err != nil {
		return "", err
	}
	return updated, nil
}

// ====================== ARCHIVAL PASSAGES =============================

// Memory is an archival passage (row in agent_memories).
type Memory struct {
	ID         int64
	AgentID    string
	Dimension  string // 'agent' | 'user'
	Tier       string // 'long_term' | 'daily' | 'signal'
	Category   string
	Title      string
	Body       string
	Source     string
	SourceRef  string
	Tags       []string
	Pinned     bool
	Score      float32
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ExpiresAt  *time.Time
}

// Remember inserts a new archival passage. Returns the new id.
func Remember(ctx context.Context, q Querier, m Memory) (int64, error) {
	if err := validateMemory(m); err != nil {
		return 0, err
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO agent_memories
			(agent_id, dimension, tier, category, title, body,
			 source, source_ref, tags, pinned, score, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id
	`, m.AgentID, m.Dimension, m.Tier, m.Category, m.Title, m.Body,
		m.Source, nullIfEmpty(m.SourceRef), m.Tags, m.Pinned, m.Score, m.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("remember: %w", err)
	}
	return id, nil
}

// Get fetches one archival passage.
func Get(ctx context.Context, q Querier, agentID string, id int64) (*Memory, error) {
	m := &Memory{ID: id, AgentID: agentID}
	err := q.QueryRow(ctx, `
		SELECT dimension, tier, category, title, body, source,
		       COALESCE(source_ref,''), tags, pinned, score,
		       created_at, updated_at, expires_at
		FROM agent_memories WHERE id=$1 AND agent_id=$2
	`, id, agentID).Scan(&m.Dimension, &m.Tier, &m.Category, &m.Title,
		&m.Body, &m.Source, &m.SourceRef, &m.Tags, &m.Pinned, &m.Score,
		&m.CreatedAt, &m.UpdatedAt, &m.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Forget deletes an archival passage.
func Forget(ctx context.Context, q Querier, agentID string, id int64) error {
	tag, err := q.Exec(ctx, `DELETE FROM agent_memories WHERE id=$1 AND agent_id=$2`, id, agentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListFilter narrows List / Search by tier and/or category. Empty strings
// are wildcards.
type ListFilter struct {
	Dimension string
	Tier      string
	Category  string
	Pinned    *bool
	Limit     int
}

// List returns archival passages for an agent, most-recent first. Pinned
// rows always float to the top.
func List(ctx context.Context, q Querier, agentID string, f ListFilter) ([]Memory, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT id, dimension, tier, category, title, body, source,
		       COALESCE(source_ref,''), tags, pinned, score,
		       created_at, updated_at, expires_at
		FROM agent_memories
		WHERE agent_id = $1
		  AND (expires_at IS NULL OR expires_at > NOW())
		  AND ($2 = '' OR dimension = $2)
		  AND ($3 = '' OR tier = $3)
		  AND ($4 = '' OR category = $4)
		  AND ($5::boolean IS NULL OR pinned = $5::boolean)
		ORDER BY pinned DESC, created_at DESC
		LIMIT $6
	`, agentID, f.Dimension, f.Tier, f.Category, f.Pinned, limit)
	if err != nil {
		return nil, err
	}
	return scanMemoryRows(rows, agentID)
}

// Search runs a Postgres FTS query against agent_memories for an agent.
// Ranks by pinned-first, then ts_rank, then recency.
func Search(ctx context.Context, q Querier, agentID, query string, f ListFilter) ([]Memory, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	rows, err := q.Query(ctx, `
		SELECT m.id, m.dimension, m.tier, m.category, m.title, m.body,
		       m.source, COALESCE(m.source_ref,''), m.tags, m.pinned, m.score,
		       m.created_at, m.updated_at, m.expires_at
		FROM agent_memories m, plainto_tsquery('simple', $2) qq
		WHERE m.agent_id = $1
		  AND (m.expires_at IS NULL OR m.expires_at > NOW())
		  AND ($3 = '' OR m.dimension = $3)
		  AND ($4 = '' OR m.tier = $4)
		  AND ($5 = '' OR m.category = $5)
		  AND m.tsv @@ qq
		ORDER BY m.pinned DESC, ts_rank(m.tsv, qq) DESC, m.created_at DESC
		LIMIT $6
	`, agentID, query, f.Dimension, f.Tier, f.Category, limit)
	if err != nil {
		return nil, err
	}
	return scanMemoryRows(rows, agentID)
}

func scanMemoryRows(rows pgx.Rows, agentID string) ([]Memory, error) {
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		m := Memory{AgentID: agentID}
		if err := rows.Scan(&m.ID, &m.Dimension, &m.Tier, &m.Category,
			&m.Title, &m.Body, &m.Source, &m.SourceRef, &m.Tags,
			&m.Pinned, &m.Score, &m.CreatedAt, &m.UpdatedAt, &m.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Pin flips the pinned flag on a passage.
func Pin(ctx context.Context, q Querier, agentID string, id int64, pinned bool) error {
	tag, err := q.Exec(ctx, `
		UPDATE agent_memories SET pinned=$1, updated_at=NOW()
		WHERE id=$2 AND agent_id=$3
	`, pinned, id, agentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FetchForInjection returns the memories a spawn should stitch into the
// system prompt: all pinned rows + up to `extra` most-recent long_term
// rows per dimension. Caller renders them into the composed prompt.
func FetchForInjection(ctx context.Context, q Querier, agentID string, extraLongTerm int) ([]Memory, error) {
	rows, err := q.Query(ctx, `
		SELECT id, dimension, tier, category, title, body, source,
		       COALESCE(source_ref,''), tags, pinned, score,
		       created_at, updated_at, expires_at
		FROM agent_memories
		WHERE agent_id = $1
		  AND (expires_at IS NULL OR expires_at > NOW())
		  AND (pinned OR tier = 'long_term')
		ORDER BY pinned DESC, tier DESC, created_at DESC
		LIMIT $2
	`, agentID, 50+extraLongTerm)
	if err != nil {
		return nil, err
	}
	return scanMemoryRows(rows, agentID)
}

func validateMemory(m Memory) error {
	if m.AgentID == "" {
		return errors.New("memory: agent_id required")
	}
	switch m.Dimension {
	case "agent", "user":
	default:
		return fmt.Errorf("memory: dimension must be agent|user, got %q", m.Dimension)
	}
	switch m.Tier {
	case "long_term", "daily", "signal":
	default:
		return fmt.Errorf("memory: tier must be long_term|daily|signal, got %q", m.Tier)
	}
	switch m.Category {
	case "feedback", "project", "reference", "fact", "preference", "other":
	default:
		return fmt.Errorf("memory: category invalid: %q", m.Category)
	}
	if m.Title == "" {
		return errors.New("memory: title required")
	}
	if m.Body == "" {
		return errors.New("memory: body required")
	}
	if m.Source == "" {
		return errors.New("memory: source required")
	}
	if len(m.Title) > 120 {
		return fmt.Errorf("memory: title too long (>%d)", 120)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
