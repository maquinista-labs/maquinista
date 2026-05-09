// Package inboxecho implements the inbox echo pipeline: non-Telegram inbox
// rows (origin_channel='dashboard', and future 'slack'/'discord') are fanned
// out to inbox_echoes (one row per owner binding per channel) so that each
// channel's dispatcher can deliver the user's message alongside the agent's
// reply. This mirrors the relay→dispatcher pattern used for agent_outbox.
package inboxecho

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run subscribes to agent_inbox_new and fans out eligible inbox rows to
// inbox_echoes. Exits when ctx is cancelled.
func Run(ctx context.Context, pool *pgxpool.Pool, workerID string) error {
	listener, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listener: %w", err)
	}
	defer listener.Release()

	if _, err := listener.Exec(ctx, "LISTEN agent_inbox_new"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	for {
		for {
			processed, err := ProcessOne(ctx, pool, workerID)
			if err != nil {
				log.Printf("inbox_echo fanout: %v", err)
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

// ProcessOne claims a single unprocessed inbox row (origin_channel not in
// 'telegram'/'a2a') and inserts inbox_echoes rows for every owner binding.
// Returns (true, nil) when a row was processed, (false, nil) when none pending.
func ProcessOne(ctx context.Context, pool *pgxpool.Pool, _ string) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		id      uuid.UUID
		agentID string
	)
	err = tx.QueryRow(ctx, `
		SELECT id, agent_id
		FROM agent_inbox
		WHERE NOT echo_processed
		  AND origin_channel NOT IN ('telegram', 'a2a')
		ORDER BY enqueued_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&id, &agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim inbox row: %w", err)
	}

	// Mark processed before releasing the lock so a concurrent worker
	// won't pick the same row while we're inserting echoes.
	if _, err := tx.Exec(ctx, `
		UPDATE agent_inbox SET echo_processed = TRUE WHERE id = $1
	`, id); err != nil {
		return false, fmt.Errorf("mark echo_processed: %w", err)
	}

	// Fan out to every Telegram owner binding. Adding a new channel
	// (Slack, Discord) means adding a new SELECT leg here and a new
	// dispatcher — no structural changes needed.
	if _, err := tx.Exec(ctx, `
		INSERT INTO inbox_echoes (inbox_id, channel, user_id, thread_id, chat_id)
		SELECT $1, 'telegram', b.user_id, b.thread_id, b.chat_id
		FROM topic_agent_bindings b
		WHERE b.agent_id     = $2
		  AND b.binding_type = 'owner'
		  AND b.user_id      IS NOT NULL
		  AND b.chat_id      IS NOT NULL
		ON CONFLICT (inbox_id, channel, COALESCE(thread_id, '')) DO NOTHING
	`, id, agentID); err != nil {
		return false, fmt.Errorf("fanout echoes: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}
