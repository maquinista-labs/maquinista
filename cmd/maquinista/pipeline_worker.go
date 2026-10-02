package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/agentspawn"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
	"github.com/maquinista-labs/maquinista/internal/sidecar"
	"github.com/maquinista-labs/maquinista/internal/taskscheduler"
)

// ensureTaskWorker is the production EnsureAgent for the task-scheduler
// (EX-07): a task-bound implementor pane with the pipeline-worker soul,
// spawned through the FULL chain (agents row, soul clone, tmux window,
// sidecar inbox goroutine) via agentspawn.SpawnFresh — mirroring
// pipelineReviewerSpawner. The orchestrator.EnsureAgent stub used by the
// standalone task-scheduler subcommand only marks the row live; this
// adapter owns the whole spawn.
func ensureTaskWorker(pool *pgxpool.Pool, cfg *config.Config, sidecars *sidecar.Manager) taskscheduler.EnsureAgentFn {
	return func(ctx context.Context, role, taskID string) (string, error) {
		if sidecars == nil {
			return "", errNoSidecars
		}

		// SpawnFresh does not stat the CWD; a missing worktree would mint a
		// dead pane. Fail early with a readable error so DispatchOne reverts
		// the task to 'ready' for the next tick.
		var wt *string
		if err := pool.QueryRow(ctx,
			`SELECT worktree_path FROM tasks WHERE id = $1`, taskID,
		).Scan(&wt); err != nil {
			return "", fmt.Errorf("read task: %w", err)
		}
		if wt == nil || *wt == "" {
			return "", fmt.Errorf("task %s: no worktree_path", taskID)
		}
		if _, err := os.Stat(*wt); err != nil {
			return "", fmt.Errorf("task %s: worktree %s unusable: %w", taskID, *wt, err)
		}

		agentID, err := pipeline.MintWorkerID(ctx, pool, role, taskID)
		if err != nil {
			return "", fmt.Errorf("mint agent id: %w", err)
		}

		runner, model, err := pipeline.ResolveWorkerExec(ctx, pool)
		if err != nil {
			// Frozen exec contract unreadable — spawn on config defaults
			// rather than wedging the task (unlike reviewers, workers are
			// cheap and replaceable).
			log.Printf("task-scheduler: worker exec resolution failed (%v); using defaults", err)
			runner, model = "", ""
		}

		if _, err := agentspawn.SpawnFresh(ctx, pool, cfg, agentspawn.FreshParams{
			AgentID:        agentID,
			CWD:            *wt,
			SoulTemplateID: pipeline.WorkerSoulTemplate,
			RunnerType:     runner,
			Role:           role,
			TaskID:         taskID,
			ModelOverride:  model,
		}, sidecars); err != nil {
			return "", err
		}
		return agentID, nil
	}
}
