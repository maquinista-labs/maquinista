package pipeline

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerSoulTemplate is the soul template the task-scheduler clones per
// implementor agent (EX-07; seeded by migration 035 alongside the reviewer,
// arbiter, fixer and merger souls).
const WorkerSoulTemplate = "pipeline-worker"

// WorkerRole is the default agents.role for scheduler-spawned implementors.
// tasks.metadata->>'role' overrides it (taskscheduler.DispatchOne).
const WorkerRole = "implementor"

// MintWorkerID mints <role>-<taskID>[-rN] ids for scheduler agents — same
// shape as dispatch's reviewer mint, exported so the cmd-side EnsureAgent
// adapter can use it without a cross-package dependency.
func MintWorkerID(ctx context.Context, pool *pgxpool.Pool, role, taskID string) (string, error) {
	return mintAgentID(ctx, pool, role, taskID)
}

// ResolveWorkerExec resolves the worker soul template's frozen exec contract
// (extras.default_runner + reasoning_class → runner override, model). Empty
// strings mean "use cfg.DefaultRunner" — agentspawn.SpawnFresh already
// implements that fallback, and ResolveWorkerExec never fails on a missing
// key, only on pool/template-read errors.
func ResolveWorkerExec(ctx context.Context, pool *pgxpool.Pool) (runnerOverride, model string, err error) {
	return resolveTemplateExecFor(ctx, pool, WorkerSoulTemplate)
}
