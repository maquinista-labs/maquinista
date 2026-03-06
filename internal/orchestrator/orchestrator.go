package orchestrator

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/otaviocarvalho/volta/internal/agent"
	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/otaviocarvalho/volta/internal/runner"
	"github.com/otaviocarvalho/volta/internal/tmux"
)

// Config holds orchestrator configuration.
type Config struct {
	Pool         *pgxpool.Pool
	Runner       runner.AgentRunner
	TmuxSession  string
	ProjectID    string
	MaxAgents    int
	PollInterval time.Duration
	UseWorktrees bool
	ClaudeMDPath string
	DatabaseURL  string
	// NotifyCh receives task events for immediate wake-up.
	// When nil, the orchestrator only uses ticker-based polling.
	NotifyCh <-chan struct{}
	// NotifyFunc is called with status messages for external notification
	// (e.g., sending to a Telegram topic). Can be nil.
	NotifyFunc func(message string)
}

// Run implements the poll-dispatch-reconcile orchestrator loop.
// It blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if cfg.MaxAgents <= 0 {
		cfg.MaxAgents = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Second
	}

	log.Printf("Orchestrator starting: project=%s maxAgents=%d poll=%s runner=%s",
		cfg.ProjectID, cfg.MaxAgents, cfg.PollInterval, cfg.Runner.Name())

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	notifyCh := cfg.NotifyCh
	if notifyCh == nil {
		// Use a nil channel that never fires if no notify channel provided.
		notifyCh = make(chan struct{})
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Orchestrator shutting down")
			return nil
		case <-ticker.C:
			if err := tick(ctx, cfg); err != nil {
				log.Printf("Orchestrator tick error: %v", err)
			}
		case <-notifyCh:
			log.Println("Orchestrator: task event received, running immediate tick")
			if err := tick(ctx, cfg); err != nil {
				log.Printf("Orchestrator tick error: %v", err)
			}
		}
	}
}

func tick(ctx context.Context, cfg Config) error {
	// 1. RECONCILE: detect and clean up dead agents.
	if err := reconcile(cfg); err != nil {
		log.Printf("Reconcile error: %v", err)
	}

	// 2. POLL: count active agents and check for ready tasks.
	agents, err := db.ListAgents(cfg.Pool)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	activeCount := 0
	for _, a := range agents {
		if a.Status != "dead" {
			activeCount++
		}
	}

	// 3. DISPATCH: spawn agents for available slots.
	slotsAvailable := cfg.MaxAgents - activeCount
	for i := 0; i < slotsAvailable; i++ {
		dispatched, err := dispatch(cfg)
		if err != nil {
			log.Printf("Dispatch error: %v", err)
			break
		}
		if !dispatched {
			break // no more ready tasks
		}
	}

	// 4. MERGE: process merge queue.
	if err := processMergeQueue(cfg); err != nil {
		log.Printf("Merge queue error: %v", err)
	}

	// 5. LOG status.
	logStatus(cfg, agents)

	return nil
}

func reconcile(cfg Config) error {
	agents, err := db.ListAgents(cfg.Pool)
	if err != nil {
		return err
	}

	for _, a := range agents {
		if !tmux.WindowExists(cfg.TmuxSession, a.TmuxWindow) {
			log.Printf("Agent %s window dead, cleaning up", a.ID)
			notify(cfg, fmt.Sprintf("Agent dead: %s (window gone)", a.ID))
			if err := agent.Kill(cfg.Pool, cfg.TmuxSession, a.ID); err != nil {
				log.Printf("Error killing dead agent %s: %v", a.ID, err)
			}
		}
	}
	return nil
}

func dispatch(cfg Config) (bool, error) {
	agentID := fmt.Sprintf("orch-%d", time.Now().UnixNano())

	env := map[string]string{
		"DATABASE_URL": cfg.DatabaseURL,
	}

	var a *agent.Agent
	var err error
	if cfg.UseWorktrees {
		a, err = agent.SpawnWithWorktree(cfg.Pool, cfg.TmuxSession, agentID, cfg.ClaudeMDPath, env, cfg.Runner)
	} else {
		a, err = agent.Spawn(cfg.Pool, cfg.TmuxSession, agentID, cfg.ClaudeMDPath, env, cfg.Runner)
	}
	if err != nil {
		return false, fmt.Errorf("spawning agent: %w", err)
	}

	// Try to claim a task for this agent.
	var projPtr *string
	if cfg.ProjectID != "" {
		projPtr = &cfg.ProjectID
	}
	task, err := db.AtomicClaim(cfg.Pool, a.ID, projPtr)
	if err != nil {
		// No task available — kill the agent we just spawned.
		agent.Kill(cfg.Pool, cfg.TmuxSession, a.ID)
		return false, nil
	}
	if task == nil {
		agent.Kill(cfg.Pool, cfg.TmuxSession, a.ID)
		return false, nil
	}

	log.Printf("Dispatched agent %s for task %s", a.ID, task.ID)
	notify(cfg, fmt.Sprintf("Agent spawned: %s → task %s", a.ID, task.ID))
	return true, nil
}

func processMergeQueue(cfg Config) error {
	entry, err := db.ClaimMergeEntry(cfg.Pool)
	if err != nil {
		return err
	}
	if entry == nil {
		return nil
	}

	log.Printf("Processing merge: task=%s branch=%s", entry.TaskID, entry.Branch)
	// Merge processing is handled by the existing merge command infrastructure.
	// The orchestrator just claims entries to signal they should be processed.
	return nil
}

func notify(cfg Config, message string) {
	if cfg.NotifyFunc != nil {
		cfg.NotifyFunc(message)
	}
}

func logStatus(cfg Config, agents []*db.Agent) {
	active := 0
	idle := 0
	working := 0
	for _, a := range agents {
		switch a.Status {
		case "idle":
			idle++
			active++
		case "working":
			working++
			active++
		}
	}
	if active > 0 {
		log.Printf("Orchestrator status: active=%d (idle=%d working=%d)", active, idle, working)
	}
}
