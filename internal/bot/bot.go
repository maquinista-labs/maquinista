package bot

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/bridge"
	"github.com/maquinista-labs/maquinista/internal/queue"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// Bot is the main Telegram bot instance.
type Bot struct {
	api    *tgbotapi.BotAPI
	config *config.Config
	state  *state.State
	mu     sync.RWMutex

	// Per-user browse state for directory browser
	browseStates map[int64]*BrowseState
	// Per-user cached window lists for window picker
	windowCache map[int64][]tmux.Window
	// Per-user window picker state
	windowPickerStates map[int64]*windowPickerState
	// Per-user tier-4 agent picker state
	agentPickerStates map[int64]*agentPickerState
	// Per-user file browser state for /get command
	fileBrowseStates map[int64]*FileBrowseState
	// Per-user add-task wizard state
	addTaskStates map[int64]*addTaskState
	// Per-user task picker state (for /pick and /pickw without args)
	taskPickerStates map[int64]*taskPickerState
	// Per-user pending input for parameterized commands
	pendingInputs map[int64]*pendingInput
	// Per-user pending plan approval state
	planStates map[int64]*planState
	// Monitor state (set by serve command when monitor is started)
	monitorState *state.MonitorState
	// Minuano CLI bridge
	minuanoBridge *bridge.Bridge
	// Message queue (set after construction via SetQueue)
	msgQueue *queue.Queue
	// DB pool (optional, connected lazily on first use)
	pool   *pgxpool.Pool
	poolMu sync.Mutex
	// Default runner for agent spawning
	defaultRunner runner.AgentRunner
}

// New creates a new Bot instance.
func New(cfg *config.Config) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, fmt.Errorf("creating bot API: %w", err)
	}

	log.Printf("Authorized as @%s", api.Self.UserName)

	// Load state
	statePath := filepath.Join(cfg.MaquinistaDir, "state.json")
	st, err := state.Load(statePath)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}

	// Ensure tmux session
	if err := tmux.EnsureSession(cfg.TmuxSessionName); err != nil {
		return nil, fmt.Errorf("ensuring tmux session: %w", err)
	}

	return &Bot{
		api:                api,
		config:             cfg,
		state:              st,
		browseStates:       make(map[int64]*BrowseState),
		windowCache:        make(map[int64][]tmux.Window),
		windowPickerStates: make(map[int64]*windowPickerState),
		agentPickerStates:  make(map[int64]*agentPickerState),
		fileBrowseStates:   make(map[int64]*FileBrowseState),
		addTaskStates:      make(map[int64]*addTaskState),
		taskPickerStates:   make(map[int64]*taskPickerState),
		pendingInputs:      make(map[int64]*pendingInput),
		planStates:         make(map[int64]*planState),
		minuanoBridge:      bridge.NewBridge(cfg.MaquinistaBin, cfg.DatabaseURL),
	}, nil
}

// registerCommands sets the bot's command menu in Telegram.
func (b *Bot) registerCommands() {
	commands := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "menu", Description: "Show command menu"},
		tgbotapi.BotCommand{Command: "c_screenshot", Description: "Terminal screenshot with control keys"},
		tgbotapi.BotCommand{Command: "c_esc", Description: "Send Escape to interrupt Claude"},
		tgbotapi.BotCommand{Command: "c_clear", Description: "Forward /clear to Claude Code"},
		tgbotapi.BotCommand{Command: "c_help", Description: "Forward /help to Claude Code"},
		tgbotapi.BotCommand{Command: "c_get", Description: "Browse and send a file"},
		tgbotapi.BotCommand{Command: "p_bind", Description: "Bind a Minuano project to this topic"},
		tgbotapi.BotCommand{Command: "p_tasks", Description: "List tasks for the bound project"},
		tgbotapi.BotCommand{Command: "p_add", Description: "Create a new Minuano task"},
		tgbotapi.BotCommand{Command: "p_delete", Description: "Delete a Minuano task"},
		tgbotapi.BotCommand{Command: "p_history", Description: "Message history for this topic"},
		tgbotapi.BotCommand{Command: "t_pick", Description: "Assign a specific task to Claude"},
		tgbotapi.BotCommand{Command: "t_pickw", Description: "Pick task in isolated worktree"},
		tgbotapi.BotCommand{Command: "t_auto", Description: "Auto-claim and work project tasks"},
		tgbotapi.BotCommand{Command: "t_batch", Description: "Work a list of tasks in order"},
		tgbotapi.BotCommand{Command: "t_unclaim", Description: "Release a claimed task back to ready"},
		tgbotapi.BotCommand{Command: "t_merge", Description: "Merge a branch (auto-resolve conflicts)"},
		tgbotapi.BotCommand{Command: "t_plan", Description: "Plan and create tasks from a description"},
		tgbotapi.BotCommand{Command: "plan", Description: "Open a planner session in this topic"},
		tgbotapi.BotCommand{Command: "runner", Description: "Show/switch default runner (claude, opencode)"},
		tgbotapi.BotCommand{Command: "agent_list", Description: "List all registered agents"},
		tgbotapi.BotCommand{Command: "agent_spawn", Description: "Spawn a new execution agent"},
		tgbotapi.BotCommand{Command: "agent_kill", Description: "Kill a specific agent"},
		tgbotapi.BotCommand{Command: "agent_kill_all", Description: "Kill all agents"},
		tgbotapi.BotCommand{Command: "default", Description: "Set your owner agent for this topic"},
		tgbotapi.BotCommand{Command: "global_default", Description: "Admin: set the global default agent"},
		tgbotapi.BotCommand{Command: "schedule", Description: "Register a scheduled job"},
		tgbotapi.BotCommand{Command: "hook_register", Description: "Admin: register a webhook"},
		tgbotapi.BotCommand{Command: "hook_enable", Description: "Admin: enable a webhook"},
		tgbotapi.BotCommand{Command: "hook_disable", Description: "Admin: disable a webhook"},
		tgbotapi.BotCommand{Command: "jobs", Description: "List scheduled jobs"},
		tgbotapi.BotCommand{Command: "hooks", Description: "List webhook handlers"},
		tgbotapi.BotCommand{Command: "job_runs", Description: "Show run history for a job"},
	)
	if _, err := b.api.Request(commands); err != nil {
		log.Printf("Warning: failed to register bot commands: %v", err)
	} else {
		log.Println("Registered bot command menu")
	}
}

// Run starts the bot polling loop. Blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	b.registerCommands()
	log.Println("Bot is running...")

	offset := 0
	for {
		select {
		case <-ctx.Done():
			b.saveState()
			log.Println("Bot shutting down.")
			return nil
		default:
		}

		updates, err := b.getUpdatesRaw(offset, 30)
		if err != nil {
			log.Printf("Error getting updates: %v", err)
			continue
		}

		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			b.handleUpdate(update)
		}

		// Periodically clean up old cache entries
		if offset > 1000 {
			cleanupCache(offset - 1000)
		}
	}
}

// handleUpdate routes an update to the appropriate handler.
func (b *Bot) handleUpdate(update tgbotapi.Update) {
	if update.Message != nil {
		log.Printf("DEBUG: received message from user=%d chat=%d text=%q",
			update.Message.From.ID, update.Message.Chat.ID, update.Message.Text)
		if !b.isAuthorized(update.Message.From.ID, update.Message.Chat.ID) {
			log.Printf("DEBUG: unauthorized user=%d chat=%d (ALLOWED_USERS=%v, ALLOWED_GROUPS=%v)",
				update.Message.From.ID, update.Message.Chat.ID,
				b.config.AllowedUsers, b.config.AllowedGroups)
			return
		}
		b.handleMessage(update.Message)
	} else if update.CallbackQuery != nil {
		log.Printf("DEBUG: callback from user=%d chat=%d data=%q",
			update.CallbackQuery.From.ID, update.CallbackQuery.Message.Chat.ID, update.CallbackQuery.Data)
		if !b.isAuthorized(update.CallbackQuery.From.ID, update.CallbackQuery.Message.Chat.ID) {
			log.Printf("DEBUG: unauthorized callback user=%d chat=%d",
				update.CallbackQuery.From.ID, update.CallbackQuery.Message.Chat.ID)
			return
		}
		b.handleCallback(update.CallbackQuery)
	}
}

// isAuthorized checks if a user/chat is allowed.
func (b *Bot) isAuthorized(userID, chatID int64) bool {
	// Always allow messages from the bot itself.
	if b.api != nil && userID == b.api.Self.ID {
		return true
	}
	if !b.config.IsAllowedUser(userID) {
		return false
	}
	if chatID < 0 && !b.config.IsAllowedGroup(chatID) {
		return false
	}
	return true
}

// handleMessage routes messages to the appropriate handler.
func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	// Check for forum topic closed events
	if isForumTopicClosed(msg) {
		b.handleTopicClose(msg)
		return
	}

	// Handle commands
	if msg.IsCommand() {
		b.handleCommand(msg)
		return
	}

	// Handle text messages
	if msg.Text != "" {
		b.handleTextMessage(msg)
		return
	}
}

// handleCallback routes callback queries.
func (b *Bot) handleCallback(cq *tgbotapi.CallbackQuery) {
	b.routeCallback(cq)
}

// saveState persists the current state to disk.
func (b *Bot) saveState() {
	path := filepath.Join(b.config.MaquinistaDir, "state.json")
	if err := b.state.Save(path); err != nil {
		log.Printf("Error saving state: %v", err)
	}
}

// reply sends a text reply to a message in its thread.
func (b *Bot) reply(chatID int64, threadID int, text string) {
	if _, err := b.sendMessageInThread(chatID, threadID, text); err != nil {
		log.Printf("Error sending reply: %v", err)
	}
}

// CreatePlannerTopic creates a Telegram forum topic for a planner agent.
func (b *Bot) CreatePlannerTopic(chatID int64, specTitle string) (int, error) {
	return b.createForumTopic(chatID, "Plan: "+specTitle)
}

// ReplyToThread sends a text message to a specific thread.
func (b *Bot) ReplyToThread(chatID int64, threadID int, text string) {
	b.reply(chatID, threadID, text)
}

// GetChatID returns the primary group chat ID from AllowedGroups.
func (b *Bot) GetChatID() int64 {
	if len(b.config.AllowedGroups) > 0 {
		return b.config.AllowedGroups[0]
	}
	return 0
}

// API returns the underlying BotAPI for use by other packages.
func (b *Bot) API() *tgbotapi.BotAPI {
	return b.api
}

// State returns the bot's state.
func (b *Bot) State() *state.State {
	return b.state
}

// Config returns the bot's config.
func (b *Bot) Config() *config.Config {
	return b.config
}

// SetQueue sets the message queue reference for flood control checks.
func (b *Bot) SetQueue(q *queue.Queue) {
	b.msgQueue = q
}

// SetDefaultRunner sets the default runner for agent spawning.
func (b *Bot) SetDefaultRunner(r runner.AgentRunner) {
	b.defaultRunner = r
}

// DefaultRunner returns the default runner for agent spawning.
func (b *Bot) DefaultRunner() runner.AgentRunner {
	return b.defaultRunner
}

// answerCallback answers an inline callback query with a toast message.
func (b *Bot) answerCallback(callbackID, text string) {
	cb := tgbotapi.NewCallback(callbackID, text)
	if _, err := b.api.Request(cb); err != nil {
		log.Printf("Error answering callback: %v", err)
	}
}
