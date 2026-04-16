// Package relay implements the outbox relay daemon from
// plans/reference/maquinista-v2.md §8.2. It claims rows from agent_outbox, fans them
// out to channel_deliveries (origin topic + owner/observer bindings), turns
// `[@agent_id: text]` mentions into follow-up agent_inbox rows, and marks
// the outbox row as routed — all inside one transaction so a crash leaves
// the row exactly as it was before the claim.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run starts the relay loop. It subscribes to NOTIFY agent_outbox_new and,
// on each wake (or a 10 s poll fallback), drains all pending outbox rows.
// The loop exits when ctx is cancelled.
func Run(ctx context.Context, pool *pgxpool.Pool, workerID string) error {
	listener, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listener: %w", err)
	}
	defer listener.Release()

	if _, err := listener.Exec(ctx, "LISTEN agent_outbox_new"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	for {
		// Drain anything queued up before blocking on the next notify.
		for {
			processed, err := ProcessOne(ctx, pool, workerID)
			if err != nil {
				log.Printf("relay: process: %v", err)
				break
			}
			if !processed {
				break
			}
		}

		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, notifyErr := listener.Conn().WaitForNotification(waitCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if notifyErr != nil && !errors.Is(notifyErr, context.DeadlineExceeded) {
			return fmt.Errorf("wait notify: %w", notifyErr)
		}
	}
}

// ProcessOne claims a single pending outbox row and routes it. Returns
// (true, nil) if a row was processed, (false, nil) if the outbox was empty.
func ProcessOne(ctx context.Context, pool *pgxpool.Pool, workerID string) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		id             uuid.UUID
		agentID        string
		conversationID *uuid.UUID
		inReplyTo      *uuid.UUID
		content        []byte
		mentionsJSON   []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT id, agent_id, conversation_id, in_reply_to, content, mentions
		FROM agent_outbox
		WHERE status = 'pending'
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&id, &agentID, &conversationID, &inReplyTo, &content, &mentionsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim outbox: %w", err)
	}

	_, err = tx.Exec(ctx, `UPDATE agent_outbox SET status='routing' WHERE id=$1`, id)
	if err != nil {
		return false, fmt.Errorf("mark routing: %w", err)
	}

	if err := fanoutDeliveries(ctx, tx, id, agentID, inReplyTo); err != nil {
		return false, fmt.Errorf("fanout: %w", err)
	}

	mentions := mergeMentions(mentionsJSON, content)
	inserted, err := enqueueMentions(ctx, tx, id, agentID, conversationID, mentions)
	if err != nil {
		return false, fmt.Errorf("enqueue mentions: %w", err)
	}
	if inserted > 0 && conversationID != nil {
		_, err = tx.Exec(ctx, `
			UPDATE conversations SET pending_count = pending_count + $2 WHERE id = $1
		`, *conversationID, inserted)
		if err != nil {
			return false, fmt.Errorf("conversation bump: %w", err)
		}
	}

	_, err = tx.Exec(ctx, `
		UPDATE agent_outbox SET status='routed', routed_at=NOW() WHERE id=$1
	`, id)
	if err != nil {
		return false, fmt.Errorf("mark routed: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// fanoutDeliveries implements §8.2: one row for the origin topic of the
// triggering inbox message (if any), plus one row per owner/observer
// binding. The UNIQUE (outbox_id, channel, user_id, thread_id) constraint
// collapses duplicates when the origin is also a subscriber.
func fanoutDeliveries(ctx context.Context, tx pgx.Tx, outboxID uuid.UUID, agentID string, inReplyTo *uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO channel_deliveries
			(outbox_id, channel, user_id, thread_id, chat_id, binding_type)
		SELECT $1::uuid, 'telegram', i.origin_user_id, i.origin_thread_id,
		       i.origin_chat_id, 'origin'
		FROM agent_inbox i
		WHERE i.id = $2::uuid
		  AND i.origin_channel = 'telegram'
		  AND i.origin_user_id  IS NOT NULL
		  AND i.origin_thread_id IS NOT NULL
		  AND i.origin_chat_id  IS NOT NULL
		UNION
		SELECT $1::uuid, 'telegram', b.user_id, b.thread_id, b.chat_id, b.binding_type
		FROM topic_agent_bindings b
		WHERE b.agent_id = $3
		  AND b.binding_type IN ('owner','observer')
		  AND b.user_id  IS NOT NULL
		  AND b.chat_id  IS NOT NULL
		ON CONFLICT (outbox_id, channel, user_id, thread_id) DO NOTHING
	`, outboxID, inReplyTo, agentID)
	return err
}

func mergeMentions(mentionsJSON, content []byte) []Mention {
	var out []Mention
	// Stored mentions take precedence (the sidecar may have parsed them
	// with richer context than this simple bracket matcher).
	if len(mentionsJSON) > 0 && string(mentionsJSON) != "[]" && string(mentionsJSON) != "null" {
		var raw []struct {
			AgentID string `json:"agent_id"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(mentionsJSON, &raw); err == nil {
			for _, m := range raw {
				if m.AgentID != "" {
					out = append(out, Mention{AgentID: m.AgentID, Text: m.Text})
				}
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	// Fall back to parsing the content text.
	var body struct {
		Text  string `json:"text"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return nil
	}
	if body.Text != "" {
		out = append(out, ParseMentions(body.Text)...)
	}
	for _, p := range body.Parts {
		if p.Type == "text" && p.Text != "" {
			out = append(out, ParseMentions(p.Text)...)
		}
	}
	return out
}

// enqueueMentions resolves each mention token to a canonical agent id
// (agents.id OR agents.handle, case-insensitive), drops self-mentions and
// tokens that don't match any agent, then inserts one agent_inbox row per
// target. Idempotent via the (origin_channel, external_msg_id) unique
// index: origin_channel='a2a' + external_msg_id='<outbox_id>:<target>' so
// a crash-restart of the relay doesn't double-deliver.
//
// Phase 1 of plans/active/agent-to-agent-communication.md.
func enqueueMentions(
	ctx context.Context,
	tx pgx.Tx,
	outboxID uuid.UUID,
	fromAgent string,
	conversationID *uuid.UUID,
	mentions []Mention,
) (int, error) {
	inserted := 0
	for _, m := range mentions {
		canonical, err := resolveAgentToken(ctx, tx, m.AgentID)
		if err != nil {
			return inserted, fmt.Errorf("resolve mention %q: %w", m.AgentID, err)
		}
		if canonical == "" {
			// Silently skip — mention of a non-existent agent (typo,
			// agent archived/deleted) should not FK-fail the whole relay.
			continue
		}
		if canonical == fromAgent {
			// Skip self-mention; keeps replies from looping back to the
			// producer.
			continue
		}
		content, _ := json.Marshal(map[string]string{"type": "text", "text": m.Text})
		externalMsgID := outboxID.String() + ":" + canonical
		tag, err := tx.Exec(ctx, `
			INSERT INTO agent_inbox
				(agent_id, conversation_id, from_kind, from_id,
				 origin_channel, external_msg_id, content)
			VALUES ($1, $2, 'agent', $3, 'a2a', $4, $5::jsonb)
			ON CONFLICT (origin_channel, external_msg_id) DO NOTHING
		`, canonical, conversationID, fromAgent, externalMsgID, content)
		if err != nil {
			return inserted, fmt.Errorf("insert mention for %s: %w", canonical, err)
		}
		if tag.RowsAffected() > 0 {
			inserted++
		}
	}
	return inserted, nil
}

// resolveAgentToken returns the canonical agents.id for a token that may
// be an id or a handle (case-insensitive). Returns ("", nil) if no match.
// Local copy of internal/routing.ResolveAgentByToken so the relay avoids
// taking a dependency on the routing package.
func resolveAgentToken(ctx context.Context, tx pgx.Tx, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	var id string
	err := tx.QueryRow(ctx, `
		SELECT id FROM agents
		WHERE id = $1 OR LOWER(handle) = LOWER($1)
		LIMIT 1
	`, token).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}
