package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
)

// AgentSpawner is the narrow interface EnsureAgent uses to materialize a
// live pty. Tests inject a no-op spawner so the function is exercisable
// without tmux. Production code injects orchestratorSpawner, which calls
// agent.Spawn / agent.SpawnWithWorktree + starts a sidecar goroutine.
type AgentSpawner interface {
	Spawn(ctx context.Context, agentID, workingDir, role string) error
}

// AgentSpawnerFunc adapts a function to AgentSpawner.
type AgentSpawnerFunc func(ctx context.Context, agentID, workingDir, role string) error

// Spawn implements AgentSpawner.
func (f AgentSpawnerFunc) Spawn(ctx context.Context, agentID, workingDir, role string) error {
	return f(ctx, agentID, workingDir, role)
}

// EnsureAgentParams bundles the inputs to EnsureAgent.
type EnsureAgentParams struct {
	Pool    *pgxpool.Pool
	Spawner AgentSpawner
	Role    string
	TaskID  string
	// ObserverChannel / UserID / ThreadID / ChatID identify the topic that
	// triggered this task so it gets an observer binding on the new agent.
	// Any may be zero — the binding is skipped when ThreadID == "".
	ObserverUserID   string
	ObserverThreadID string
	ObserverChatID   *int64
}

// EnsureAgent implements §D.3. Returns the minted agent ID on success.
//
// Behavior:
//  1. Reads tasks.worktree_path; error if missing (don't leak a half-spawned pane).
//  2. Mints @impl-<task_id>, bumping the alias suffix (-r2, -r3, …) on collision.
//  3. Inserts the agents row with (task_id, role, status='working', stop_requested=FALSE).
//     The uq_agents_task_live partial index enforces at most one live agent per task.
//  4. Calls Spawner.Spawn for the pty + sidecar; rolls back on spawner failure.
//  5. Writes an observer topic_agent_bindings row so the originating topic sees the agent.
func EnsureAgent(ctx context.Context, p EnsureAgentParams) (string, error) {
	if p.Role == "" {
		return "", errors.New("ensure_agent: role required")
	}
	if p.TaskID == "" {
		return "", errors.New("ensure_agent: task_id required")
	}

	// Look up the task + worktree path.
	var worktree *string
	err := p.Pool.QueryRow(ctx, `SELECT worktree_path FROM tasks WHERE id = $1`, p.TaskID).Scan(&worktree)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("ensure_agent: task %q not found", p.TaskID)
	}
	if err != nil {
		return "", fmt.Errorf("ensure_agent: read task: %w", err)
	}
	if worktree == nil || *worktree == "" {
		return "", fmt.Errorf("ensure_agent: task %q has no worktree_path", p.TaskID)
	}
	// Fail loudly if the worktree directory doesn't exist — matches the
	// task's "don't leak a half-spawned pane" requirement.
	if _, statErr := os.Stat(*worktree); statErr != nil {
		return "", fmt.Errorf("ensure_agent: worktree %q unusable: %w", *worktree, statErr)
	}

	// Mint a unique agent id. Retries use -r2, -r3 etc.
	agentID, err := mintAgentID(ctx, p.Pool, p.Role, p.TaskID)
	if err != nil {
		return "", err
	}

	// Insert agents row. The uq_agents_task_live index (migration 011)
	// guarantees no sibling 'working' row for the same task exists.
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure_agent: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO agents
			(id, tmux_session, tmux_window, task_id, status, stop_requested, role, started_at, last_seen)
		VALUES ($1, 'maquinista', $1, $2, 'working', FALSE, $3, NOW(), NOW())
	`, agentID, p.TaskID, p.Role)
	if err != nil {
		// Concurrent caller won the unique-live slot — surface cleanly so
		// the caller can fetch + reuse the already-live agent.
		if strings.Contains(err.Error(), "uq_agents_task_live") {
			existing, lookupErr := lookupLiveAgent(ctx, p.Pool, p.TaskID)
			if lookupErr == nil && existing != "" {
				return existing, errAgentAlreadyLive
			}
		}
		return "", fmt.Errorf("ensure_agent: insert agents: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("ensure_agent: commit agents: %w", err)
	}

	// Spawn the pty + sidecar.
	if err := p.Spawner.Spawn(ctx, agentID, *worktree, p.Role); err != nil {
		// Mark the agents row dead so the unique-live index doesn't block retries.
		_ = db.UpdateAgentStatus(p.Pool, agentID, "dead")
		return "", fmt.Errorf("ensure_agent: spawner: %w", err)
	}

	// Observer binding for the originating topic.
	if p.ObserverThreadID != "" {
		var topicNum int64
		fmt.Sscanf(p.ObserverThreadID, "%d", &topicNum)
		_, err := p.Pool.Exec(ctx, `
			INSERT INTO topic_agent_bindings
				(topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
			VALUES ($1, $2, 'observer', $3, $4, $5)
			ON CONFLICT DO NOTHING
		`, topicNum, agentID, p.ObserverUserID, p.ObserverThreadID, p.ObserverChatID)
		if err != nil {
			// Non-fatal: the agent is live; log but return success.
			return agentID, fmt.Errorf("observer binding: %w", err)
		}
	}

	return agentID, nil
}

// ErrAgentAlreadyLive is returned when a concurrent caller won the
// unique-live slot. EnsureAgent returns the already-live agent id plus
// this sentinel so the caller can choose to reuse it.
var ErrAgentAlreadyLive = errAgentAlreadyLive

var errAgentAlreadyLive = errors.New("ensure_agent: another agent is already live for this task")

func lookupLiveAgent(ctx context.Context, pool *pgxpool.Pool, taskID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		SELECT id FROM agents
		WHERE task_id=$1 AND status!='dead'
		ORDER BY started_at DESC
		LIMIT 1
	`, taskID).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// mintAgentID picks the first @<alias>-<task_id>[-rN] variant not already
// present in the agents table. Uses a role-derived prefix ("impl" for
// implementor, otherwise the role itself truncated to 8 chars).
func mintAgentID(ctx context.Context, pool *pgxpool.Pool, role, taskID string) (string, error) {
	prefix := "impl"
	if role != "implementor" {
		prefix = role
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
	}
	base := fmt.Sprintf("%s-%s", prefix, taskID)
	for n := 1; n < 100; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s-r%d", base, n)
		}
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE id=$1)`, candidate).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("mintAgentID: exhausted retries for %s", base)
}
