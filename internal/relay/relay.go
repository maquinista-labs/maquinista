// Package relay implements the outbox relay daemon from
// plans/maquinista-v2.md §8.2. It claims rows from agent_outbox, fans them
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
	inserted, err := enqueueMentions(ctx, tx, agentID, conversationID, mentions)
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

func enqueueMentions(ctx context.Context, tx pgx.Tx, fromAgent string, conversationID *uuid.UUID, mentions []Mention) (int, error) {
	inserted := 0
	for _, m := range mentions {
		content, _ := json.Marshal(map[string]string{"type": "text", "text": m.Text})
		tag, err := tx.Exec(ctx, `
			INSERT INTO agent_inbox
				(agent_id, conversation_id, from_kind, from_id, content)
			VALUES ($1, $2, 'agent', $3, $4::jsonb)
		`, m.AgentID, conversationID, fromAgent, content)
		if err != nil {
			return inserted, fmt.Errorf("insert mention for %s: %w", m.AgentID, err)
		}
		if tag.RowsAffected() > 0 {
			inserted++
		}
	}
	return inserted, nil
}
