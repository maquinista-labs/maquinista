package bot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunTopicProvisioner runs as a background goroutine. Every 15 s it:
//   - creates Telegram forum topics for user agents that have none, and
//   - closes + removes bindings for agents that have been archived or deleted.
//
// No-ops when AllowedGroups is empty. Terminates when ctx is cancelled.
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
			if err := b.closeOrphanedTopics(ctx, pool); err != nil {
				log.Printf("topic provisioner (close): %v", err)
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
		// Populate user_thread_chats so the monitor's GetGroupChatID lookup
		// succeeds immediately — without this the monitor skips Telegram
		// delivery until the user sends a message from Telegram first.
		b.State().SetGroupChatID(userID, threadIDStr, chatID)
		log.Printf("topic provisioner: created topic %d (chat %d) for agent %s", threadID, chatID, a.id)
	}
	return nil
}

// closeOrphanedTopics closes Telegram forum topics for agents that have been
// archived (or whose row no longer exists) and removes the binding so the
// relay stops delivering to them.
func (b *Bot) closeOrphanedTopics(ctx context.Context, pool *pgxpool.Pool) error {
	// Bindings whose agent is archived, dead, or deleted (LEFT JOIN → NULL).
	rows, err := pool.Query(ctx, `
		SELECT b.chat_id, b.thread_id::bigint, b.agent_id
		FROM topic_agent_bindings b
		LEFT JOIN agents a ON a.id = b.agent_id
		WHERE b.binding_type = 'owner'
		  AND b.chat_id IS NOT NULL
		  AND b.thread_id IS NOT NULL
		  AND (a.id IS NULL OR a.status IN ('archived', 'dead'))
	`)
	if err != nil {
		return fmt.Errorf("query orphaned bindings: %w", err)
	}
	defer rows.Close()

	type orphan struct {
		chatID   int64
		threadID int64
		agentID  string
	}
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.chatID, &o.threadID, &o.agentID); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		orphans = append(orphans, o)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, o := range orphans {
		if err := b.closeForumTopic(o.chatID, int(o.threadID)); err != nil {
			// Topic may already be closed or deleted — log and clean up binding anyway.
			log.Printf("topic provisioner: close topic %d for agent %s: %v (removing binding)", o.threadID, o.agentID, err)
		}
		if _, err := pool.Exec(ctx, `
			DELETE FROM topic_agent_bindings
			WHERE agent_id = $1 AND binding_type = 'owner'
		`, o.agentID); err != nil {
			log.Printf("topic provisioner: remove binding for agent %s: %v", o.agentID, err)
			continue
		}
		log.Printf("topic provisioner: closed topic %d and removed binding for agent %s", o.threadID, o.agentID)
	}
	return nil
}
