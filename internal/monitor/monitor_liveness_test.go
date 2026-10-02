package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedLivenessAgent inserts an agents row with a distinct window so
// resolveAgentFromWindow has an unambiguous match.
func seedLivenessAgent(t *testing.T, pool *pgxpool.Pool, id, window string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, status, runner_type, role)
		VALUES ($1, 'sess', $2, 'running', 'pi', 'reviewer')
	`, id, window); err != nil {
		t.Fatalf("seedLivenessAgent %q: %v", id, err)
	}
}

// TestTouchTranscriptLiveness_WritesAgentColumn pins the MAQ-9 liveness
// write: transcript growth for a window owned by an agent lands in
// agents.last_transcript_at — the column the pipeline watchdog reads as
// activity (silence there no longer means stalled).
func TestTouchTranscriptLiveness_WritesAgentColumn(t *testing.T) {
	pool := migratedPool(t)
	seedLivenessAgent(t, pool, "live-agent", "w-live")
	// freq 0 = no throttle: every call writes (test seam).
	m := &Monitor{pool: pool, transcriptTouchFreq: 0}

	m.touchTranscriptLiveness("w-live")

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents WHERE id='live-agent' AND last_transcript_at IS NOT NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("last_transcript_at rows = %d, want 1 (PASS: growth recorded on agent)", n)
	}
	t.Log("PASS TestTouchTranscriptLiveness_WritesAgentColumn")
}

// TestTouchTranscriptLiveness_ThrottlesPerWindow pins the rate bound: a
// streaming agent advances its transcript every poll; the DB write happens
// at most once per transcriptTouchFreq per window.
func TestTouchTranscriptLiveness_ThrottlesPerWindow(t *testing.T) {
	pool := migratedPool(t)
	seedLivenessAgent(t, pool, "throttle-agent", "w-throttle")
	m := &Monitor{pool: pool, transcriptTouchFreq: time.Hour}

	m.touchTranscriptLiveness("w-throttle") // first growth: writes
	// Wipe the signal, then simulate a second growth tick inside the
	// throttle window: the write must be suppressed.
	if _, err := pool.Exec(context.Background(),
		`UPDATE agents SET last_transcript_at = NULL WHERE id='throttle-agent'`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	m.touchTranscriptLiveness("w-throttle")

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents WHERE id='throttle-agent' AND last_transcript_at IS NOT NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatalf("last_transcript_at rows = %d, want 0 (PASS: throttled inside window)", n)
	}
	t.Log("PASS TestTouchTranscriptLiveness_ThrottlesPerWindow")
}

// TestTouchTranscriptLiveness_UnknownWindowNoop: windows without an agent
// row (user sessions) are skipped silently — no error, no crash.
func TestTouchTranscriptLiveness_UnknownWindowNoop(t *testing.T) {
	pool := migratedPool(t)
	m := &Monitor{pool: pool, transcriptTouchFreq: 0}

	m.touchTranscriptLiveness("w-nobody") // must not panic

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents WHERE last_transcript_at IS NOT NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows = %d, want 0 (PASS: unknown window skipped)", n)
	}
	t.Log("PASS TestTouchTranscriptLiveness_UnknownWindowNoop")
}
