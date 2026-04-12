package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/tmux"
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
	// maxAgentsAtomic allows dynamic updates to MaxAgents at runtime.
	// Initialized from MaxAgents in Run().
	maxAgentsAtomic *atomic.Int32
}

// SetMaxAgents updates the max agents value at runtime.
func (c *Config) SetMaxAgents(n int) {
	if c.maxAgentsAtomic != nil {
		c.maxAgentsAtomic.Store(int32(n))
	}
}

// GetMaxAgents returns the current max agents value.
func (c *Config) GetMaxAgents() int {
	if c.maxAgentsAtomic != nil {
		return int(c.maxAgentsAtomic.Load())
	}
	return c.MaxAgents
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
	cfg.maxAgentsAtomic = &atomic.Int32{}
	cfg.maxAgentsAtomic.Store(int32(cfg.MaxAgents))

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
	// 1. RECONCILE: detect and clean up dead agents (both planner and executor).
	if err := reconcile(cfg); err != nil {
		log.Printf("Reconcile error: %v", err)
	}

	// 2. POLL: count active EXECUTOR agents only (planners don't count against slots).
	agents, err := db.ListAgents(cfg.Pool)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	executorCount := 0
	for _, a := range agents {
		if a.Status != "dead" && a.Role == "executor" {
			executorCount++
		}
	}

	// 3. DISPATCH: retired in task 3.6. Task dispatch now flows through
	// agent_inbox via the `maquinista task-scheduler` subcommand (§D.4)
	// which claims ready tasks, calls orchestrator.EnsureAgent, and
	// enqueues /work-on-task. The legacy volta-era direct-dispatch path
	// (spawn agent → db.AtomicClaim → send keys into the pane) is gone.
	_ = executorCount // slot-bookkeeping preserved for future policy hooks

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

// Legacy volta-era dispatch() was removed in task 3.6. Every ready task
// now reaches its implementor via the task-scheduler → agent_inbox path,
// not a direct tmux spawn-and-claim. Runtimes that still want to cap
// concurrent agents should do so in the task-scheduler's EnsureAgent
// callback (a future follow-up on this package).

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
