package bot

import (
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
)

// SetPool sets the database pool explicitly (e.g. from --orchestrate startup).
func (b *Bot) SetPool(pool *pgxpool.Pool) {
	b.poolMu.Lock()
	b.pool = pool
	b.poolMu.Unlock()
}

// getPool returns the DB pool, connecting lazily on first use if DATABASE_URL is set.
func (b *Bot) getPool() *pgxpool.Pool {
	b.poolMu.Lock()
	defer b.poolMu.Unlock()
	if b.pool != nil {
		return b.pool
	}
	if b.config.DatabaseURL == "" {
		return nil
	}
	pool, err := db.Connect(b.config.DatabaseURL)
	if err != nil {
		log.Printf("DB connection failed: %v", err)
		return nil
	}
	log.Println("Database connected (lazy)")
	b.pool = pool
	return b.pool
}

// handleObserveCommand binds the current topic to an agent for observation.
func (b *Bot) handleObserveCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use observation.")
		return
	}

	agentID := strings.TrimSpace(msg.CommandArguments())
	if agentID == "" {
		b.reply(chatID, threadID, "Usage: /observe <agent-id>")
		return
	}

	topicID := int64(threadID)
	if err := db.BindTopicToAgent(pool, topicID, agentID, "observe"); err != nil {
		log.Printf("Error binding topic %d to agent %s: %v", topicID, agentID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Now observing agent: %s", agentID))
}

// handleUnobserveCommand unbinds the current topic from an agent.
func (b *Bot) handleUnobserveCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use observation.")
		return
	}

	agentID := strings.TrimSpace(msg.CommandArguments())
	if agentID == "" {
		b.reply(chatID, threadID, "Usage: /unobserve <agent-id>")
		return
	}

	topicID := int64(threadID)
	if err := db.UnbindTopicFromAgent(pool, topicID, agentID); err != nil {
		log.Printf("Error unbinding topic %d from agent %s: %v", topicID, agentID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Stopped observing agent: %s", agentID))
}

// handleWatchingCommand lists agents observed by the current topic.
func (b *Bot) handleWatchingCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use observation.")
		return
	}

	topicID := int64(threadID)
	bindings, err := db.GetAgentsForTopic(pool, topicID)
	if err != nil {
		log.Printf("Error getting agents for topic %d: %v", topicID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(bindings) == 0 {
		b.reply(chatID, threadID, "Not observing any agents.")
		return
	}

	var lines []string
	lines = append(lines, "Observing agents:")
	for _, bind := range bindings {
		lines = append(lines, fmt.Sprintf("  - %s (%s)", bind.AgentID, bind.BindingType))
	}
	b.reply(chatID, threadID, strings.Join(lines, "\n"))
}
