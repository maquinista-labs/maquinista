// Package inboxbridge is the minimal in-process consumer of agent_inbox
// that drives the legacy tmux pty during the mailbox.inbound rollout
// (plans/maquinista-v2-implementation.md task 1.6). It owns no pty state
// — `ptyDriver` is the only escape hatch — and exits cleanly when ctx is
// cancelled. Once task 1.7 extracts the sidecar, this package goes away.
package inboxbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// PtyDriver sends content into an agent's pty. Agent IDs correspond to the
// tmux window name by existing convention (see internal/db/queries.go
// RegisterAgent). Returning a non-nil error causes the bridge to FailInbox
// the row so it can retry or die per attempts.
type PtyDriver func(agentID, text string) error

// Config bundles bridge knobs.
type Config struct {
	WorkerID      string
	Lease         time.Duration
	PollFallback  time.Duration
	MaxPerWake    int
}

// DefaultConfig returns production defaults.
func DefaultConfig(workerID string) Config {
	return Config{
		WorkerID:     workerID,
		Lease:        5 * time.Minute,
		PollFallback: 10 * time.Second,
		MaxPerWake:   8,
	}
}

// Run blocks driving the inbox loop until ctx is cancelled.
func Run(ctx context.Context, pool *pgxpool.Pool, drive PtyDriver, cfg Config) error {
	if drive == nil {
		return errors.New("inboxbridge: nil PtyDriver")
	}
	listener, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN agent_inbox_new"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	for {
		// Drain everything queued across all agents on this tick.
		if err := drainAll(ctx, pool, drive, cfg); err != nil {
			log.Printf("inboxbridge: drain: %v", err)
		}

		waitCtx, cancel := context.WithTimeout(ctx, cfg.PollFallback)
		_, nerr := listener.Conn().WaitForNotification(waitCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if nerr != nil && !errors.Is(nerr, context.DeadlineExceeded) {
			return fmt.Errorf("wait notify: %w", nerr)
		}
	}
}

// drainAll pulls the distinct set of agents with pending work and processes
// each agent's queue sequentially (a single pty can't parallelize turns).
func drainAll(ctx context.Context, pool *pgxpool.Pool, drive PtyDriver, cfg Config) error {
	agents, err := listPendingAgents(ctx, pool)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		for i := 0; i < cfg.MaxPerWake; i++ {
			processed, err := processOne(ctx, pool, agent, drive, cfg)
			if err != nil {
				log.Printf("inboxbridge: %s: %v", agent, err)
				break
			}
			if !processed {
				break
			}
		}
	}
	return nil
}

func listPendingAgents(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT agent_id FROM agent_inbox
		WHERE status='pending' OR (status='processing' AND lease_expires < NOW())
	`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func processOne(ctx context.Context, pool *pgxpool.Pool, agentID string, drive PtyDriver, cfg Config) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := mailbox.ClaimInbox(ctx, tx, agentID, cfg.WorkerID, cfg.Lease, 1)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit claim: %w", err)
	}
	row := rows[0]

	text := extractText(row.Content)
	driveErr := drive(row.AgentID, text)

	tx2, err := pool.Begin(ctx)
	if err != nil {
		return true, fmt.Errorf("begin ack: %w", err)
	}
	defer tx2.Rollback(ctx)

	if driveErr != nil {
		if err := mailbox.FailInbox(ctx, tx2, row.ID, driveErr.Error()); err != nil {
			return true, fmt.Errorf("fail inbox: %w", err)
		}
	} else if err := mailbox.AckInbox(ctx, tx2, row.ID); err != nil {
		// Could be a lease-lost race (reclaimed by another worker). Best-effort
		// log and move on — the other worker's ack wins.
		log.Printf("inboxbridge: ack %s: %v", row.ID, err)
	}
	if err := tx2.Commit(ctx); err != nil {
		return true, fmt.Errorf("commit ack: %w", err)
	}
	return true, nil
}

func extractText(content []byte) string {
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

