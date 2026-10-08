// ADR-0008 turn-end producer tests: the monitor records the sticky
// agents.last_turn_end_at signal, and the batch-shape predicate only closes
// a turn on a trailing assistant text entry.
package monitor

import (
	"context"
	"testing"
)

// TestRecordTurnEnd_WritesAgentColumn pins the ADR-0008 producer: a turn
// end observed for a window owned by an agent lands in
// agents.last_turn_end_at — the sticky evidence the pipeline's nudge legs
// and freeze-cause classifier consume.
func TestRecordTurnEnd_WritesAgentColumn(t *testing.T) {
	pool := migratedPool(t)
	seedLivenessAgent(t, pool, "turn-end-agent", "w-turnend")
	m := &Monitor{pool: pool}

	m.recordTurnEnd("turn-end-agent")

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents WHERE id='turn-end-agent' AND last_turn_end_at IS NOT NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("last_turn_end_at rows = %d, want 1 (PASS: turn end recorded on agent)", n)
	}
}

// TestRecordTurnEnd_UnownedWindowIsSilent: windows that resolve to no
// agent row (user sessions, dead rows) are skipped without error.
func TestRecordTurnEnd_UnownedWindowIsSilent(t *testing.T) {
	pool := migratedPool(t)
	m := &Monitor{pool: pool}
	m.recordTurnEnd("") // empty agent id — must not touch the DB
	m.recordTurnEnd("no-such-agent")
}

// TestBatchEndsWithTurnEnd pins the batch-shape predicate (ADR-0008):
// only a trailing assistant text entry closes a turn; mid-turn batches end
// on tool calls, tool results, or thinking.
func TestBatchEndsWithTurnEnd(t *testing.T) {
	cases := []struct {
		name   string
		batch  []ParsedEntry
		wantOK bool
	}{
		{"empty batch", nil, false},
		{"assistant text closes the turn", []ParsedEntry{
			{Role: "user", ContentType: "text"},
			{Role: "assistant", ContentType: "text"},
		}, true},
		{"mid-turn: tool call last", []ParsedEntry{
			{Role: "assistant", ContentType: "text"},
			{Role: "assistant", ContentType: "tool_use"},
		}, false},
		{"mid-turn: tool result last", []ParsedEntry{
			{Role: "assistant", ContentType: "tool_use"},
			{Role: "user", ContentType: "tool_result"},
		}, false},
		{"mid-turn: thinking last", []ParsedEntry{
			{Role: "assistant", ContentType: "thinking"},
		}, false},
	}
	for _, tc := range cases {
		if got := batchEndsWithTurnEnd(tc.batch); got != tc.wantOK {
			t.Errorf("%s: batchEndsWithTurnEnd = %v, want %v", tc.name, got, tc.wantOK)
		}
	}
}
