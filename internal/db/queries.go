package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Task represents a work unit.
type Task struct {
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	Body             string          `json:"body"`
	Status           string          `json:"status"`
	Priority         int             `json:"priority"`
	ClaimedBy        *string         `json:"claimed_by,omitempty"`
	ClaimedAt        *time.Time      `json:"claimed_at,omitempty"`
	DoneAt           *time.Time      `json:"done_at,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	Attempt          int             `json:"attempt"`
	MaxAttempts      int             `json:"max_attempts"`
	ProjectID        *string         `json:"project_id,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	RequiresApproval bool            `json:"requires_approval"`
	ApprovedBy       *string         `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time      `json:"approved_at,omitempty"`
	RejectionReason  *string         `json:"rejection_reason,omitempty"`
}

// TaskContext represents a persistent context entry for a task.
type TaskContext struct {
	ID         int64      `json:"id"`
	TaskID     string     `json:"task_id"`
	AgentID    *string    `json:"agent_id,omitempty"`
	Kind       string     `json:"kind"`
	Content    string     `json:"content"`
	SourceTask *string    `json:"source_task,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Agent represents a running agent instance.
type Agent struct {
	ID           string     `json:"id"`
	TmuxSession  string     `json:"tmux_session"`
	TmuxWindow   string     `json:"tmux_window"`
	TaskID       *string    `json:"task_id,omitempty"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	LastSeen     *time.Time `json:"last_seen,omitempty"`
	WorktreeDir  *string    `json:"worktree_dir,omitempty"`
	Branch       *string    `json:"branch,omitempty"`
	RunnerType   string     `json:"runner_type"`
	RunnerConfig *string    `json:"runner_config,omitempty"`
	Role         string     `json:"role"`
}

// MergeQueueEntry represents an entry in the merge queue.
type MergeQueueEntry struct {
	ID            int64      `json:"id"`
	TaskID        string     `json:"task_id"`
	AgentID       string     `json:"agent_id"`
	Branch        string     `json:"branch"`
	WorktreeDir   string     `json:"worktree_dir"`
	BaseBranch    string     `json:"base_branch"`
	Status        string     `json:"status"`
	CommitSHA     *string    `json:"commit_sha,omitempty"`
	MergeSHA      *string    `json:"merge_sha,omitempty"`
	ConflictFiles []string   `json:"conflict_files,omitempty"`
	ErrorMsg      *string    `json:"error_msg,omitempty"`
	EnqueuedAt    time.Time  `json:"enqueued_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// taskColumns is the canonical SELECT column list for tasks.
const taskColumns = `id, title, body, status, priority, claimed_by, claimed_at,
		       done_at, created_at, attempt, max_attempts, project_id, metadata,
		       requires_approval, approved_by, approved_at, rejection_reason`

// scanTask scans a single task row (must match taskColumns order).
func scanTask(row pgx.Row) (Task, error) {
	var t Task
	err := row.Scan(
		&t.ID, &t.Title, &t.Body, &t.Status, &t.Priority,
		&t.ClaimedBy, &t.ClaimedAt, &t.DoneAt, &t.CreatedAt, &t.Attempt,
		&t.MaxAttempts, &t.ProjectID, &t.Metadata,
		&t.RequiresApproval, &t.ApprovedBy, &t.ApprovedAt, &t.RejectionReason,
	)
	return t, err
}

// TreeNode is a task with its children for tree rendering.
type TreeNode struct {
	Task     *Task
	Children []*TreeNode
}

// CreateTask inserts a new task.
func CreateTask(pool *pgxpool.Pool, id, title, body string, priority int, projectID *string, metadata json.RawMessage, requiresApproval bool) error {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO tasks (id, title, body, priority, project_id, metadata, requires_approval)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, id, title, body, priority, projectID, metadata, requiresApproval)
	if err != nil {
		return fmt.Errorf("creating task: %w", err)
	}
	return nil
}

// SetTaskStatus sets a task's status directly.
func SetTaskStatus(pool *pgxpool.Pool, id, status string) error {
	_, err := pool.Exec(context.Background(), `
		UPDATE tasks SET status = $2 WHERE id = $1
	`, id, status)
	if err != nil {
		return fmt.Errorf("setting task status: %w", err)
	}
	return nil
}

// AddDependency creates a dependency edge.
func AddDependency(pool *pgxpool.Pool, taskID, dependsOn string) error {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO task_deps (task_id, depends_on) VALUES ($1, $2)
	`, taskID, dependsOn)
	if err != nil {
		return fmt.Errorf("adding dependency: %w", err)
	}
	return nil
}

// ResolvePartialID finds a single task ID matching the given prefix.
// Returns an error if zero or multiple tasks match.
func ResolvePartialID(pool *pgxpool.Pool, prefix string) (string, error) {
	rows, err := pool.Query(context.Background(), `
		SELECT id FROM tasks WHERE id LIKE $1 || '%' LIMIT 2
	`, prefix)
	if err != nil {
		return "", fmt.Errorf("resolving partial id: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("scanning id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating ids: %w", err)
	}

	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no task found matching %q", prefix)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches multiple tasks", prefix)
	}
}

// GetTask retrieves a task by exact or partial ID.
func GetTask(pool *pgxpool.Pool, id string) (*Task, error) {
	resolvedID, err := ResolvePartialID(pool, id)
	if err != nil {
		return nil, err
	}

	t, err := scanTask(pool.QueryRow(context.Background(),
		`SELECT `+taskColumns+` FROM tasks WHERE id = $1`, resolvedID))
	if err != nil {
		return nil, fmt.Errorf("getting task %s: %w", resolvedID, err)
	}
	return &t, nil
}

// ListTasks returns all tasks, optionally filtered by project.
func ListTasks(pool *pgxpool.Pool, projectID *string) ([]*Task, error) {
	var rows pgx.Rows
	var err error

	if projectID != nil {
		rows, err = pool.Query(context.Background(),
			`SELECT `+taskColumns+` FROM tasks WHERE project_id = $1
			ORDER BY priority DESC, created_at ASC`, *projectID)
	} else {
		rows, err = pool.Query(context.Background(),
			`SELECT `+taskColumns+` FROM tasks
			ORDER BY priority DESC, created_at ASC`)
	}
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	defer rows.Close()

	return scanTasks(rows)
}

// GetTaskWithContext retrieves a task and all its context entries.
func GetTaskWithContext(pool *pgxpool.Pool, id string) (*Task, []*TaskContext, error) {
	task, err := GetTask(pool, id)
	if err != nil {
		return nil, nil, err
	}

	rows, err := pool.Query(context.Background(), `
		SELECT id, task_id, agent_id, kind, content, source_task, created_at
		FROM task_context
		WHERE task_id = $1
		ORDER BY created_at ASC
	`, task.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("getting task context: %w", err)
	}
	defer rows.Close()

	var ctxs []*TaskContext
	for rows.Next() {
		var c TaskContext
		if err := rows.Scan(&c.ID, &c.TaskID, &c.AgentID, &c.Kind, &c.Content, &c.SourceTask, &c.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scanning context: %w", err)
		}
		ctxs = append(ctxs, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating context: %w", err)
	}

	return task, ctxs, nil
}

// AtomicClaim atomically claims one ready task, injects inherited context, and updates the agent.
// Returns nil if no task is available. When projectID is non-nil, only claims from that project.
func AtomicClaim(pool *pgxpool.Pool, agentID string, projectID *string) (*Task, error) {
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning claim tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var proj interface{}
	if projectID != nil {
		proj = *projectID
	}

	t, claimErr := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks
		SET    status     = 'claimed',
		       claimed_by = $1,
		       claimed_at = NOW(),
		       attempt    = attempt + 1
		WHERE  id = (
			SELECT id FROM tasks
			WHERE  status = 'ready'
			  AND  ($2::text IS NULL OR project_id = $2)
			  AND  attempt < max_attempts
			ORDER  BY priority DESC, created_at ASC
			LIMIT  1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+taskColumns+`
	`, agentID, proj))
	if claimErr == pgx.ErrNoRows {
		return nil, nil // No task available.
	}
	if claimErr != nil {
		return nil, fmt.Errorf("claiming task: %w", claimErr)
	}

	// Inject inherited context from done dependencies.
	_, err = tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content, source_task)
		SELECT $1, $2, 'inherited', tc.content, tc.task_id
		FROM   task_deps td
		JOIN   task_context tc ON tc.task_id = td.depends_on
		                      AND tc.kind IN ('result', 'observation', 'handoff', 'test_failure')
		JOIN   tasks dep       ON dep.id = td.depends_on
		                      AND dep.status = 'done'
		WHERE  td.task_id = $1
	`, t.ID, agentID)
	if err != nil {
		return nil, fmt.Errorf("injecting inherited context: %w", err)
	}

	// Update agent status.
	_, err = tx.Exec(ctx, `
		UPDATE agents
		SET    task_id   = $2,
		       status    = 'working',
		       last_seen = NOW()
		WHERE  id = $1
	`, agentID, t.ID)
	if err != nil {
		return nil, fmt.Errorf("updating agent: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing claim: %w", err)
	}

	return &t, nil
}

// ClaimByID claims a specific task by ID (with partial matching).
// Returns an error if the task is not found, not ready, or has reached max attempts.
func ClaimByID(pool *pgxpool.Pool, taskID, agentID string) (*Task, error) {
	resolvedID, err := ResolvePartialID(pool, taskID)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning claim tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Verify task is claimable and claim it.
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks
		SET    status     = 'claimed',
		       claimed_by = $1,
		       claimed_at = NOW(),
		       attempt    = attempt + 1
		WHERE  id         = $2
		  AND  status     = 'ready'
		  AND  attempt    < max_attempts
		RETURNING `+taskColumns+`
	`, agentID, resolvedID))
	if err == pgx.ErrNoRows {
		// Determine reason for failure.
		var status string
		var attempt, maxAttempts int
		scanErr := pool.QueryRow(ctx, `SELECT status, attempt, max_attempts FROM tasks WHERE id = $1`, resolvedID).Scan(&status, &attempt, &maxAttempts)
		if scanErr != nil {
			return nil, fmt.Errorf("task %q not found", resolvedID)
		}
		if attempt >= maxAttempts {
			return nil, fmt.Errorf("task %q has reached max attempts (%d/%d)", resolvedID, attempt, maxAttempts)
		}
		return nil, fmt.Errorf("task %q is not ready (status: %s)", resolvedID, status)
	}
	if err != nil {
		return nil, fmt.Errorf("claiming task: %w", err)
	}

	// Inject inherited context from done dependencies.
	_, err = tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content, source_task)
		SELECT $1, $2, 'inherited', tc.content, tc.task_id
		FROM   task_deps td
		JOIN   task_context tc ON tc.task_id = td.depends_on
		                      AND tc.kind IN ('result', 'observation', 'handoff', 'test_failure')
		JOIN   tasks dep       ON dep.id = td.depends_on
		                      AND dep.status = 'done'
		WHERE  td.task_id = $1
	`, t.ID, agentID)
	if err != nil {
		return nil, fmt.Errorf("injecting inherited context: %w", err)
	}

	// Update agent status if agent is registered.
	_, err = tx.Exec(ctx, `
		UPDATE agents
		SET    task_id   = $2,
		       status    = 'working',
		       last_seen = NOW()
		WHERE  id = $1
	`, agentID, t.ID)
	if err != nil {
		return nil, fmt.Errorf("updating agent: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing claim: %w", err)
	}

	return &t, nil
}

// MarkDone marks a task as done after tests pass, recording the result summary.
func MarkDone(pool *pgxpool.Pool, taskID, agentID, summary string) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning done tx: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'result', $3)
	`, taskID, agentID, summary)
	if err != nil {
		return fmt.Errorf("recording result: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE tasks
		SET    status     = 'done',
		       done_at    = NOW(),
		       claimed_by = NULL,
		       claimed_at = NULL
		WHERE  id         = $1
		  AND  claimed_by = $2
	`, taskID, agentID)
	if err != nil {
		return fmt.Errorf("marking done: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE agents SET task_id = NULL, status = 'idle', last_seen = NOW()
		WHERE id = $1
	`, agentID)
	if err != nil {
		return fmt.Errorf("releasing agent: %w", err)
	}

	return tx.Commit(ctx)
}

// RecordFailure records a test failure and resets the task to ready, or marks it failed
// if max attempts reached.
func RecordFailure(pool *pgxpool.Pool, taskID, agentID, output string) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning failure tx: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'test_failure', $3)
	`, taskID, agentID, output)
	if err != nil {
		return fmt.Errorf("recording failure: %w", err)
	}

	// Reset to ready if under max attempts.
	_, err = tx.Exec(ctx, `
		UPDATE tasks
		SET    status     = 'ready',
		       claimed_by = NULL,
		       claimed_at = NULL
		WHERE  id         = $1
		  AND  claimed_by = $2
		  AND  attempt    < max_attempts
	`, taskID, agentID)
	if err != nil {
		return fmt.Errorf("resetting task: %w", err)
	}

	// Mark failed if at max attempts.
	_, err = tx.Exec(ctx, `
		UPDATE tasks SET status = 'failed'
		WHERE id = $1 AND attempt >= max_attempts
	`, taskID)
	if err != nil {
		return fmt.Errorf("marking failed: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE agents SET task_id = NULL, status = 'idle', last_seen = NOW()
		WHERE id = $1
	`, agentID)
	if err != nil {
		return fmt.Errorf("releasing agent: %w", err)
	}

	return tx.Commit(ctx)
}

// ReclaimStale resets tasks that have been claimed for longer than the given minutes.
// Returns the number of reclaimed tasks.
func ReclaimStale(pool *pgxpool.Pool, minutes int) (int, error) {
	tag, err := pool.Exec(context.Background(), `
		UPDATE tasks
		SET    status     = 'ready',
		       claimed_by = NULL,
		       claimed_at = NULL
		WHERE  status     = 'claimed'
		  AND  claimed_at < NOW() - make_interval(mins => $1)
	`, minutes)
	if err != nil {
		return 0, fmt.Errorf("reclaiming stale tasks: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// AddObservation records an observation for a task.
func AddObservation(pool *pgxpool.Pool, taskID, agentID, content string) error {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'observation', $3)
	`, taskID, agentID, content)
	if err != nil {
		return fmt.Errorf("adding observation: %w", err)
	}
	return nil
}

// AddHandoff records a handoff note for a task.
func AddHandoff(pool *pgxpool.Pool, taskID, agentID, content string) error {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'handoff', $3)
	`, taskID, agentID, content)
	if err != nil {
		return fmt.Errorf("adding handoff: %w", err)
	}
	return nil
}

// GetDependencyTree builds a forest of TreeNodes for tree rendering.
func GetDependencyTree(pool *pgxpool.Pool, projectID *string) ([]*TreeNode, error) {
	tasks, err := ListTasks(pool, projectID)
	if err != nil {
		return nil, err
	}

	// Load all deps.
	rows, err := pool.Query(context.Background(), `SELECT task_id, depends_on FROM task_deps`)
	if err != nil {
		return nil, fmt.Errorf("loading deps: %w", err)
	}
	defer rows.Close()

	// Map of taskID -> list of dependency IDs (parents).
	parents := make(map[string][]string)
	children := make(map[string][]string)
	for rows.Next() {
		var taskID, depID string
		if err := rows.Scan(&taskID, &depID); err != nil {
			return nil, fmt.Errorf("scanning dep: %w", err)
		}
		parents[taskID] = append(parents[taskID], depID)
		children[depID] = append(children[depID], taskID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating deps: %w", err)
	}

	// Build node map.
	nodeMap := make(map[string]*TreeNode)
	for _, t := range tasks {
		nodeMap[t.ID] = &TreeNode{Task: t}
	}

	// Wire children.
	for _, t := range tasks {
		for _, childID := range children[t.ID] {
			if child, ok := nodeMap[childID]; ok {
				nodeMap[t.ID].Children = append(nodeMap[t.ID].Children, child)
			}
		}
	}

	// Roots are tasks with no parents.
	var roots []*TreeNode
	for _, t := range tasks {
		if len(parents[t.ID]) == 0 {
			roots = append(roots, nodeMap[t.ID])
		}
	}

	return roots, nil
}

// SearchContext performs full-text search across task context.
func SearchContext(pool *pgxpool.Pool, query string) ([]*TaskContext, error) {
	rows, err := pool.Query(context.Background(), `
		SELECT tc.id, tc.task_id, tc.agent_id, tc.kind, tc.content, tc.source_task, tc.created_at
		FROM task_context tc
		WHERE to_tsvector('english', tc.content) @@ plainto_tsquery('english', $1)
		ORDER BY ts_rank(to_tsvector('english', tc.content), plainto_tsquery('english', $1)) DESC
	`, query)
	if err != nil {
		return nil, fmt.Errorf("searching context: %w", err)
	}
	defer rows.Close()

	var results []*TaskContext
	for rows.Next() {
		var c TaskContext
		if err := rows.Scan(&c.ID, &c.TaskID, &c.AgentID, &c.Kind, &c.Content, &c.SourceTask, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning search result: %w", err)
		}
		results = append(results, &c)
	}
	return results, rows.Err()
}

// RegisterAgent inserts a new agent record.
func RegisterAgent(pool *pgxpool.Pool, id, tmuxSession, tmuxWindow string, worktreeDir, branch *string, runnerType string, runnerConfig *string, role string) error {
	if runnerType == "" {
		runnerType = "claude"
	}
	if role == "" {
		role = "executor"
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO agents (id, tmux_session, tmux_window, last_seen, worktree_dir, branch, runner_type, runner_config, role)
		VALUES ($1, $2, $3, NOW(), $4, $5, $6, $7, $8)
	`, id, tmuxSession, tmuxWindow, worktreeDir, branch, runnerType, runnerConfig, role)
	if err != nil {
		return fmt.Errorf("registering agent: %w", err)
	}
	return nil
}

// ListAgents returns all registered agents.
func ListAgents(pool *pgxpool.Pool) ([]*Agent, error) {
	rows, err := pool.Query(context.Background(), `
		SELECT id, tmux_session, tmux_window, task_id, status, started_at, last_seen,
		       worktree_dir, branch, runner_type, runner_config, role
		FROM agents
		ORDER BY started_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	defer rows.Close()

	var agents []*Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.TmuxSession, &a.TmuxWindow, &a.TaskID, &a.Status, &a.StartedAt, &a.LastSeen,
			&a.WorktreeDir, &a.Branch, &a.RunnerType, &a.RunnerConfig, &a.Role); err != nil {
			return nil, fmt.Errorf("scanning agent: %w", err)
		}
		agents = append(agents, &a)
	}
	return agents, rows.Err()
}

// ListAgentsByRole returns all agents with the given role.
func ListAgentsByRole(pool *pgxpool.Pool, role string) ([]*Agent, error) {
	rows, err := pool.Query(context.Background(), `
		SELECT id, tmux_session, tmux_window, task_id, status, started_at, last_seen,
		       worktree_dir, branch, runner_type, runner_config, role
		FROM agents
		WHERE role = $1
		ORDER BY started_at ASC
	`, role)
	if err != nil {
		return nil, fmt.Errorf("listing agents by role: %w", err)
	}
	defer rows.Close()

	var agents []*Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.TmuxSession, &a.TmuxWindow, &a.TaskID, &a.Status, &a.StartedAt, &a.LastSeen,
			&a.WorktreeDir, &a.Branch, &a.RunnerType, &a.RunnerConfig, &a.Role); err != nil {
			return nil, fmt.Errorf("scanning agent: %w", err)
		}
		agents = append(agents, &a)
	}
	return agents, rows.Err()
}

// UpdateAgentStatus updates an agent's status and last_seen.
func UpdateAgentStatus(pool *pgxpool.Pool, id, status string) error {
	_, err := pool.Exec(context.Background(), `
		UPDATE agents SET status = $2, last_seen = NOW() WHERE id = $1
	`, id, status)
	if err != nil {
		return fmt.Errorf("updating agent status: %w", err)
	}
	return nil
}

// DeleteAgent removes an agent and releases any claimed task.
func DeleteAgent(pool *pgxpool.Pool, id string) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning delete tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Release any claimed task.
	_, err = tx.Exec(ctx, `
		UPDATE tasks
		SET    status     = 'ready',
		       claimed_by = NULL,
		       claimed_at = NULL
		WHERE  claimed_by = $1
		  AND  status     = 'claimed'
	`, id)
	if err != nil {
		return fmt.Errorf("releasing agent tasks: %w", err)
	}

	_, err = tx.Exec(ctx, `DELETE FROM agents WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("deleting agent: %w", err)
	}

	return tx.Commit(ctx)
}

// UpdateTask updates a task's title and body (for roda edit).
func UpdateTask(pool *pgxpool.Pool, id, title, body string) error {
	_, err := pool.Exec(context.Background(), `
		UPDATE tasks SET title = $2, body = $3 WHERE id = $1
	`, id, title, body)
	if err != nil {
		return fmt.Errorf("updating task: %w", err)
	}
	return nil
}

// HasUnmetDeps returns true if any of the task's dependencies are not yet done.
func HasUnmetDeps(pool *pgxpool.Pool, taskID string) (bool, error) {
	var count int
	err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*)
		FROM task_deps td
		JOIN tasks t ON t.id = td.depends_on
		WHERE td.task_id = $1 AND t.status != 'done'
	`, taskID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking unmet deps: %w", err)
	}
	return count > 0, nil
}

// GetAgentByTaskID finds the agent currently working on a given task.
func GetAgentByTaskID(pool *pgxpool.Pool, taskID string) (*Agent, error) {
	var a Agent
	err := pool.QueryRow(context.Background(), `
		SELECT id, tmux_session, tmux_window, task_id, status, started_at, last_seen,
		       worktree_dir, branch, runner_type, runner_config, role
		FROM agents WHERE task_id = $1
	`, taskID).Scan(&a.ID, &a.TmuxSession, &a.TmuxWindow, &a.TaskID, &a.Status, &a.StartedAt, &a.LastSeen,
		&a.WorktreeDir, &a.Branch, &a.RunnerType, &a.RunnerConfig, &a.Role)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting agent by task: %w", err)
	}
	return &a, nil
}

// GetAgent retrieves a single agent by ID.
func GetAgent(pool *pgxpool.Pool, id string) (*Agent, error) {
	var a Agent
	err := pool.QueryRow(context.Background(), `
		SELECT id, tmux_session, tmux_window, task_id, status, started_at, last_seen,
		       worktree_dir, branch, runner_type, runner_config, role
		FROM agents WHERE id = $1
	`, id).Scan(&a.ID, &a.TmuxSession, &a.TmuxWindow, &a.TaskID, &a.Status, &a.StartedAt, &a.LastSeen,
		&a.WorktreeDir, &a.Branch, &a.RunnerType, &a.RunnerConfig, &a.Role)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting agent: %w", err)
	}
	return &a, nil
}

// EnqueueMerge adds an entry to the merge queue.
func EnqueueMerge(pool *pgxpool.Pool, taskID, agentID, branch, worktreeDir, baseBranch, commitSHA string) error {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, commit_sha)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, taskID, agentID, branch, worktreeDir, baseBranch, commitSHA)
	if err != nil {
		return fmt.Errorf("enqueuing merge: %w", err)
	}
	return nil
}

// ClaimMergeEntry atomically claims the next pending merge queue entry.
// Returns nil if no entry is available.
func ClaimMergeEntry(pool *pgxpool.Pool) (*MergeQueueEntry, error) {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning merge claim tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var e MergeQueueEntry
	err = tx.QueryRow(ctx, `
		UPDATE merge_queue
		SET    status     = 'merging',
		       started_at = NOW()
		WHERE  id = (
			SELECT id FROM merge_queue
			WHERE  status = 'pending'
			ORDER  BY enqueued_at ASC
			LIMIT  1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, task_id, agent_id, branch, worktree_dir, base_branch, status,
		          commit_sha, merge_sha, conflict_files, error_msg,
		          enqueued_at, started_at, completed_at
	`).Scan(
		&e.ID, &e.TaskID, &e.AgentID, &e.Branch, &e.WorktreeDir, &e.BaseBranch, &e.Status,
		&e.CommitSHA, &e.MergeSHA, &e.ConflictFiles, &e.ErrorMsg,
		&e.EnqueuedAt, &e.StartedAt, &e.CompletedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claiming merge entry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing merge claim: %w", err)
	}
	return &e, nil
}

// CompleteMerge marks a merge queue entry as merged.
func CompleteMerge(pool *pgxpool.Pool, id int64, mergeSHA string) error {
	_, err := pool.Exec(context.Background(), `
		UPDATE merge_queue
		SET    status       = 'merged',
		       merge_sha    = $2,
		       completed_at = NOW()
		WHERE  id = $1
	`, id, mergeSHA)
	if err != nil {
		return fmt.Errorf("completing merge: %w", err)
	}
	return nil
}

// ConflictMerge marks a merge queue entry as conflicted.
func ConflictMerge(pool *pgxpool.Pool, id int64, conflictFiles []string) error {
	_, err := pool.Exec(context.Background(), `
		UPDATE merge_queue
		SET    status         = 'conflict',
		       conflict_files = $2,
		       completed_at   = NOW()
		WHERE  id = $1
	`, id, conflictFiles)
	if err != nil {
		return fmt.Errorf("recording merge conflict: %w", err)
	}
	return nil
}

// FailMerge marks a merge queue entry as failed.
func FailMerge(pool *pgxpool.Pool, id int64, errMsg string) error {
	_, err := pool.Exec(context.Background(), `
		UPDATE merge_queue
		SET    status       = 'failed',
		       error_msg    = $2,
		       completed_at = NOW()
		WHERE  id = $1
	`, id, errMsg)
	if err != nil {
		return fmt.Errorf("failing merge: %w", err)
	}
	return nil
}

// ListMergeQueue returns all merge queue entries, ordered by enqueue time.
func ListMergeQueue(pool *pgxpool.Pool) ([]*MergeQueueEntry, error) {
	rows, err := pool.Query(context.Background(), `
		SELECT id, task_id, agent_id, branch, worktree_dir, base_branch, status,
		       commit_sha, merge_sha, conflict_files, error_msg,
		       enqueued_at, started_at, completed_at
		FROM merge_queue
		ORDER BY enqueued_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing merge queue: %w", err)
	}
	defer rows.Close()

	var entries []*MergeQueueEntry
	for rows.Next() {
		var e MergeQueueEntry
		if err := rows.Scan(
			&e.ID, &e.TaskID, &e.AgentID, &e.Branch, &e.WorktreeDir, &e.BaseBranch, &e.Status,
			&e.CommitSHA, &e.MergeSHA, &e.ConflictFiles, &e.ErrorMsg,
			&e.EnqueuedAt, &e.StartedAt, &e.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning merge entry: %w", err)
		}
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

// ApproveTask transitions a task from pending_approval to ready.
func ApproveTask(pool *pgxpool.Pool, taskID, approvedBy string) error {
	tag, err := pool.Exec(context.Background(), `
		UPDATE tasks
		SET    status      = 'ready',
		       approved_by = $2,
		       approved_at = NOW()
		WHERE  id     = $1
		  AND  status = 'pending_approval'
	`, taskID, approvedBy)
	if err != nil {
		return fmt.Errorf("approving task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is not pending_approval", taskID)
	}
	return nil
}

// RejectTask transitions a task from pending_approval to rejected.
func RejectTask(pool *pgxpool.Pool, taskID, reason string) error {
	tag, err := pool.Exec(context.Background(), `
		UPDATE tasks
		SET    status           = 'rejected',
		       rejection_reason = $2
		WHERE  id     = $1
		  AND  status = 'pending_approval'
	`, taskID, reason)
	if err != nil {
		return fmt.Errorf("rejecting task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is not pending_approval", taskID)
	}
	return nil
}

// UnclaimTask releases a claimed task back to ready.
func UnclaimTask(pool *pgxpool.Pool, taskID string) error {
	tag, err := pool.Exec(context.Background(), `
		UPDATE tasks
		SET    status     = 'ready',
		       claimed_by = NULL,
		       claimed_at = NULL
		WHERE  id     = $1
		  AND  status = 'claimed'
	`, taskID)
	if err != nil {
		return fmt.Errorf("unclaiming task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is not claimed", taskID)
	}
	return nil
}

// DraftRelease transitions a single task from draft to ready (respecting deps).
func DraftRelease(pool *pgxpool.Pool, taskID string) error {
	// Check if task has unmet deps — if so, transition to pending instead.
	hasUnmet, err := HasUnmetDeps(pool, taskID)
	if err != nil {
		return err
	}
	targetStatus := "ready"
	if hasUnmet {
		targetStatus = "pending"
	}

	tag, err := pool.Exec(context.Background(), `
		UPDATE tasks SET status = $2 WHERE id = $1 AND status = 'draft'
	`, taskID, targetStatus)
	if err != nil {
		return fmt.Errorf("releasing draft task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is not in draft status", taskID)
	}
	return nil
}

// DraftReleaseAll transitions all draft tasks in a project.
// Tasks with unmet deps go to pending; tasks with all deps met go to ready.
func DraftReleaseAll(pool *pgxpool.Pool, projectID string) (int, error) {
	// First, move drafts with unmet deps to pending.
	pool.Exec(context.Background(), `
		UPDATE tasks
		SET    status = 'pending'
		WHERE  status     = 'draft'
		  AND  project_id = $1
		  AND  id IN (
			SELECT td.task_id
			FROM task_deps td
			JOIN tasks dep ON dep.id = td.depends_on
			WHERE dep.status != 'done'
		  )
	`, projectID)

	// Then, move remaining drafts (no unmet deps) to ready.
	tag, err := pool.Exec(context.Background(), `
		UPDATE tasks
		SET    status = 'ready'
		WHERE  status     = 'draft'
		  AND  project_id = $1
	`, projectID)
	if err != nil {
		return 0, fmt.Errorf("releasing draft tasks: %w", err)
	}

	// Count total released (both pending + ready).
	var count int
	pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM tasks
		WHERE project_id = $1 AND status IN ('pending', 'ready')
		  AND created_at > NOW() - INTERVAL '1 minute'
	`, projectID).Scan(&count)

	return int(tag.RowsAffected()), nil
}

// PlannerSession represents a planner session.
type PlannerSession struct {
	ID          string     `json:"id"`
	TopicID     int64      `json:"topic_id"`
	ProjectID   *string    `json:"project_id,omitempty"`
	TmuxWindow  *string    `json:"tmux_window,omitempty"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	StoppedAt   *time.Time `json:"stopped_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// UpsertPlannerSession creates or updates a planner session.
func UpsertPlannerSession(pool *pgxpool.Pool, topicID int64, projectID, tmuxWindow, status string) error {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO planner_sessions (topic_id, project_id, tmux_window, status, started_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (topic_id) DO UPDATE SET
			tmux_window = $3,
			status      = $4,
			started_at  = NOW(),
			stopped_at  = NULL
	`, topicID, projectID, tmuxWindow, status)
	if err != nil {
		return fmt.Errorf("upserting planner session: %w", err)
	}
	return nil
}

// StopPlannerSession marks a planner session as stopped.
func StopPlannerSession(pool *pgxpool.Pool, topicID int64) error {
	tag, err := pool.Exec(context.Background(), `
		UPDATE planner_sessions
		SET    status     = 'stopped',
		       stopped_at = NOW()
		WHERE  topic_id = $1
		  AND  status   IN ('running', 'crashed')
	`, topicID)
	if err != nil {
		return fmt.Errorf("stopping planner session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no active planner session for topic %d", topicID)
	}
	return nil
}

// ReopenPlannerSession re-activates a stopped or crashed planner session.
func ReopenPlannerSession(pool *pgxpool.Pool, topicID int64, tmuxWindow string) (*PlannerSession, error) {
	var s PlannerSession
	err := pool.QueryRow(context.Background(), `
		UPDATE planner_sessions
		SET    status      = 'running',
		       tmux_window = $2,
		       started_at  = NOW(),
		       stopped_at  = NULL
		WHERE  topic_id = $1
		  AND  status   IN ('stopped', 'crashed')
		RETURNING id, topic_id, project_id, tmux_window, status, started_at, stopped_at, created_at
	`, topicID, tmuxWindow).Scan(
		&s.ID, &s.TopicID, &s.ProjectID, &s.TmuxWindow, &s.Status,
		&s.StartedAt, &s.StoppedAt, &s.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("no planner session found for topic %d", topicID)
	}
	if err != nil {
		return nil, fmt.Errorf("reopening planner session: %w", err)
	}
	return &s, nil
}

// GetPlannerSession retrieves a planner session by topic ID.
func GetPlannerSession(pool *pgxpool.Pool, topicID int64) (*PlannerSession, error) {
	var s PlannerSession
	err := pool.QueryRow(context.Background(), `
		SELECT id, topic_id, project_id, tmux_window, status, started_at, stopped_at, created_at
		FROM planner_sessions WHERE topic_id = $1
	`, topicID).Scan(
		&s.ID, &s.TopicID, &s.ProjectID, &s.TmuxWindow, &s.Status,
		&s.StartedAt, &s.StoppedAt, &s.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting planner session: %w", err)
	}
	return &s, nil
}

// ListPlannerSessions lists all planner sessions, optionally filtered by project.
func ListPlannerSessions(pool *pgxpool.Pool, projectID *string) ([]*PlannerSession, error) {
	var rows pgx.Rows
	var err error
	if projectID != nil {
		rows, err = pool.Query(context.Background(), `
			SELECT id, topic_id, project_id, tmux_window, status, started_at, stopped_at, created_at
			FROM planner_sessions WHERE project_id = $1
			ORDER BY created_at DESC
		`, *projectID)
	} else {
		rows, err = pool.Query(context.Background(), `
			SELECT id, topic_id, project_id, tmux_window, status, started_at, stopped_at, created_at
			FROM planner_sessions ORDER BY created_at DESC
		`)
	}
	if err != nil {
		return nil, fmt.Errorf("listing planner sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*PlannerSession
	for rows.Next() {
		var s PlannerSession
		if err := rows.Scan(&s.ID, &s.TopicID, &s.ProjectID, &s.TmuxWindow, &s.Status,
			&s.StartedAt, &s.StoppedAt, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning planner session: %w", err)
		}
		sessions = append(sessions, &s)
	}
	return sessions, rows.Err()
}

func scanTasks(rows pgx.Rows) ([]*Task, error) {
	var tasks []*Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(
			&t.ID, &t.Title, &t.Body, &t.Status, &t.Priority,
			&t.ClaimedBy, &t.ClaimedAt, &t.DoneAt, &t.CreatedAt, &t.Attempt,
			&t.MaxAttempts, &t.ProjectID, &t.Metadata,
			&t.RequiresApproval, &t.ApprovedBy, &t.ApprovedAt, &t.RejectionReason,
		); err != nil {
			return nil, fmt.Errorf("scanning task: %w", err)
		}
		tasks = append(tasks, &t)
	}
	return tasks, rows.Err()
}

// BindTopicToAgent creates an observation binding between a Telegram topic and an agent.
func BindTopicToAgent(pool *pgxpool.Pool, topicID int64, agentID, bindingType string) error {
	if bindingType == "" {
		bindingType = "observe"
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (topic_id, agent_id) DO UPDATE SET binding_type = $3
	`, topicID, agentID, bindingType)
	if err != nil {
		return fmt.Errorf("binding topic %d to agent %s: %w", topicID, agentID, err)
	}
	return nil
}

// UnbindTopicFromAgent removes the observation binding.
func UnbindTopicFromAgent(pool *pgxpool.Pool, topicID int64, agentID string) error {
	_, err := pool.Exec(context.Background(), `
		DELETE FROM topic_agent_bindings WHERE topic_id = $1 AND agent_id = $2
	`, topicID, agentID)
	if err != nil {
		return fmt.Errorf("unbinding topic %d from agent %s: %w", topicID, agentID, err)
	}
	return nil
}

// TopicAgentBinding represents a binding between a topic and an agent.
type TopicAgentBinding struct {
	TopicID     int64     `json:"topic_id"`
	AgentID     string    `json:"agent_id"`
	BindingType string    `json:"binding_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// GetAgentsForTopic returns all agents observed by a topic.
func GetAgentsForTopic(pool *pgxpool.Pool, topicID int64) ([]TopicAgentBinding, error) {
	rows, err := pool.Query(context.Background(), `
		SELECT topic_id, agent_id, binding_type, created_at
		FROM topic_agent_bindings WHERE topic_id = $1
		ORDER BY created_at ASC
	`, topicID)
	if err != nil {
		return nil, fmt.Errorf("getting agents for topic %d: %w", topicID, err)
	}
	defer rows.Close()

	var bindings []TopicAgentBinding
	for rows.Next() {
		var b TopicAgentBinding
		if err := rows.Scan(&b.TopicID, &b.AgentID, &b.BindingType, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning binding: %w", err)
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// GetTopicsForAgent returns all topics observing an agent.
func GetTopicsForAgent(pool *pgxpool.Pool, agentID string) ([]TopicAgentBinding, error) {
	rows, err := pool.Query(context.Background(), `
		SELECT topic_id, agent_id, binding_type, created_at
		FROM topic_agent_bindings WHERE agent_id = $1
		ORDER BY created_at ASC
	`, agentID)
	if err != nil {
		return nil, fmt.Errorf("getting topics for agent %s: %w", agentID, err)
	}
	defer rows.Close()

	var bindings []TopicAgentBinding
	for rows.Next() {
		var b TopicAgentBinding
		if err := rows.Scan(&b.TopicID, &b.AgentID, &b.BindingType, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning binding: %w", err)
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}
