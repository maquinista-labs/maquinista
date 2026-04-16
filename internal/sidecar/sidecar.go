// Package sidecar is the per-agent supervisor from plans/reference/maquinista-v2.md §7.
//
// A SidecarRunner owns two things for exactly one agent:
//   - the pty driver: consumes agent_inbox rows, pipes content into the pty,
//     acks or fails the row based on the driver's return.
//   - the JSONL transcript tail: observes the runner's output, and inserts one
//     agent_outbox row per completed assistant response.
//
// Both concerns used to live in separate processes (the bot's in-process
// bridge from task 1.6 and the monitor from task 1.5). A sidecar collapses
// them into a single goroutine per live agent so claim/lease state stays
// coherent across crash and restart (a crashed sidecar leaves rows in
// 'processing'; lease expiry on the next tick reclaims them exactly once).
//
// The monitor's standalone process is kept for parity-validation during the
// task-1.5 ↔ task-1.7 rollout; task 1.9 retires it.
package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// PtyDriver pipes `text` into the agent's interactive pty. The input is a
// full user turn; the driver is responsible for chunking large inputs and
// appending an Enter keystroke.
type PtyDriver interface {
	Drive(ctx context.Context, text string) error
}

// PtyDriverFunc adapts a function to PtyDriver.
type PtyDriverFunc func(ctx context.Context, text string) error

func (f PtyDriverFunc) Drive(ctx context.Context, text string) error { return f(ctx, text) }

// TranscriptEvent is one observed turn emitted by the runner. Only assistant
// text + thinking entries become outbox rows (matching task 1.5's gate so
// parity is byte-for-byte).
type TranscriptEvent struct {
	Role    string // 'assistant' | 'user' | …
	Kind    string // 'text' | 'thinking' | 'tool_use' | 'tool_result'
	Text    string
	TurnEnd bool // true on the final assistant event of a turn
}

// TranscriptTailer streams observed transcript events until ctx is cancelled
// or the tailer errors. Closes ch on exit.
type TranscriptTailer interface {
	Tail(ctx context.Context, ch chan<- TranscriptEvent) error
}

// Config bundles sidecar knobs.
type Config struct {
	AgentID    string
	WorkerID   string
	Lease      time.Duration
	Poll       time.Duration
}

// DefaultConfig returns production defaults.
func DefaultConfig(agentID string) Config {
	return Config{
		AgentID:  agentID,
		WorkerID: fmt.Sprintf("sidecar-%s-%s", agentID, uuid.New().String()[:8]),
		Lease:    5 * time.Minute,
		Poll:     10 * time.Second,
	}
}

// SidecarRunner drives one agent's inbox/outbox loop.
type SidecarRunner struct {
	pool    *pgxpool.Pool
	cfg     Config
	driver  PtyDriver
	tailer  TranscriptTailer
}

// New constructs a sidecar. Both driver and tailer are required.
func New(pool *pgxpool.Pool, cfg Config, driver PtyDriver, tailer TranscriptTailer) *SidecarRunner {
	return &SidecarRunner{pool: pool, cfg: cfg, driver: driver, tailer: tailer}
}

// Run blocks until ctx is cancelled. Spawns the transcript tailer as a
// goroutine and drives the inbox claim/drive/ack loop on the main goroutine.
func (s *SidecarRunner) Run(ctx context.Context) error {
	if s.driver == nil || s.tailer == nil {
		return errors.New("sidecar: driver and tailer required")
	}

	events := make(chan TranscriptEvent, 32)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.tailer.Tail(ctx, events); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("sidecar %s: tailer: %v", s.cfg.AgentID, err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.consumeTranscript(ctx, events)
	}()

	inboxErr := s.runInboxLoop(ctx)
	wg.Wait()
	return inboxErr
}

func (s *SidecarRunner) runInboxLoop(ctx context.Context) error {
	listener, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN agent_inbox_new"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	for {
		// Drain all eligible rows for this agent.
		for {
			processed, perr := s.processOneInbox(ctx)
			if perr != nil {
				log.Printf("sidecar %s: inbox: %v", s.cfg.AgentID, perr)
				break
			}
			if !processed {
				break
			}
		}
		waitCtx, cancel := context.WithTimeout(ctx, s.cfg.Poll)
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

func (s *SidecarRunner) processOneInbox(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	rows, err := mailbox.ClaimInbox(ctx, tx, s.cfg.AgentID, s.cfg.WorkerID, s.cfg.Lease, 1)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	row := rows[0]

	text := extractInboxText(row.Content)
	driveErr := s.driver.Drive(ctx, text)

	tx2, err := s.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx2.Rollback(ctx)
	if driveErr != nil {
		if ferr := mailbox.FailInbox(ctx, tx2, row.ID, driveErr.Error()); ferr != nil {
			return true, ferr
		}
	} else if aerr := mailbox.AckInbox(ctx, tx2, row.ID); aerr != nil {
		log.Printf("sidecar %s: ack %s: %v", s.cfg.AgentID, row.ID, aerr)
	}
	return true, tx2.Commit(ctx)
}

func (s *SidecarRunner) consumeTranscript(ctx context.Context, events <-chan TranscriptEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Role != "assistant" {
				continue
			}
			if ev.Kind != "text" && ev.Kind != "thinking" {
				continue
			}
			if ev.Text == "" {
				continue
			}
			s.appendOutbox(ctx, ev)
		}
	}
}

func (s *SidecarRunner) appendOutbox(ctx context.Context, ev TranscriptEvent) {
	content, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Role string `json:"role,omitempty"`
	}{Type: "text", Text: ev.Text, Role: ev.Role})
	if err != nil {
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Printf("sidecar %s: outbox begin: %v", s.cfg.AgentID, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err := mailbox.AppendOutbox(ctx, tx, mailbox.OutboxMessage{
		AgentID: s.cfg.AgentID,
		Content: content,
	}); err != nil {
		log.Printf("sidecar %s: outbox append: %v", s.cfg.AgentID, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("sidecar %s: outbox commit: %v", s.cfg.AgentID, err)
	}
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
