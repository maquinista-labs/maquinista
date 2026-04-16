// Package mailbox provides typed wrappers for the agent_inbox / agent_outbox /
// channel_deliveries / message_attachments tables introduced by migration 009
// (see plans/reference/maquinista-v2.md §6). Every op accepts a pgx.Tx so callers hold
// the transaction boundary.
package mailbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LargeObjectThreshold is the inclusive byte size at which InsertAttachment
// switches from inline BYTEA to a server-side large object.
const LargeObjectThreshold = 5 * 1024 * 1024

// InboxMessage is the insert payload for EnqueueInbox.
type InboxMessage struct {
	AgentID        string
	ConversationID *uuid.UUID
	FromKind       string // 'user' | 'agent' | 'system'
	FromID         string
	OriginChannel  string // 'telegram' | ""
	OriginUserID   string
	OriginThreadID string
	OriginChatID   *int64
	ExternalMsgID  string
	Content        []byte // JSON-encoded
	MaxAttempts    int    // 0 → leave DB default
}

// InboxRow is the row layout returned by ClaimInbox.
type InboxRow struct {
	ID             uuid.UUID
	AgentID        string
	ConversationID *uuid.UUID
	FromKind       string
	FromID         *string
	OriginChannel  *string
	OriginUserID   *string
	OriginThreadID *string
	OriginChatID   *int64
	ExternalMsgID  *string
	Content        []byte
	Attempts       int
	MaxAttempts    int
	EnqueuedAt     time.Time
}

// EnqueueInbox inserts a row into agent_inbox. Returns (id, inserted).
// Duplicate (origin_channel, external_msg_id) collapses to a no-op: the
// returned id is the pre-existing row's id and inserted is false.
func EnqueueInbox(ctx context.Context, tx pgx.Tx, m InboxMessage) (uuid.UUID, bool, error) {
	if m.AgentID == "" {
		return uuid.Nil, false, errors.New("agent_id required")
	}
	if len(m.Content) == 0 {
		return uuid.Nil, false, errors.New("content required")
	}
	if m.FromKind == "" {
		m.FromKind = "user"
	}

	id := uuid.New()
	var returned uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO agent_inbox (
			id, agent_id, conversation_id, from_kind, from_id,
			origin_channel, origin_user_id, origin_thread_id, origin_chat_id,
			external_msg_id, content, max_attempts
		) VALUES (
			$1, $2, $3, $4, NULLIF($5,''),
			NULLIF($6,''), NULLIF($7,''), NULLIF($8,''), $9,
			NULLIF($10,''), $11, COALESCE(NULLIF($12,0), 5)
		)
		ON CONFLICT (origin_channel, external_msg_id) DO NOTHING
		RETURNING id
	`, id, m.AgentID, m.ConversationID, m.FromKind, m.FromID,
		m.OriginChannel, m.OriginUserID, m.OriginThreadID, m.OriginChatID,
		m.ExternalMsgID, m.Content, m.MaxAttempts,
	).Scan(&returned)

	if err == nil {
		return returned, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, fmt.Errorf("enqueue inbox: %w", err)
	}

	// ON CONFLICT path: look up the existing row.
	if m.OriginChannel == "" || m.ExternalMsgID == "" {
		return uuid.Nil, false, errors.New("insert returned no rows without conflict key")
	}
	err = tx.QueryRow(ctx, `
		SELECT id FROM agent_inbox
		WHERE origin_channel = $1 AND external_msg_id = $2
	`, m.OriginChannel, m.ExternalMsgID).Scan(&returned)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("lookup on conflict: %w", err)
	}
	return returned, false, nil
}

// ClaimInbox picks up to `limit` pending rows for `agentID` with a lease.
// Rows whose lease has expired while still 'processing' are also eligible.
// Uses FOR UPDATE SKIP LOCKED so concurrent claimers never overlap.
func ClaimInbox(ctx context.Context, tx pgx.Tx, agentID, workerID string, lease time.Duration, limit int) ([]InboxRow, error) {
	if limit <= 0 {
		limit = 1
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}

	rows, err := tx.Query(ctx, `
		WITH picked AS (
			SELECT id
			FROM agent_inbox
			WHERE agent_id = $1
			  AND (
			        status = 'pending'
			        OR (status = 'processing' AND lease_expires < NOW())
			      )
			ORDER BY enqueued_at
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE agent_inbox
		SET status     = 'processing',
		    claimed_by = $3,
		    claimed_at = NOW(),
		    lease_expires = NOW() + ($4 * INTERVAL '1 millisecond'),
		    attempts   = agent_inbox.attempts + 1
		FROM picked
		WHERE agent_inbox.id = picked.id
		RETURNING agent_inbox.id, agent_inbox.agent_id, agent_inbox.conversation_id,
		          agent_inbox.from_kind, agent_inbox.from_id,
		          agent_inbox.origin_channel, agent_inbox.origin_user_id,
		          agent_inbox.origin_thread_id, agent_inbox.origin_chat_id,
		          agent_inbox.external_msg_id, agent_inbox.content,
		          agent_inbox.attempts, agent_inbox.max_attempts, agent_inbox.enqueued_at
	`, agentID, limit, workerID, lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claim inbox: %w", err)
	}
	defer rows.Close()

	var out []InboxRow
	for rows.Next() {
		var r InboxRow
		if err := rows.Scan(&r.ID, &r.AgentID, &r.ConversationID,
			&r.FromKind, &r.FromID,
			&r.OriginChannel, &r.OriginUserID, &r.OriginThreadID, &r.OriginChatID,
			&r.ExternalMsgID, &r.Content,
			&r.Attempts, &r.MaxAttempts, &r.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("scan inbox row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AckInbox marks a claimed inbox row as processed.
func AckInbox(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE agent_inbox
		SET status = 'processed', processed_at = NOW(),
		    claimed_by = NULL, claimed_at = NULL, lease_expires = NULL
		WHERE id = $1 AND status = 'processing'
	`, id)
	if err != nil {
		return fmt.Errorf("ack inbox: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ack inbox %s: no row in 'processing' state", id)
	}
	return nil
}

// FailInbox records an error on a claimed row. If attempts exceed max_attempts
// the row transitions to 'dead'; otherwise it returns to 'pending' for retry.
func FailInbox(ctx context.Context, tx pgx.Tx, id uuid.UUID, errText string) error {
	_, err := tx.Exec(ctx, `
		UPDATE agent_inbox
		SET last_error = $2,
		    status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
		    claimed_by = NULL, claimed_at = NULL, lease_expires = NULL
		WHERE id = $1
	`, id, errText)
	if err != nil {
		return fmt.Errorf("fail inbox: %w", err)
	}
	return nil
}

// OutboxMessage is the insert payload for AppendOutbox.
type OutboxMessage struct {
	AgentID        string
	ConversationID *uuid.UUID
	InReplyTo      *uuid.UUID
	Content        []byte // JSON-encoded {parts:[...]}
	Mentions       []byte // JSON-encoded array; nil → '[]'
}

// AppendOutbox inserts a row into agent_outbox. Returns the new id.
func AppendOutbox(ctx context.Context, tx pgx.Tx, m OutboxMessage) (uuid.UUID, error) {
	if m.AgentID == "" {
		return uuid.Nil, errors.New("agent_id required")
	}
	if len(m.Content) == 0 {
		return uuid.Nil, errors.New("content required")
	}
	if len(m.Mentions) == 0 {
		m.Mentions = []byte("[]")
	}
	id := uuid.New()
	_, err := tx.Exec(ctx, `
		INSERT INTO agent_outbox (id, agent_id, conversation_id, in_reply_to, content, mentions)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, m.AgentID, m.ConversationID, m.InReplyTo, m.Content, m.Mentions)
	if err != nil {
		return uuid.Nil, fmt.Errorf("append outbox: %w", err)
	}
	return id, nil
}

// OutboxRow describes a claimed outbox row.
type OutboxRow struct {
	ID             uuid.UUID
	AgentID        string
	ConversationID *uuid.UUID
	InReplyTo      *uuid.UUID
	Content        []byte
	Mentions       []byte
	Attempts       int
	CreatedAt      time.Time
}

// ClaimOutbox flips up to `limit` pending outbox rows to 'routing' and returns
// them. Protected by FOR UPDATE SKIP LOCKED.
func ClaimOutbox(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := tx.Query(ctx, `
		WITH picked AS (
			SELECT id FROM agent_outbox
			WHERE status = 'pending'
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE agent_outbox
		SET status = 'routing', attempts = agent_outbox.attempts + 1
		FROM picked
		WHERE agent_outbox.id = picked.id
		RETURNING agent_outbox.id, agent_outbox.agent_id, agent_outbox.conversation_id,
		          agent_outbox.in_reply_to, agent_outbox.content, agent_outbox.mentions,
		          agent_outbox.attempts, agent_outbox.created_at
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()

	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.ID, &r.AgentID, &r.ConversationID, &r.InReplyTo,
			&r.Content, &r.Mentions, &r.Attempts, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AckChannelDelivery marks a channel_deliveries row as sent with the returned
// external (channel-side) message id.
func AckChannelDelivery(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalMsgID int64) error {
	tag, err := tx.Exec(ctx, `
		UPDATE channel_deliveries
		SET status = 'sent', sent_at = NOW(), external_msg_id = $2
		WHERE id = $1
	`, id, externalMsgID)
	if err != nil {
		return fmt.Errorf("ack channel delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ack channel delivery %s: not found", id)
	}
	return nil
}

// AttachmentTarget identifies which message owns the attachment.
type AttachmentTarget struct {
	InboxID  *uuid.UUID
	OutboxID *uuid.UUID
}

// InsertAttachment writes an attachment row. Payloads ≥ LargeObjectThreshold
// bypass BYTEA and go to a server-side large object; smaller payloads use
// inline BYTEA storage.
func InsertAttachment(ctx context.Context, tx pgx.Tx, target AttachmentTarget, name, mime string, content []byte) (uuid.UUID, error) {
	if (target.InboxID == nil) == (target.OutboxID == nil) {
		return uuid.Nil, errors.New("exactly one of InboxID or OutboxID must be set")
	}

	id := uuid.New()
	if len(content) < LargeObjectThreshold {
		_, err := tx.Exec(ctx, `
			INSERT INTO message_attachments (id, inbox_id, outbox_id, name, mime_type, size_bytes, content)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, id, target.InboxID, target.OutboxID, name, mime, len(content), content)
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert attachment (inline): %w", err)
		}
		return id, nil
	}

	lo := tx.LargeObjects()
	oid, err := lo.Create(ctx, 0)
	if err != nil {
		return uuid.Nil, fmt.Errorf("lo_create: %w", err)
	}
	obj, err := lo.Open(ctx, oid, pgx.LargeObjectModeWrite)
	if err != nil {
		return uuid.Nil, fmt.Errorf("lo_open: %w", err)
	}
	if _, err := obj.Write(content); err != nil {
		return uuid.Nil, fmt.Errorf("lo write: %w", err)
	}
	if err := obj.Close(); err != nil {
		return uuid.Nil, fmt.Errorf("lo close: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO message_attachments (id, inbox_id, outbox_id, name, mime_type, size_bytes, large_object_oid)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, id, target.InboxID, target.OutboxID, name, mime, len(content), oid)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert attachment (lo): %w", err)
	}
	return id, nil
}
