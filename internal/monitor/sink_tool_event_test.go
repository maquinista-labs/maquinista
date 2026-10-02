package monitor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setupToolEventSink(t *testing.T) *ToolEventSink {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	// Insert a real agent so resolveAgentFromWindow can find it.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('agent-1','s','win-1')`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return NewToolEventSink(pool)
}

// collectNotifications listens on the tool_event channel and collects up to n
// notifications, timing out after timeout.
func collectNotifications(t *testing.T, s *ToolEventSink, n int, timeout time.Duration) []map[string]any {
	t.Helper()
	conn, err := s.pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(context.Background(), "LISTEN tool_event"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	var results []map[string]any
	deadline := time.Now().Add(timeout)
	for len(results) < n && time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		notif, err := conn.Conn().WaitForNotification(ctx)
		cancel()
		if err != nil {
			break
		}
		var payload map[string]any
		if jsonErr := json.Unmarshal([]byte(notif.Payload), &payload); jsonErr != nil {
			t.Logf("unmarshal notification: %v", jsonErr)
			continue
		}
		results = append(results, payload)
	}
	return results
}

func TestToolEventSink_NotifiesToolUse(t *testing.T) {
	s := setupToolEventSink(t)

	done := make(chan []map[string]any, 1)
	go func() {
		done <- collectNotifications(t, s, 1, 5*time.Second)
	}()
	// Give the listener a moment to start.
	time.Sleep(50 * time.Millisecond)

	s.Handle(AgentEvent{
		Kind:      AgentEventToolUse,
		AgentID:   "win-1",
		ToolName:  "bash",
		ToolUseID: "toolu_01",
		ChatID:    0,
	})

	notifs := <-done
	if len(notifs) != 1 {
		t.Fatalf("got %d notifications, want 1", len(notifs))
	}
	if notifs[0]["type"] != "tool_use" {
		t.Errorf("type = %v, want tool_use", notifs[0]["type"])
	}
	if notifs[0]["tool_name"] != "bash" {
		t.Errorf("tool_name = %v, want bash", notifs[0]["tool_name"])
	}
}

func TestToolEventSink_NotifiesToolResult(t *testing.T) {
	s := setupToolEventSink(t)

	done := make(chan []map[string]any, 1)
	go func() {
		done <- collectNotifications(t, s, 1, 5*time.Second)
	}()
	time.Sleep(50 * time.Millisecond)

	s.Handle(AgentEvent{
		Kind:      AgentEventToolResult,
		AgentID:   "win-1",
		ToolName:  "bash",
		ToolUseID: "toolu_02",
		ChatID:    0,
	})

	notifs := <-done
	if len(notifs) != 1 {
		t.Fatalf("got %d notifications, want 1", len(notifs))
	}
	if notifs[0]["type"] != "tool_result" {
		t.Errorf("type = %v, want tool_result", notifs[0]["type"])
	}
}

func TestToolEventSink_PairedEmitsResultOnly(t *testing.T) {
	s := setupToolEventSink(t)

	done := make(chan []map[string]any, 1)
	go func() {
		done <- collectNotifications(t, s, 1, 5*time.Second)
	}()
	time.Sleep(50 * time.Millisecond)

	s.Handle(AgentEvent{
		Kind:      AgentEventToolPaired,
		AgentID:   "win-1",
		ToolName:  "bash",
		ToolUseID: "toolu_03",
		ToolInput: "ls -la",
		Text:      "total 0",
		ChatID:    0,
	})

	notifs := <-done
	// Paired events emit ONLY tool_result (tool_input included): a separate
	// tool_use would duplicate the live entry emitted in the prior cycle.
	if len(notifs) != 1 {
		t.Fatalf("got %d notifications, want 1 (tool_result only)", len(notifs))
	}
	if notifs[0]["type"] != "tool_result" {
		t.Errorf("notification type = %v, want tool_result", notifs[0]["type"])
	}
	if notifs[0]["tool_input"] != "ls -la" {
		t.Errorf("tool_input = %v, want ls -la", notifs[0]["tool_input"])
	}
	if notifs[0]["text"] != "total 0" {
		t.Errorf("text = %v, want total 0", notifs[0]["text"])
	}
}

func TestToolEventSink_SkipsWhenChatIDNonZero(t *testing.T) {
	s := setupToolEventSink(t)

	done := make(chan []map[string]any, 1)
	go func() {
		done <- collectNotifications(t, s, 1, 500*time.Millisecond)
	}()
	time.Sleep(50 * time.Millisecond)

	s.Handle(AgentEvent{
		Kind:      AgentEventToolUse,
		AgentID:   "win-1",
		ToolName:  "bash",
		ToolUseID: "toolu_04",
		ChatID:    5, // non-zero → skip
	})

	notifs := <-done
	if len(notifs) != 0 {
		t.Errorf("got %d notifications, want 0 for chatID!=0", len(notifs))
	}
}

func TestToolEventSink_SkipsEmptyToolUseID(t *testing.T) {
	s := setupToolEventSink(t)

	done := make(chan []map[string]any, 1)
	go func() {
		done <- collectNotifications(t, s, 1, 500*time.Millisecond)
	}()
	time.Sleep(50 * time.Millisecond)

	s.Handle(AgentEvent{
		Kind:      AgentEventToolUse,
		AgentID:   "win-1",
		ToolName:  "bash",
		ToolUseID: "", // empty → skip
		ChatID:    0,
	})

	notifs := <-done
	if len(notifs) != 0 {
		t.Errorf("got %d notifications, want 0 for empty ToolUseID", len(notifs))
	}
}

func TestToolEventSink_SkipsEmptyAgentID(t *testing.T) {
	s := setupToolEventSink(t)

	done := make(chan []map[string]any, 1)
	go func() {
		done <- collectNotifications(t, s, 1, 500*time.Millisecond)
	}()
	time.Sleep(50 * time.Millisecond)

	s.Handle(AgentEvent{
		Kind:      AgentEventToolUse,
		AgentID:   "", // empty → skip
		ToolName:  "bash",
		ToolUseID: "toolu_05",
		ChatID:    0,
	})

	notifs := <-done
	if len(notifs) != 0 {
		t.Errorf("got %d notifications, want 0 for empty AgentID", len(notifs))
	}
}
