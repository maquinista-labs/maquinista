package monitor

import (
	"context"
	"encoding/json"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewDBToolEventNotifier returns a ToolEventWriter that fires a
// pg_notify("tool_event", <json>) for every tool_use / tool_result
// observation. The notification payload is a JSON object:
//
//	{"agent_id":"…","type":"tool_use|tool_result","tool_name":"…","tool_use_id":"…","is_error":false}
//
// The AgentID field arriving from the monitor is the tmux window id;
// this function resolves it to the logical agents.id before notifying
// so dashboard subscribers can match by agent id directly.
func NewDBToolEventNotifier(pool *pgxpool.Pool) ToolEventWriter {
	return func(e ToolEvent) {
		if pool == nil || e.ToolUseID == "" {
			return
		}
		ctx := context.Background()

		agentID, err := resolveAgentFromWindow(ctx, pool, e.AgentID)
		if err != nil {
			log.Printf("tool_event notifier: resolve agent for window %s: %v", e.AgentID, err)
			return
		}
		if agentID == "" {
			// Unbound window — nothing to push.
			return
		}

		payload, err := json.Marshal(map[string]any{
			"agent_id":    agentID,
			"type":        e.Type,
			"tool_name":   e.ToolName,
			"tool_use_id": e.ToolUseID,
			"is_error":    e.IsError,
		})
		if err != nil {
			log.Printf("tool_event notifier: marshal: %v", err)
			return
		}

		if _, err := pool.Exec(ctx, "SELECT pg_notify($1, $2)", "tool_event", string(payload)); err != nil {
			log.Printf("tool_event notifier: pg_notify: %v", err)
		}
	}
}
