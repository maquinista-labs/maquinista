package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/agentspawn"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
	"github.com/maquinista-labs/maquinista/internal/sidecar"
)

// pipelineReviewerSpawner adapts agentspawn.SpawnFresh to the dispatch
// loop's ReviewSpawner interface (EX-03): a task-bound reviewer pane with
// the pipeline-reviewer soul, started with a sidecar inbox goroutine.
type pipelineReviewerSpawner struct {
	pool     *pgxpool.Pool
	cfg      *config.Config
	sidecars *sidecar.Manager
}

// errNoSidecars parks reviewer spawning when the sidecar manager is
// unavailable (panes without inbox goroutines can never receive the
// review prompt) — the task stays in 'review' and retries next tick.
var errNoSidecars = errors.New("sidecar manager unavailable, reviewer spawn skipped")

// SpawnReviewer implements pipeline.ReviewSpawner. Role/SoulTemplateID
// default to the reviewer pair when empty (EX-03 callers); EX-04's fixer
// pass sets both explicitly.
func (s pipelineReviewerSpawner) SpawnReviewer(ctx context.Context, p pipeline.ReviewSpawnParams) error {
	if s.sidecars == nil {
		return errNoSidecars
	}
	role := p.Role
	if role == "" {
		role = "reviewer"
	}
	template := p.SoulTemplateID
	if template == "" {
		template = pipeline.ReviewerSoulTemplate
	}
	_, err := agentspawn.SpawnFresh(ctx, s.pool, s.cfg, agentspawn.FreshParams{
		AgentID:        p.AgentID,
		CWD:            p.WorktreePath,
		SoulTemplateID: template,
		RunnerType:     p.RunnerType,
		Role:           role,
		TaskID:         p.TaskID,
		ModelOverride:  p.Model,
	}, s.sidecars)
	return err
}
