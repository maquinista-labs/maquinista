package pipeline

// Intake half of the ticket bridge (ADR-0005/0006): ticket-system issues in
// the intake queue become maquinista task rows the task-scheduler can claim.
// The ticket_issue_map INSERT is the single-owner claim; workers spawned for
// these rows are EX-02 scope and never talk to the ticket system. All
// provider specifics live behind TicketProvider.

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TicketsConfig is the operator env contract for the bridge (ADR-0006).
type TicketsConfig struct {
	Provider string
	APIKey   string
	TeamID   string
	Project  string
	Interval time.Duration
	// Repo is the repo root sibling worktrees are provisioned from
	// (MAQ-13). Empty → RunBridge resolves WorktreeRepo() once at startup;
	// still empty → claims go out without a worktree and park needs-human.
	Repo string
}

// Enabled reports whether the bridge should run: both the credential and the
// team are required. Anything else is a logged no-op at startup.
func (c TicketsConfig) Enabled() bool { return c.APIKey != "" && c.TeamID != "" }

// FromEnv reads the env contract. The MAQUINISTA_TICKETS_* namespace is
// provider-neutral; the provider itself resolves its own credential
// fallbacks (the Linear provider additionally honors the legacy Linear env key).
func FromEnv() TicketsConfig {
	cfg := TicketsConfig{
		Provider: os.Getenv("MAQUINISTA_TICKETS_PROVIDER"),
		APIKey:   os.Getenv("MAQUINISTA_TICKETS_API_KEY"),
		TeamID:   os.Getenv("MAQUINISTA_TICKETS_TEAM_ID"),
		Project:  os.Getenv("MAQUINISTA_TICKETS_PROJECT"),
		Repo:     os.Getenv(WorktreeRepoEnv),
	}
	if cfg.Provider == "" {
		cfg.Provider = "linear"
	}
	if cfg.Project == "" {
		cfg.Project = os.Getenv("MAQUINISTA_PROJECT")
	}
	cfg.Interval = 60 * time.Second // ADR-0005 intake bound
	if v := os.Getenv("MAQUINISTA_TICKETS_POLL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Interval = d
		}
	}
	return cfg
}

// ClaimIssue inserts, in one transaction, the task row (status "ready",
// worktree_path = worktree when non-nil) and the ticket_issue_map row
// (pending_state canonical "In Progress"). The map row's PK makes the INSERT
// the claim: a conflict rolls the whole transaction back and reports
// created=false.
func ClaimIssue(ctx context.Context, pool *pgxpool.Pool, iss Issue, teamID, project string, worktree *string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("pipeline: begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	taskID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO tasks (id, title, body, status, project_id, worktree_path, metadata)
		VALUES ($1, $2, $3, 'ready', $4, $5, $6::jsonb)`,
		taskID, fmt.Sprintf("[%s] %s", iss.Key, iss.Title), iss.Description, project,
		worktree,
		fmt.Sprintf(`{"ticket_issue_id":%q,"ticket_url":%q}`, iss.ID, iss.URL),
	); err != nil {
		return false, fmt.Errorf("pipeline: insert task: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id, pending_state)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (issue_id) DO NOTHING`,
		iss.ID, iss.Key, teamID, taskID, ColInProgress.String(),
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

// RunBridge polls the ticket system on cfg.Interval and claims every
// unmapped intake issue. Tick errors are logged and the loop continues; the
// process ctx cancels it.
func RunBridge(ctx context.Context, pool *pgxpool.Pool, prov TicketProvider, cfg TicketsConfig) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	// MAQ-13: resolve the worktree repo root once. Unresolvable (no
	// MAQUINISTA_TICKETS_REPO, cwd not a checkout) is logged once here —
	// claims then go out without a worktree and the task scheduler parks
	// each one needs-human, so the failure stays loud without looping.
	if cfg.Repo == "" {
		if r, err := WorktreeRepo(); err != nil {
			log.Printf("pipeline: worktree repo root unresolved (%v); claims will park needs-human — set %s", err, WorktreeRepoEnv)
		} else {
			cfg.Repo = r
			log.Printf("pipeline: worktree repo root %s", cfg.Repo)
		}
	}
	log.Printf("pipeline: bridge polling team %s every %s (project %q, provider %s)", cfg.TeamID, cfg.Interval, cfg.Project, cfg.Provider)
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if err := claimReady(ctx, pool, prov, cfg); err != nil {
			log.Printf("pipeline: claim tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// claimReady is one intake tick: fetch intake issues, claim the unmapped ones.
func claimReady(ctx context.Context, pool *pgxpool.Pool, prov TicketProvider, cfg TicketsConfig) error {
	issues, err := prov.IntakeIssues(ctx, cfg.TeamID)
	if err != nil {
		return fmt.Errorf("fetching intake issues: %w", err)
	}
	for _, iss := range issues {
		wt := ensureClaimWorktree(cfg, iss)
		created, err := ClaimIssue(ctx, pool, iss, cfg.TeamID, cfg.Project, wt)
		if err != nil {
			log.Printf("pipeline: claim %s: %v", iss.Key, err)
			continue
		}
		if created {
			if wt != nil {
				log.Printf("pipeline: claimed %s (%s) with worktree %s (branch %s)", iss.Key, iss.Title, *wt, SlugFromKey(iss.Key))
			} else {
				log.Printf("pipeline: claimed %s (%s) WITHOUT worktree — scheduler will park it needs-human", iss.Key, iss.Title)
			}
		}
	}
	return nil
}

// ensureClaimWorktree provisions the sibling worktree for iss (MAQ-13).
// Best effort: nil on any failure — the task claims without a worktree_path
// and the task scheduler parks it needs-human exactly once (loud, no loop).
func ensureClaimWorktree(cfg TicketsConfig, iss Issue) *string {
	if cfg.Repo == "" {
		return nil
	}
	dir, err := EnsureIssueWorktree(cfg.Repo, iss)
	if err != nil {
		log.Printf("pipeline: claim %s: worktree: %v (claiming without — will park needs-human)", iss.Key, err)
		return nil
	}
	return &dir
}
