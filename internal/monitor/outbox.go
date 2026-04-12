package monitor

import (
	"context"
	"encoding/json"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// NewDBOutboxWriter returns an OutboxWriter that mirrors each captured
// assistant response into agent_outbox via the typed mailbox wrappers.
// Writes are best-effort: a failed write is logged but does not interrupt
// the legacy Telegram delivery path (the new path is passive under the
// mailbox.outbound flag).
func NewDBOutboxWriter(pool *pgxpool.Pool) OutboxWriter {
	return func(e OutboxEvent) {
		if pool == nil || e.AgentID == "" || e.Text == "" {
			return
		}
		ctx := context.Background()

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
			AgentID: e.AgentID,
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
