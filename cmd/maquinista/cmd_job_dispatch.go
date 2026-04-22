package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/agentspawn"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
	"github.com/maquinista-labs/maquinista/internal/scheduler"
)

// dispatchJobSpawn handles fresh-agent spawning for scheduled_jobs rows that
// have soul_template_id set. Called from the SpawnFunc hook in cmd_start.go.
func dispatchJobSpawn(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, spawner agentspawn.AgentSpawner, defaultCWD string, job scheduler.FiredJob) error {
	slug := agentspawn.SlugifyJobName(job.Name)
	agentID := fmt.Sprintf("job-%s-%s", slug, uuid.New().String()[:8])

	cwd := job.AgentCWD
	if cwd == "" {
		cwd = defaultCWD
	}

	// Spawn the agent (insert row, clone soul, open tmux pane).
	_, err := agentspawn.SpawnFresh(ctx, pool, cfg, agentspawn.FreshParams{
		AgentID:        agentID,
		CWD:            cwd,
		SoulTemplateID: job.SoulTemplateID,
	}, spawner)
	if err != nil {
		return fmt.Errorf("spawn agent: %w", err)
	}

	// Build and enqueue the first inbox message.
	text := buildJobPromptText(job)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	_, _, err = mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "job",
		FromID:        job.ID,
		OriginChannel: "scheduled",
		ExternalMsgID: fmt.Sprintf("job:%s:%s", job.ID, agentID),
		Content:       []byte(fmt.Sprintf(`{"type":"text","text":%s}`, jsonString(text))),
	})
	if err != nil {
		return fmt.Errorf("enqueue inbox: %w", err)
	}

	// Record execution.
	jobUUID, _ := uuid.Parse(job.ID)
	if _, execErr := tx.Exec(ctx, `
		INSERT INTO job_executions (job_id, agent_id) VALUES ($1, $2)
	`, jobUUID, agentID); execErr != nil {
		log.Printf("job dispatch: record execution: %v", execErr)
	}

	return tx.Commit(ctx)
}

// buildJobPromptText renders the job prompt as plain text, optionally
// prepending the context_markdown block.
func buildJobPromptText(job scheduler.FiredJob) string {
	var body struct {
		Text string `json:"text"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(job.Prompt, &body)
	prompt := body.Text
	if prompt == "" {
		prompt = string(job.Prompt)
	}
	if job.ContextMarkdown == "" {
		return prompt
	}
	return "## Context\n\n" + job.ContextMarkdown + "\n\n---\n\n" + prompt
}

// jsonString returns the JSON-encoded form of s (a JSON string literal).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// slugifyJobName delegates to agentspawn.SlugifyJobName.
func slugifyJobName(name string) string {
	return agentspawn.SlugifyJobName(name)
}
