package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Archive is a shared collection of archival passages that multiple agents
// can read from or write to. See Phase 5 of plans/active/agent-memory-db.md.
type Archive struct {
	ID           int64
	Name         string
	Description  string
	OwnerAgentID string
	CreatedAt    time.Time
}

// ArchiveMember is a row from archive_members.
type ArchiveMember struct {
	ArchiveID int64
	AgentID   string
	Role      string // 'owner' | 'writer' | 'reader'
	GrantedAt time.Time
}

// CreateArchive creates a new shared archive owned by ownerAgentID.
// Returns ErrNotFound if the owner agent does not exist.
func CreateArchive(ctx context.Context, q Querier, ownerAgentID, name, description string) (int64, error) {
	if ownerAgentID == "" {
		return 0, errors.New("archive: owner_agent_id required")
	}
	if name == "" {
		return 0, errors.New("archive: name required")
	}
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO agent_archives (name, description, owner_agent_id)
		VALUES ($1, $2, $3)
		RETURNING id
	`, name, nullIfEmpty(description), ownerAgentID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create archive: %w", err)
	}
	return id, nil
}

// GetArchive loads one archive by id.
func GetArchive(ctx context.Context, q Querier, id int64) (*Archive, error) {
	a := &Archive{ID: id}
	err := q.QueryRow(ctx, `
		SELECT name, COALESCE(description,''), owner_agent_id, created_at
		FROM agent_archives WHERE id=$1
	`, id).Scan(&a.Name, &a.Description, &a.OwnerAgentID, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get archive: %w", err)
	}
	return a, nil
}

// ListArchives returns all archives the agent owns or has membership in.
func ListArchives(ctx context.Context, q Querier, agentID string) ([]Archive, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT a.id, a.name, COALESCE(a.description,''), a.owner_agent_id, a.created_at
		FROM agent_archives a
		LEFT JOIN archive_members m ON m.archive_id = a.id AND m.agent_id = $1
		WHERE a.owner_agent_id = $1 OR m.agent_id = $1
		ORDER BY a.name
	`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Archive
	for rows.Next() {
		var a Archive
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.OwnerAgentID, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GrantAccess upserts an archive_members row granting agentID the given role.
func GrantAccess(ctx context.Context, q Querier, archiveID int64, agentID, role string) error {
	switch role {
	case "owner", "writer", "reader":
	default:
		return fmt.Errorf("archive: role must be owner|writer|reader, got %q", role)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO archive_members (archive_id, agent_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (archive_id, agent_id) DO UPDATE SET
			role       = EXCLUDED.role,
			granted_at = NOW()
	`, archiveID, agentID, role); err != nil {
		return fmt.Errorf("grant archive access: %w", err)
	}
	return nil
}

// RevokeAccess removes an agent from an archive's membership list.
func RevokeAccess(ctx context.Context, q Querier, archiveID int64, agentID string) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM archive_members WHERE archive_id=$1 AND agent_id=$2
	`, archiveID, agentID)
	if err != nil {
		return fmt.Errorf("revoke archive access: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListMembers returns all members of an archive.
func ListMembers(ctx context.Context, q Querier, archiveID int64) ([]ArchiveMember, error) {
	rows, err := q.Query(ctx, `
		SELECT archive_id, agent_id, role, granted_at
		FROM archive_members WHERE archive_id=$1
		ORDER BY role, agent_id
	`, archiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchiveMember
	for rows.Next() {
		var m ArchiveMember
		if err := rows.Scan(&m.ArchiveID, &m.AgentID, &m.Role, &m.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RememberInArchive inserts a new archival passage scoped to an archive.
// The caller must have at least 'writer' access (enforced by the application
// layer — this function writes unconditionally). Returns the new passage id.
func RememberInArchive(ctx context.Context, q Querier, m Memory, archiveID int64) (int64, error) {
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
			 source, source_ref, tags, pinned, score, expires_at,
			 owner_scope, owner_ref, archive_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
		        'agent', NULL, $13)
		RETURNING id
	`, m.AgentID, m.Dimension, m.Tier, m.Category, m.Title, m.Body,
		m.Source, nullIfEmpty(m.SourceRef), m.Tags, m.Pinned, m.Score, m.ExpiresAt,
		archiveID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("remember in archive: %w", err)
	}
	return id, nil
}

// SearchArchive runs FTS over passages the caller can read: their own
// private rows plus any archive they have membership in.
func SearchArchive(ctx context.Context, q Querier, callerAgentID, query string, f ListFilter) ([]Memory, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	rows, err := q.Query(ctx, `
		SELECT m.id, m.dimension, m.tier, m.category, m.title, m.body,
		       m.source, COALESCE(m.source_ref,''), m.tags, m.pinned, m.score,
		       m.created_at, m.updated_at, m.expires_at
		FROM agent_memories m, plainto_tsquery('simple', $2) qq
		WHERE (m.expires_at IS NULL OR m.expires_at > NOW())
		  AND ($3 = '' OR m.dimension = $3)
		  AND ($4 = '' OR m.tier = $4)
		  AND ($5 = '' OR m.category = $5)
		  AND m.tsv @@ qq
		  AND (
		       m.agent_id = $1
		    OR m.archive_id IN (
		         SELECT archive_id FROM archive_members WHERE agent_id = $1
		       )
		  )
		ORDER BY m.pinned DESC, ts_rank(m.tsv, qq) DESC, m.created_at DESC
		LIMIT $6
	`, callerAgentID, query, f.Dimension, f.Tier, f.Category, limit)
	if err != nil {
		return nil, err
	}
	return scanMemoryRows(rows, callerAgentID)
}
