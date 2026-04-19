package bot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunTopicProvisioner runs as a background goroutine. Every 15 s it checks
// for user agents that have no Telegram owner binding and creates a forum
// topic for each one so the relay can deliver their responses to Telegram.
//
// No-ops when AllowedGroups is empty (no group configured). Terminates when
// ctx is cancelled.
func (b *Bot) RunTopicProvisioner(ctx context.Context, pool *pgxpool.Pool) {
	if len(b.config.AllowedGroups) == 0 {
		log.Println("topic provisioner: no ALLOWED_GROUPS configured — skipping")
		return
	}
	if pool == nil {
		log.Println("topic provisioner: no DB pool — skipping")
		return
	}
	log.Println("topic provisioner: started")
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.provisionMissingTopics(ctx, pool); err != nil {
				log.Printf("topic provisioner: %v", err)
			}
		}
	}
}

// provisionMissingTopics finds user agents without an owner binding and
// creates a Telegram forum topic + binding for each.
func (b *Bot) provisionMissingTopics(ctx context.Context, pool *pgxpool.Pool) error {
	chatID := b.config.AllowedGroups[0]
	if len(b.config.AllowedUsers) == 0 {
		return nil // need at least one operator user id for the binding
	}
	userID := fmt.Sprintf("%d", b.config.AllowedUsers[0])

	// Agents that are active (not archived/dead) and have no owner binding.
	rows, err := pool.Query(ctx, `
		SELECT a.id, COALESCE(NULLIF(a.handle,''), a.id)
		FROM agents a
		WHERE a.role = 'user'
		  AND a.task_id IS NULL
		  AND a.status NOT IN ('archived', 'dead')
		  AND a.stop_requested = FALSE
		  AND NOT EXISTS (
		        SELECT 1 FROM topic_agent_bindings b
		        WHERE b.agent_id = a.id
		          AND b.binding_type = 'owner'
		      )
	`)
	if err != nil {
		return fmt.Errorf("query unbound agents: %w", err)
	}
	defer rows.Close()

	type agentRow struct{ id, name string }
	var agents []agentRow
	for rows.Next() {
		var r agentRow
		if err := rows.Scan(&r.id, &r.name); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		agents = append(agents, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, a := range agents {
		threadID, err := b.createForumTopic(chatID, a.name)
		if err != nil {
			log.Printf("topic provisioner: create topic for agent %s: %v", a.id, err)
			continue
		}
		threadIDStr := fmt.Sprintf("%d", threadID)
		if _, err := pool.Exec(ctx, `
			INSERT INTO topic_agent_bindings
				(topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
			VALUES ($1, $2, 'owner', $3, $4, $5)
			ON CONFLICT DO NOTHING
		`, threadID, a.id, userID, threadIDStr, chatID); err != nil {
			log.Printf("topic provisioner: bind agent %s → topic %d: %v", a.id, threadID, err)
			continue
		}
		log.Printf("topic provisioner: created topic %d (chat %d) for agent %s", threadID, chatID, a.id)
	}
	return nil
}
