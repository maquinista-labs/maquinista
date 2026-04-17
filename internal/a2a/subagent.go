package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SpawnFunc hands the actual tmux + runner machinery back out so this
// package doesn't depend on cmd/maquinista. Wire it in from the bot /
// cmd_start layer where the per-topic spawner already lives.
// Returns the new agent's canonical id.
type SpawnFunc func(ctx context.Context, parentID, childID, goal string) error

// ErrDelegationDenied is returned when the parent agent's soul has
// allow_delegation=false. Callers (the tool-layer exposing
// `spawn_subagent` to agents) should surface this verbatim.
var ErrDelegationDenied = errors.New("a2a: parent agent has allow_delegation=false")

// ErrDepthExceeded is returned when the delegation chain would go past
// the configured max depth (default 2 — mirrors hermes / openclaw).
var ErrDepthExceeded = errors.New("a2a: sub-agent depth exceeded")

// MaxSpawnDepth caps nested delegation. Fixed constant to keep callers
// honest; raise via config if a specific workflow needs deeper chains.
const MaxSpawnDepth = 2

// SubagentHandle is what SpawnSubagent returns: the child agent id, the
// conversation that tracks the parent↔child exchange, and a bound
// AskAgent call that waits for the child's result.
type SubagentHandle struct {
	ChildID        string
	ConversationID uuid.UUID
}

// SpawnSubagent creates a task-scoped child agent to pursue `goal` on
// behalf of `parentID`. The child id is derived as
// sub-<parent>-<short-uuid> so logs trace back to the parent easily.
//
// Requires the parent's soul to have allow_delegation=true (checked via
// agent_souls). Delegation depth is capped at MaxSpawnDepth — the
// check walks parent chains using conversations.parent_conversation_id
// to count hops.
//
// The runner-side tmux spawn is delegated to SpawnFunc. If SpawnFunc is
// nil, SpawnSubagent inserts the agents + conversations + inbox rows
// but does not spin a pane up — useful for tests and for callers that
// want to defer runner spawn to the reconcile loop.
func SpawnSubagent(ctx context.Context, pool *pgxpool.Pool, parentID, goal string, spawn SpawnFunc) (*SubagentHandle, error) {
	if pool == nil {
		return nil, errors.New("a2a: nil pool")
	}
	allowed, depth, err := delegationGate(ctx, pool, parentID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrDelegationDenied
	}
	if depth >= MaxSpawnDepth {
		return nil, fmt.Errorf("%w (parent chain depth=%d, max=%d)", ErrDepthExceeded, depth, MaxSpawnDepth)
	}

	childID := "sub-" + parentID + "-" + uuid.New().String()[:8]

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// agents row — task-scoped, minimal metadata. tmux_window left empty
	// until SpawnFunc (or the reconcile loop) creates the pane.
	var parentRunner string
	_ = tx.QueryRow(ctx,
		`SELECT COALESCE(runner_type,'claude') FROM agents WHERE id=$1`,
		parentID,
	).Scan(&parentRunner)

	if _, err := tx.Exec(ctx, `
		INSERT INTO agents
			(id, tmux_session, tmux_window, role, status, runner_type,
			 cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ($1, 'maquinista', '', 'executor', 'stopped', $2,
		        (SELECT cwd FROM agents WHERE id=$3),
		        $1, NOW(), NOW(), FALSE)
	`, childID, parentRunner, parentID); err != nil {
		return nil, fmt.Errorf("insert child agent: %w", err)
	}

	// Conversation — kind='a2a', parent pointer set so the depth walk
	// finds this row next time.
	parentConvoID, err := currentA2AConvo(ctx, tx, parentID)
	if err != nil {
		return nil, err
	}

	var convoID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO conversations
			(kind, participants, topic, parent_conversation_id, pending_count)
		VALUES ('a2a', ARRAY[$1, $2]::text[], $3, $4, 0)
		RETURNING id
	`, parentID, childID, trimToLen(goal, 120), parentConvoID).Scan(&convoID)
	if err != nil {
		return nil, fmt.Errorf("insert child conversation: %w", err)
	}

	// Inbox row — the spawn goal itself, addressed to the child.
	content, _ := json.Marshal(map[string]string{"type": "text", "text": goal})
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_inbox
			(agent_id, conversation_id, from_kind, from_id,
			 origin_channel, external_msg_id, content)
		VALUES ($1, $2, 'agent', $3, 'a2a:spawn', $4, $5::jsonb)
	`, childID, convoID, parentID, "spawn:"+childID, content); err != nil {
		return nil, fmt.Errorf("insert goal inbox row: %w", err)
	}

	// Clone the parent's soul into the child so it inherits the
	// identity. Operators who want a different persona can follow up
	// with `maquinista soul import`.
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_souls
			(agent_id, template_id, name, tagline, role, goal,
			 core_truths, boundaries, vibe, continuity, extras,
			 allow_delegation, max_iter, respect_context, version)
		SELECT $1, template_id, name, tagline, role, $2,
		       core_truths, boundaries, vibe, continuity, extras,
		       FALSE, max_iter, respect_context, 1
		FROM agent_souls WHERE agent_id=$3
		ON CONFLICT (agent_id) DO NOTHING
	`, childID, goal, parentID); err != nil {
		// Soul inheritance is nice-to-have — don't fail the spawn.
		_ = err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// External side effect: ask the caller to bring the pane up.
	// Done outside the TX so a slow tmux spawn doesn't block the DB
	// transaction.
	if spawn != nil {
		if err := spawn(ctx, parentID, childID, goal); err != nil {
			return nil, fmt.Errorf("runner spawn: %w", err)
		}
	}

	return &SubagentHandle{ChildID: childID, ConversationID: convoID}, nil
}

// WaitForResult polls for a final reply from the child agent on the
// given conversation. Returns the reply text or ErrTimeout. Intended to
// be called by tools that spawn-and-wait; fire-and-forget callers skip
// this.
func WaitForResult(ctx context.Context, pool *pgxpool.Pool, convoID uuid.UUID, childID string, timeout time.Duration) (*Reply, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w (after %s)", ErrTimeout, timeout)
		}
		rep, err := pollAnyReply(ctx, pool, childID, convoID)
		if err != nil {
			return nil, err
		}
		if rep != nil {
			return rep, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrTimeout, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func pollAnyReply(ctx context.Context, pool *pgxpool.Pool, fromAgent string, convoID uuid.UUID) (*Reply, error) {
	var outID uuid.UUID
	var rawContent []byte
	err := pool.QueryRow(ctx, `
		SELECT id, content FROM agent_outbox
		WHERE agent_id = $1 AND conversation_id = $2
		ORDER BY created_at ASC
		LIMIT 1
	`, fromAgent, convoID).Scan(&outID, &rawContent)
	if errors.Is(err, pgxerrNoRows()) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var body struct {
		Text  string `json:"text"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	_ = json.Unmarshal(rawContent, &body)
	text := body.Text
	if text == "" {
		for _, p := range body.Parts {
			if p.Type == "text" && p.Text != "" {
				text += p.Text + "\n"
			}
		}
	}
	return &Reply{
		Text:           text,
		ConversationID: convoID,
		OutboxID:       outID,
	}, nil
}

func pgxerrNoRows() error { return pgx.ErrNoRows }

// delegationGate returns (allowed, currentDepth, error).
//
// - allowed is the parent's agent_souls.allow_delegation.
// - currentDepth counts how many kind='a2a' ancestor conversations
//   the parent already participates in (via parent_conversation_id
//   chains), capped when it sees itself as participant of a root convo
//   (depth 1 = parent is already a child of someone).
func delegationGate(ctx context.Context, pool *pgxpool.Pool, parentID string) (bool, int, error) {
	var allow bool
	err := pool.QueryRow(ctx,
		`SELECT allow_delegation FROM agent_souls WHERE agent_id=$1`,
		parentID,
	).Scan(&allow)
	if err != nil {
		// Missing soul row → treat as not allowed; operator must
		// explicitly set allow_delegation.
		return false, 0, nil
	}

	// Depth = longest chain of conversations.parent_conversation_id
	// where parent participates. Bounded walk at MaxSpawnDepth+1 so a
	// malformed chain can't loop.
	depth := 0
	var convoID any
	err = pool.QueryRow(ctx, `
		SELECT id FROM conversations
		WHERE kind='a2a' AND participants @> ARRAY[$1]::text[]
		ORDER BY created_at DESC
		LIMIT 1
	`, parentID).Scan(&convoID)
	if errors.Is(err, pgxerrNoRows()) {
		return allow, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	for i := 0; i < MaxSpawnDepth+1; i++ {
		var parentConvo any
		err := pool.QueryRow(ctx,
			`SELECT parent_conversation_id FROM conversations WHERE id=$1`,
			convoID,
		).Scan(&parentConvo)
		if err != nil {
			break
		}
		if parentConvo == nil {
			break
		}
		depth++
		convoID = parentConvo
	}
	return allow, depth, nil
}

func currentA2AConvo(ctx context.Context, tx pgx.Tx, agentID string) (any, error) {
	// Return the most-recent open a2a conversation this agent is on, as
	// an `any` (usable directly in INSERT). nil = no parent context.
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM conversations
		WHERE kind='a2a' AND closed_at IS NULL
		  AND participants @> ARRAY[$1]::text[]
		ORDER BY created_at DESC
		LIMIT 1
	`, agentID).Scan(&id)
	if errors.Is(err, pgxerrNoRows()) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}

func trimToLen(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
