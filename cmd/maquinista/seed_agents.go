package main

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/soul"
)

// G.4 of plans/active/dashboard-gaps.md — first-boot seed so a fresh
// install comes up with coordinator / planner / coder archetypes
// already populated, instead of forcing the operator through three
// manual `maquinista agent add` invocations before the UI is useful.
//
// Idempotent by design: rows that already exist are left untouched.
// The soul templates themselves are shipped by migration 028.
//
// Seeded rows land as status='stopped'; the existing
// reconcileAgentPanes path brings their tmux panes up on the very
// next tick — same code that handles post-restart respawn, so the
// "never been spawned" and "was running before the last stop" cases
// follow the same path.

type seedAgent struct {
	id         string
	handle     string
	templateID string
}

var defaultSeedAgents = []seedAgent{
	{"seed-coordinator", "coordinator", "coordinator"},
	{"seed-planner", "planner", "planner"},
	{"seed-coder", "coder", "coder"},
}

// seedDefaultAgents ensures the default archetypes exist. The env
// var MAQUINISTA_SKIP_SEED_AGENTS=1 opts out (used by tests and by
// operators who want a clean slate).
func seedDefaultAgents(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, defaultCWD string) error {
	if os.Getenv("MAQUINISTA_SKIP_SEED_AGENTS") == "1" {
		log.Println("seed-agents: MAQUINISTA_SKIP_SEED_AGENTS=1, skipping")
		return nil
	}
	if pool == nil {
		return errors.New("seedDefaultAgents: pool is nil")
	}
	if cfg == nil {
		return errors.New("seedDefaultAgents: cfg is nil")
	}
	if defaultCWD == "" {
		return errors.New("seedDefaultAgents: defaultCWD is empty")
	}
	runnerType := cfg.DefaultRunner
	if runnerType == "" {
		runnerType = "claude"
	}

	for _, a := range defaultSeedAgents {
		if err := seedOneAgent(ctx, pool, cfg.TmuxSessionName, runnerType, defaultCWD, a); err != nil {
			// Log and move on — one failed seed shouldn't block the others
			// or the daemon startup.
			log.Printf("seed-agents: %s: %v", a.id, err)
		}
	}
	return nil
}

func seedOneAgent(
	ctx context.Context,
	pool *pgxpool.Pool,
	tmuxSession, runnerType, cwd string,
	a seedAgent,
) error {
	var one int
	err := pool.QueryRow(ctx, `SELECT 1 FROM agents WHERE id = $1`, a.id).Scan(&one)
	if err == nil {
		log.Printf("seed-agents: %s already exists, skipping", a.id)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	log.Printf("seed-agents: seeding %s (handle=%s, template=%s)",
		a.id, a.handle, a.templateID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO agents
			(id, handle, tmux_session, tmux_window, role, status,
			 runner_type, cwd, window_name,
			 started_at, last_seen, stop_requested, workspace_scope)
		VALUES
			($1, $2, $3, '', 'user', 'stopped',
			 $4, $5, $1,
			 NOW(), NOW(), FALSE, 'shared')
	`, a.id, a.handle, tmuxSession, runnerType, cwd); err != nil {
		return err
	}

	if err := soul.CreateFromTemplate(ctx, tx, a.id, a.templateID, soul.Overrides{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
