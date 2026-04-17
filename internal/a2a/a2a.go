// Package a2a layers synchronous request/response on top of the
// mailbox. Async fire-and-forget via outbox mentions stays in
// internal/relay; this package is the "block until a reply lands"
// surface per plans/active/agent-to-agent-communication.md §Phase 3.
package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the minimal pgx surface this package touches — satisfied by
// *pgxpool.Pool as well as a wrapped pgx.Tx in tests.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (commandTag, error)
}

// commandTag is a subset of pgconn.CommandTag to avoid pulling pgconn
// into user code unnecessarily. The interface ship-works with
// *pgxpool.Pool whose Exec returns pgconn.CommandTag (which satisfies).
type commandTag interface {
	RowsAffected() int64
}

// ErrTimeout means AskAgent waited longer than the caller's budget for
// a reply. Callers can distinguish this from other errors via errors.Is.
var ErrTimeout = errors.New("a2a: timed out waiting for reply")

// ErrUnknownAgent is returned when `to` doesn't resolve to any existing
// agent (same semantics as the routing / handle resolver).
var ErrUnknownAgent = errors.New("a2a: no agent with that id or handle")

// Reply carries the answer text plus conversation bookkeeping so
// callers can chain into follow-up turns.
type Reply struct {
	Text           string
	ConversationID uuid.UUID
	OutboxID       uuid.UUID
	InboxID        uuid.UUID
}

// AskAgent sends `question` from agent `from` to agent `to` and blocks
// for up to `timeout` waiting for the target agent to produce a reply.
// Under the hood it:
//
//  1. Resolves `to` as id or handle.
//  2. Finds-or-creates an a2a conversations row with participants
//     {from, to}, kind='a2a'.
//  3. Inserts a fresh agent_inbox row for `to` on that conversation
//     (from_kind='agent', origin_channel='a2a:sync').
//  4. Polls agent_outbox for a row from `to` with in_reply_to equal to
//     the inserted inbox id (or any outbox carrying the same
//     conversation id issued after the request, as fallback) until
//     timeout.
//
// Timeout=0 defaults to 2 minutes; callers with long-running peers
// should pass a longer one.
func AskAgent(ctx context.Context, pool *pgxpool.Pool, from, to, question string, timeout time.Duration) (*Reply, error) {
	if pool == nil {
		return nil, errors.New("a2a: nil pool")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	// Bound the overall call with timeout.
	askCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	canonical, err := resolveAgentToken(askCtx, pool, to)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	if canonical == "" {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAgent, to)
	}

	tx, err := pool.Begin(askCtx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(askCtx)

	// Find or create an a2a conversation for {from, canonical}.
	convoID, err := ensureA2AConversation(askCtx, tx, from, canonical)
	if err != nil {
		return nil, err
	}

	// Insert the request into the target's inbox.
	var inboxID uuid.UUID
	content, _ := json.Marshal(map[string]string{"type": "text", "text": question})
	externalMsgID := "ask:" + uuid.New().String()
	err = tx.QueryRow(askCtx, `
		INSERT INTO agent_inbox
			(agent_id, conversation_id, from_kind, from_id,
			 origin_channel, external_msg_id, content)
		VALUES ($1, $2, 'agent', $3, 'a2a:sync', $4, $5::jsonb)
		RETURNING id
	`, canonical, convoID, from, externalMsgID, content).Scan(&inboxID)
	if err != nil {
		return nil, fmt.Errorf("insert inbox: %w", err)
	}
	if err := tx.Commit(askCtx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Poll the outbox for a reply. Matches on in_reply_to=inboxID first
	// (most precise); falls back to the next outbox row from `to` on
	// this conversation after the insert time, which handles cases
	// where the target emits a reply without threading in_reply_to.
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w (after %s)", ErrTimeout, timeout)
		}
		rep, err := pollReply(askCtx, pool, canonical, convoID, inboxID)
		if err != nil {
			return nil, err
		}
		if rep != nil {
			return rep, nil
		}
		select {
		case <-askCtx.Done():
			return nil, fmt.Errorf("%w: %v", ErrTimeout, askCtx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func pollReply(ctx context.Context, pool *pgxpool.Pool, fromAgent string, convoID, inboxID uuid.UUID) (*Reply, error) {
	var outID uuid.UUID
	var rawContent []byte
	err := pool.QueryRow(ctx, `
		SELECT id, content FROM agent_outbox
		WHERE agent_id = $1
		  AND (in_reply_to = $2
		       OR conversation_id = $3)
		  AND created_at > (SELECT enqueued_at FROM agent_inbox WHERE id = $2)
		ORDER BY created_at ASC
		LIMIT 1
	`, fromAgent, inboxID, convoID).Scan(&outID, &rawContent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("poll reply: %w", err)
	}
	var body struct {
		Text  string `json:"text"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	_ = json.Unmarshal(rawContent, &body)
	text := body.Text
	if text == "" {
		for _, p := range body.Parts {
			if p.Type == "text" && p.Text != "" {
				text += p.Text + "\n"
			}
		}
	}
	return &Reply{
		Text:           text,
		ConversationID: convoID,
		OutboxID:       outID,
		InboxID:        inboxID,
	}, nil
}

func resolveAgentToken(ctx context.Context, pool *pgxpool.Pool, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	var id string
	err := pool.QueryRow(ctx, `
		SELECT id FROM agents
		WHERE id = $1 OR LOWER(handle) = LOWER($1)
		LIMIT 1
	`, token).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ensureA2AConversation mirrors the relay's helper: find an open
// kind='a2a' conversation whose participants are exactly {from, to};
// else insert a fresh one. Kept here as a package-local so a2a does
// not depend on internal/relay.
func ensureA2AConversation(ctx context.Context, tx pgx.Tx, from, to string) (uuid.UUID, error) {
	var existing uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM conversations
		WHERE kind = 'a2a'
		  AND closed_at IS NULL
		  AND participants @> ARRAY[$1, $2]::text[]
		  AND participants <@ ARRAY[$1, $2]::text[]
		ORDER BY created_at DESC
		LIMIT 1
	`, from, to).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("lookup a2a convo: %w", err)
	}

	var newID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO conversations
			(kind, participants, pending_count)
		VALUES ('a2a', ARRAY[$1, $2]::text[], 0)
		RETURNING id
	`, from, to).Scan(&newID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert a2a convo: %w", err)
	}
	return newID, nil
}
