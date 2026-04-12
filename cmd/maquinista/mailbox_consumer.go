package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// runMailboxConsumer claims agent_inbox rows for every live agent and pipes
// their content into the matching tmux window via SendKeysWithDelay. It
// subscribes to NOTIFY agent_inbox_new with a 10 s poll fallback so empty
// periods don't chew CPU.
//
// This function replaces the task-1.6 internal/inboxbridge package
// (retired by task 1.9). The long-term plan is one sidecar goroutine per
// agent (plans/maquinista-v2.md §7); that wiring lives in a follow-up.
func runMailboxConsumer(ctx context.Context, pool *pgxpool.Pool, tmuxSession, workerID string) {
	listener, err := pool.Acquire(ctx)
	if err != nil {
		log.Printf("mailbox consumer: acquire: %v", err)
		return
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN agent_inbox_new"); err != nil {
		log.Printf("mailbox consumer: LISTEN: %v", err)
		return
	}

	for {
		if err := drainAllAgents(ctx, pool, tmuxSession, workerID); err != nil {
			log.Printf("mailbox consumer: drain: %v", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, nerr := listener.Conn().WaitForNotification(waitCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if nerr != nil && !errors.Is(nerr, context.DeadlineExceeded) {
			log.Printf("mailbox consumer: notify wait: %v", nerr)
			time.Sleep(time.Second) // back off transient listener errors
		}
	}
}

func drainAllAgents(ctx context.Context, pool *pgxpool.Pool, tmuxSession, workerID string) error {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT agent_id FROM agent_inbox
		WHERE status='pending' OR (status='processing' AND lease_expires < NOW())
	`)
	if err != nil {
		return err
	}
	var agents []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		agents = append(agents, a)
	}
	rows.Close()

	for _, agent := range agents {
		for i := 0; i < 8; i++ {
			processed, perr := consumeOne(ctx, pool, tmuxSession, workerID, agent)
			if perr != nil {
				log.Printf("mailbox consumer %s: %v", agent, perr)
				break
			}
			if !processed {
				break
			}
		}
	}
	return nil
}

func consumeOne(ctx context.Context, pool *pgxpool.Pool, tmuxSession, workerID, agentID string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	claimed, err := mailbox.ClaimInbox(ctx, tx, agentID, workerID, 5*time.Minute, 1)
	if err != nil {
		return false, err
	}
	if len(claimed) == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	row := claimed[0]
	text := extractInboxText(row.Content)
	driveErr := tmux.SendKeysWithDelay(tmuxSession, row.AgentID, text, 500)

	tx2, err := pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx2.Rollback(ctx)
	if driveErr != nil {
		if err := mailbox.FailInbox(ctx, tx2, row.ID, driveErr.Error()); err != nil {
			return true, err
		}
	} else if err := mailbox.AckInbox(ctx, tx2, row.ID); err != nil {
		log.Printf("mailbox consumer: ack %s: %v", row.ID, err)
	}
	return true, tx2.Commit(ctx)
}

func extractInboxText(content []byte) string {
	var body struct {
		Text  string `json:"text"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return string(content)
	}
	if body.Text != "" {
		return body.Text
	}
	for _, p := range body.Parts {
		if p.Type == "text" && p.Text != "" {
			return p.Text
		}
	}
	return string(content)
}
