package pipeline

// Intake half of the linear-bridge (ADR-0005): Linear MAQ Todo issues
// (label "pipeline") become maquinista task rows the task-scheduler can
// claim. The linear_issue_map INSERT is the single-owner claim; workers
// spawned for these rows are EX-02 scope and never talk to Linear.

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BridgeConfig is the operator env contract for the bridge (plan Landing).
type BridgeConfig struct {
	APIKey   string
	TeamID   string
	Project  string
	Interval time.Duration
}

// Enabled reports whether the bridge should run: both the credential and the
// team are required. Anything else is a logged no-op at startup.
func (c BridgeConfig) Enabled() bool { return c.APIKey != "" && c.TeamID != "" }

// FromEnv reads the env contract. LINEAR_API_KEY is Linear's documented
// credential name (unprefixed on purpose — the same name other Linear
// tooling on the operator's boxes already reads); the maquinista knobs are
// MAQUINISTA_-prefixed to avoid collisions.
func FromEnv() BridgeConfig {
	cfg := BridgeConfig{
		APIKey:  os.Getenv("LINEAR_API_KEY"),
		TeamID:  os.Getenv("MAQUINISTA_LINEAR_TEAM_ID"),
		Project: os.Getenv("MAQUINISTA_LINEAR_PROJECT"),
	}
	if cfg.Project == "" {
		cfg.Project = os.Getenv("MAQUINISTA_PROJECT")
	}
	cfg.Interval = 60 * time.Second // ADR-0005 intake bound
	if v := os.Getenv("MAQUINISTA_LINEAR_POLL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Interval = d
		}
	}
	return cfg
}

// ClaimIssue inserts, in one transaction, the task row (status "ready") and
// the linear_issue_map row (pending_state "In Progress"). The map row's PK
// makes the INSERT the claim: a conflict rolls the whole transaction back
// and reports created=false.
func ClaimIssue(ctx context.Context, pool *pgxpool.Pool, iss Issue, teamID, project string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("pipeline: begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	taskID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO tasks (id, title, body, status, project_id, metadata)
		VALUES ($1, $2, $3, 'ready', $4, $5::jsonb)`,
		taskID, fmt.Sprintf("[%s] %s", iss.Identifier, iss.Title), iss.Description, project,
		fmt.Sprintf(`{"linear_issue_id":%q,"linear_url":%q}`, iss.ID, iss.URL),
	); err != nil {
		return false, fmt.Errorf("pipeline: insert task: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO linear_issue_map (linear_issue_id, identifier, team_id, task_id, pending_state)
		VALUES ($1, $2, $3, $4, 'In Progress')
		ON CONFLICT (linear_issue_id) DO NOTHING`,
		iss.ID, iss.Identifier, teamID, taskID,
	)
	if err != nil {
		return false, fmt.Errorf("pipeline: insert map row: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already claimed; task insert rolls back
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("pipeline: commit claim: %w", err)
	}
	return true, nil
}

// RunBridge polls Linear on cfg.Interval and claims every unmapped Todo
// issue. Tick errors are logged and the loop continues; the process ctx
// cancels it.
func RunBridge(ctx context.Context, pool *pgxpool.Pool, client LinearAPI, cfg BridgeConfig) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	log.Printf("pipeline: bridge polling team %s every %s (project %q)", cfg.TeamID, cfg.Interval, cfg.Project)
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if err := claimReady(ctx, pool, client, cfg); err != nil {
			log.Printf("pipeline: claim tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// claimReady is one intake tick: fetch Todo issues, claim the unmapped ones.
func claimReady(ctx context.Context, pool *pgxpool.Pool, client LinearAPI, cfg BridgeConfig) error {
	issues, err := client.TodoIssues(ctx, cfg.TeamID)
	if err != nil {
		return fmt.Errorf("fetching todo issues: %w", err)
	}
	for _, iss := range issues {
		created, err := ClaimIssue(ctx, pool, iss, cfg.TeamID, cfg.Project)
		if err != nil {
			log.Printf("pipeline: claim %s: %v", iss.Identifier, err)
			continue
		}
		if created {
			log.Printf("pipeline: claimed %s (%s)", iss.Identifier, iss.Title)
		}
	}
	return nil
}
