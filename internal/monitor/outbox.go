package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// NewDBOutboxWriter returns an OutboxWriter that mirrors each captured
// assistant response into agent_outbox via the typed mailbox wrappers.
// Writes are best-effort: a failed write is logged but does not interrupt
// the legacy Telegram delivery path (the new path is passive under the
// mailbox.outbound flag).
//
// OutboxEvent.AgentID arrives as the tmux window id (e.g. "@25") — the
// monitor has no direct handle to the logical agent id. Resolve it to
// the agents.id via tmux_window before writing; otherwise the insert
// violates the FK.
func NewDBOutboxWriter(pool *pgxpool.Pool) OutboxWriter {
	return func(e OutboxEvent) {
		if pool == nil || e.AgentID == "" || e.Text == "" {
			return
		}
		ctx := context.Background()

		agentID, err := resolveAgentFromWindow(ctx, pool, e.AgentID)
		if err != nil {
			log.Printf("outbox writer: resolve agent for window %s: %v", e.AgentID, err)
			return
		}
		if agentID == "" {
			// Unbound window — nothing to mirror.
			return
		}

		content, err := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Role string `json:"role,omitempty"`
		}{Type: "text", Text: e.Text, Role: e.Role})
		if err != nil {
			log.Printf("outbox writer: marshal: %v", err)
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			log.Printf("outbox writer: begin: %v", err)
			return
		}
		defer tx.Rollback(ctx)

		if _, err := mailbox.AppendOutbox(ctx, tx, mailbox.OutboxMessage{
			AgentID: agentID,
			Content: content,
		}); err != nil {
			log.Printf("outbox writer: append: %v", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			log.Printf("outbox writer: commit: %v", err)
		}
	}
}

// resolveAgentFromWindow returns the agents.id whose tmux_window matches
// the given window id. If the input already matches an agents.id
// directly, return it unchanged (tests / legacy callers pass the real id).
// Returns "" with no error when nothing is found — callers silently skip.
func resolveAgentFromWindow(ctx context.Context, pool *pgxpool.Pool, windowOrID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		SELECT id FROM agents
		WHERE tmux_window = $1 OR id = $1
		ORDER BY (id = $1) DESC
		LIMIT 1
	`, windowOrID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}
