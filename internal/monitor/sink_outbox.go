package monitor

import (
	"context"
	"encoding/json"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
	"github.com/maquinista-labs/maquinista/internal/render"
)

type OutboxSink struct {
	pool *pgxpool.Pool
	amap *mailbox.ActiveInboxMap
}

func NewOutboxSink(pool *pgxpool.Pool, amap *mailbox.ActiveInboxMap) *OutboxSink {
	return &OutboxSink{pool: pool, amap: amap}
}

func (s *OutboxSink) Name() string { return "outbox" }

func (s *OutboxSink) Handle(e AgentEvent) {
	// Only fire on the DB-only pass (chatID=0). The Telegram routing pass
	// (chatID!=0) would cause one outbox row per user binding — incorrect.
	if e.ChatID != 0 {
		return
	}
	if s.pool == nil || e.AgentID == "" || e.Role != "assistant" {
		return
	}

	var text string
	switch e.Kind {
	case AgentEventText:
		text = render.FormatText(e.Text)
	case AgentEventThinking:
		text = render.FormatThinking(e.Text)
	default:
		return
	}
	if text == "" {
		return
	}

	ctx := context.Background()

	// Resolve window ID to logical agent ID.
	agentID, err := resolveAgentFromWindow(ctx, s.pool, e.AgentID)
	if err != nil {
		log.Printf("outbox sink: resolve agent for window %s: %v", e.AgentID, err)
		return
	}
	if agentID == "" {
		return
	}

	content, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Role string `json:"role,omitempty"`
	}{Type: "text", Text: text, Role: e.Role})
	if err != nil {
		log.Printf("outbox sink: marshal: %v", err)
		return
	}

	var inReplyTo *uuid.UUID
	if s.amap != nil {
		if raw := s.amap.Get(agentID); raw != "" {
			if id, parseErr := uuid.Parse(raw); parseErr == nil {
				inReplyTo = &id
			}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Printf("outbox sink: begin: %v", err)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := mailbox.AppendOutbox(ctx, tx, mailbox.OutboxMessage{
		AgentID:   agentID,
		InReplyTo: inReplyTo,
		Content:   content,
	}); err != nil {
		log.Printf("outbox sink: append: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("outbox sink: commit: %v", err)
	}
}
