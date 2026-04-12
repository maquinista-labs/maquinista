package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// BackfillThreadBindings migrates state.ThreadBindings + state.GroupChatIDs
// into the topic_agent_bindings table produced by migration 009.
//
// Each (user_id, thread_id, window_id) triple becomes an owner binding for
// the agent whose id matches the window_id. chat_id is looked up from
// state.GroupChatIDs when available. The partial unique index
// uq_topic_binding_owner_thread guarantees idempotency across reruns —
// subsequent calls insert no new rows as long as the same state is replayed.
//
// Rows referencing an agent_id that is not present in the agents table are
// skipped (FK would reject them) and counted in the returned `skipped`.
func BackfillThreadBindings(ctx context.Context, pool *pgxpool.Pool, st *state.State) (inserted int, skipped int, err error) {
	if st == nil {
		return 0, 0, fmt.Errorf("state is nil")
	}

	known := make(map[string]bool)
	rows, err := pool.Query(ctx, `SELECT id FROM agents`)
	if err != nil {
		return 0, 0, fmt.Errorf("listing agents: %w", err)
	}
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scanning agent id: %w", scanErr)
		}
		known[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterating agents: %w", err)
	}

	type entry struct{ userID, threadID, agentID string }
	var entries []entry
	for uid, threads := range st.ThreadBindings {
		for tid, wid := range threads {
			entries = append(entries, entry{uid, tid, wid})
		}
	}

	for _, e := range entries {
		if !known[e.agentID] {
			skipped++
			continue
		}
		chatID, hasChat := st.GetGroupChatID(e.userID, e.threadID)
		var chatIDArg any
		if hasChat {
			chatIDArg = chatID
		}

		// topic_id is BIGINT in migration 007. Accept thread_id as numeric when
		// possible; otherwise fall back to 0 so we still capture the binding.
		// The real routing key is (user_id, thread_id).
		var topicID int64
		fmt.Sscanf(e.threadID, "%d", &topicID)

		tag, execErr := pool.Exec(ctx, `
			INSERT INTO topic_agent_bindings
				(topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
			VALUES ($1, $2, 'owner', $3, $4, $5)
			ON CONFLICT (user_id, thread_id) WHERE binding_type = 'owner' DO NOTHING
		`, topicID, e.agentID, e.userID, e.threadID, chatIDArg)
		if execErr != nil {
			return inserted, skipped, fmt.Errorf("inserting binding (%s,%s,%s): %w", e.userID, e.threadID, e.agentID, execErr)
		}
		if tag.RowsAffected() == 1 {
			inserted++
		}
	}

	return inserted, skipped, nil
}
