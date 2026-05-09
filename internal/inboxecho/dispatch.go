package inboxecho

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

// TelegramClient is the narrow sending interface. *dispatcher.BotAPIClient
// satisfies it — pass the same instance used by the outbox dispatcher.
type TelegramClient interface {
	SendMessage(ctx context.Context, chatID int64, threadID int, text string) (externalMsgID int64, err error)
}

// DispatchConfig controls retry behaviour.
type DispatchConfig struct {
	MaxAttempts             int
	DefaultRateLimitBackoff time.Duration
}

// DefaultDispatchConfig returns conservative production defaults.
func DefaultDispatchConfig() DispatchConfig {
	return DispatchConfig{
		MaxAttempts:             5,
		DefaultRateLimitBackoff: 30 * time.Second,
	}
}

// RunDispatch subscribes to inbox_echo_new and drains pending inbox_echoes
// rows, sending each to Telegram prefixed with "👤 <sender>:". Exits when
// ctx is cancelled.
func RunDispatch(ctx context.Context, pool *pgxpool.Pool, client TelegramClient, cfg DispatchConfig) error {
	listener, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listener: %w", err)
	}
	defer listener.Release()

	if _, err := listener.Exec(ctx, "LISTEN inbox_echo_new"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	for {
		for {
			processed, err := DispatchOne(ctx, pool, client, cfg)
			if err != nil {
				log.Printf("inbox_echo dispatch: %v", err)
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

// DispatchOne claims a single pending inbox_echoes row and sends it.
func DispatchOne(ctx context.Context, pool *pgxpool.Pool, client TelegramClient, cfg DispatchConfig) (bool, error) {
	type claim struct {
		id          uuid.UUID
		chatID      int64
		threadIDStr string
		threadID    int
		attempts    int
		content     []byte
		originUser  *string
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var c claim
	err = tx.QueryRow(ctx, `
		WITH picked AS (
			SELECT e.id
			FROM inbox_echoes e
			WHERE e.status = 'pending'
			  AND (e.next_attempt_at IS NULL OR e.next_attempt_at <= NOW())
			ORDER BY COALESCE(e.next_attempt_at, e.created_at)
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE inbox_echoes
		SET status = 'sending', attempts = inbox_echoes.attempts + 1
		FROM picked, agent_inbox i
		WHERE inbox_echoes.id = picked.id
		  AND i.id             = inbox_echoes.inbox_id
		RETURNING inbox_echoes.id, inbox_echoes.chat_id,
		          inbox_echoes.thread_id, inbox_echoes.attempts,
		          i.content, i.origin_user_id
	`).Scan(&c.id, &c.chatID, &c.threadIDStr, &c.attempts, &c.content, &c.originUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim echo: %w", err)
	}
	if c.threadIDStr != "" {
		fmt.Sscanf(c.threadIDStr, "%d", &c.threadID)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit claim: %w", err)
	}

	text := renderEchoText(c.content, c.originUser)
	msgID, sendErr := client.SendMessage(ctx, c.chatID, c.threadID, text)
	if sendErr == nil {
		_, uerr := pool.Exec(ctx, `
			UPDATE inbox_echoes
			SET status='sent', sent_at=NOW(), external_msg_id=$2, next_attempt_at=NULL
			WHERE id=$1
		`, c.id, msgID)
		return true, uerr
	}

	max := cfg.MaxAttempts
	if max <= 0 {
		max = 5
	}
	if c.attempts >= max {
		_, uerr := pool.Exec(ctx, `
			UPDATE inbox_echoes SET status='failed', last_error=$2 WHERE id=$1
		`, c.id, sendErr.Error())
		return true, uerr
	}
	backoff := cfg.DefaultRateLimitBackoff
	_, uerr := pool.Exec(ctx, `
		UPDATE inbox_echoes
		SET status='pending',
		    next_attempt_at = NOW() + ($2 * INTERVAL '1 millisecond'),
		    last_error = $3
		WHERE id=$1
	`, c.id, backoff.Milliseconds(), sendErr.Error())
	return true, uerr
}

// renderEchoText formats a user's inbox message for Telegram display.
// Uses "👤 <sender>: <text>" so it's visually distinct from agent replies.
func renderEchoText(content []byte, originUser *string) string {
	var body struct {
		Text string `json:"text"`
	}
	text := string(content)
	if err := json.Unmarshal(content, &body); err == nil && body.Text != "" {
		text = body.Text
	}
	sender := "operator"
	if originUser != nil && *originUser != "" {
		sender = *originUser
	}
	return "👤 " + sender + ": " + text
}
