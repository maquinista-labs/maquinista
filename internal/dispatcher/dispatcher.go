// Package dispatcher implements the Telegram channel dispatcher from
// plans/maquinista-v2.md §8.3. It claims pending channel_deliveries rows,
// renders agent_outbox.content into Telegram text, sends via the Bot API,
// and flips the row to 'sent' (success), 'pending+next_attempt_at' (429),
// or 'failed' (exhausted attempts).
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TelegramClient is the narrow interface the dispatcher needs. The real
// implementation wraps tgbotapi.BotAPI; tests inject a mock.
type TelegramClient interface {
	SendMessage(ctx context.Context, chatID int64, threadID int, text string) (externalMsgID int64, err error)
}

// RateLimitError is the 429 case. RetryAfter is the server-specified cooldown.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("telegram: rate-limited, retry after %s", e.RetryAfter)
}

// Config bundles dispatcher knobs.
type Config struct {
	// MaxAttempts is the cap before a delivery is marked 'failed'.
	MaxAttempts int
	// RatePerSec caps global send rate. Telegram's global ceiling is 30 msg/s;
	// staying below that avoids flood errors for the whole bot.
	RatePerSec int
	// DefaultRateLimitBackoff is used when a 429 lacks retry_after.
	DefaultRateLimitBackoff time.Duration
	// Shadow, when true, claims rows and flips them to 'sent' with a null
	// external_msg_id — but never calls TelegramClient.SendMessage. Used
	// during rollout so channel_deliveries accumulates alongside the legacy
	// Telegram path without double-sending. Mapped from MAILBOX_DISPATCHER
	// (off = shadow, on = live) at the callsite.
	Shadow bool
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		MaxAttempts:             5,
		RatePerSec:              25, // 5-msg headroom below Telegram's 30/s global limit
		DefaultRateLimitBackoff: 30 * time.Second,
	}
}

// Run drives the dispatcher loop until ctx is cancelled. It subscribes to
// NOTIFY channel_delivery_new and, on each wake, drains eligible rows.
func Run(ctx context.Context, pool *pgxpool.Pool, client TelegramClient, cfg Config) error {
	listener, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listener: %w", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN channel_delivery_new"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	limiter := newTokenBucket(cfg.RatePerSec)

	for {
		for {
			processed, perr := ProcessOne(ctx, pool, client, cfg, limiter)
			if perr != nil {
				log.Printf("dispatcher: %v", perr)
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

// ProcessOne handles at most one delivery. Returns (true, nil) when a row was
// handled (success, deferred, or failed), (false, nil) when no work was
// available. Errors are returned only for unexpected failures (DB issues).
func ProcessOne(ctx context.Context, pool *pgxpool.Pool, client TelegramClient, cfg Config, limiter *tokenBucket) (bool, error) {
	type claim struct {
		id         uuid.UUID
		chatID     int64
		threadIDStr string
		threadID   int
		text       string
		attempts   int
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var c claim
	var rawContent []byte
	err = tx.QueryRow(ctx, `
		WITH picked AS (
			SELECT d.id
			FROM channel_deliveries d
			WHERE d.status = 'pending'
			  AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= NOW())
			ORDER BY COALESCE(d.next_attempt_at, d.created_at)
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE channel_deliveries
		SET status='sending', attempts = channel_deliveries.attempts + 1
		FROM picked, agent_outbox o
		WHERE channel_deliveries.id = picked.id
		  AND o.id = channel_deliveries.outbox_id
		RETURNING channel_deliveries.id, channel_deliveries.chat_id,
		          channel_deliveries.thread_id, channel_deliveries.attempts,
		          o.content
	`).Scan(&c.id, &c.chatID, &c.threadIDStr, &c.attempts, &rawContent)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim delivery: %w", err)
	}
	c.text = renderContent(rawContent)
	if c.threadIDStr != "" {
		if _, scanErr := fmt.Sscanf(c.threadIDStr, "%d", &c.threadID); scanErr != nil {
			c.threadID = 0
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit claim: %w", err)
	}

	if cfg.Shadow {
		// Shadow mode: skip the network call but still mark 'sent' so rows
		// don't pile up as 'sending'. external_msg_id is left NULL so a
		// later parity check can tell shadow rows from live ones.
		_, uerr := pool.Exec(ctx, `
			UPDATE channel_deliveries
			SET status='sent', sent_at=NOW(), external_msg_id=NULL, next_attempt_at=NULL
			WHERE id=$1
		`, c.id)
		return true, uerr
	}

	if limiter != nil {
		limiter.Wait(ctx)
	}

	msgID, sendErr := client.SendMessage(ctx, c.chatID, c.threadID, c.text)
	if sendErr == nil {
		_, uerr := pool.Exec(ctx, `
			UPDATE channel_deliveries
			SET status='sent', sent_at=NOW(), external_msg_id=$2, next_attempt_at=NULL
			WHERE id=$1
		`, c.id, msgID)
		if uerr != nil {
			return true, fmt.Errorf("mark sent: %w", uerr)
		}
		return true, nil
	}

	// 429 → defer.
	var rl *RateLimitError
	if errors.As(sendErr, &rl) {
		backoff := rl.RetryAfter
		if backoff <= 0 {
			backoff = cfg.DefaultRateLimitBackoff
		}
		_, uerr := pool.Exec(ctx, `
			UPDATE channel_deliveries
			SET status='pending',
			    next_attempt_at = NOW() + ($2 * INTERVAL '1 millisecond'),
			    last_error = $3
			WHERE id=$1
		`, c.id, backoff.Milliseconds(), sendErr.Error())
		return true, uerr
	}

	// Other error: fail terminally if attempts exhausted, else bounce back
	// to 'pending' for another retry.
	max := cfg.MaxAttempts
	if max <= 0 {
		max = 5
	}
	if c.attempts >= max {
		_, uerr := pool.Exec(ctx, `
			UPDATE channel_deliveries SET status='failed', last_error=$2 WHERE id=$1
		`, c.id, sendErr.Error())
		return true, uerr
	}
	_, uerr := pool.Exec(ctx, `
		UPDATE channel_deliveries SET status='pending', last_error=$2 WHERE id=$1
	`, c.id, sendErr.Error())
	return true, uerr
}

// renderContent projects a (possibly {parts:[]}) JSON blob into plain text.
// Unknown shapes fall back to the marshaled original so nothing is silently
// dropped.
func renderContent(raw []byte) string {
	var body struct {
		Text  string `json:"text"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return string(raw)
	}
	if body.Text != "" && len(body.Parts) == 0 {
		return body.Text
	}
	var b strings.Builder
	if body.Text != "" {
		b.WriteString(body.Text)
	}
	for _, p := range body.Parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	if b.Len() == 0 {
		return string(raw)
	}
	return b.String()
}
