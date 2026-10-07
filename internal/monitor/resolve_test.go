// MAQ-38 AC 1 regression pins: after a crash/restart, tmux window ids
// (@N) restart with the tmux server, so the pre-crash agents row (dead,
// older) and the post-crash round's row (live, newer) can claim the SAME
// window id. Every window→agent resolution — outbox attribution and
// transcript-liveness touches alike — must land on the LIVE row, never on
// the stale pre-restart id. Before the live-preferring ordering this was
// an arbitrary tie-break: the 07/10 incident streamed whole rounds into
// outbox rows under stale ids while the live rows starved both freshness
// channels and the watchdog false-froze them to the respawn cap.
package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// TestResolveAgentFromWindow_PostCrashCollisionPicksLive: window @5 is
// claimed by a dead pre-crash row and a live post-crash row — resolution
// must return the LIVE id (the row the watchdog watches and the pane is
// actually serving).
func TestResolveAgentFromWindow_PostCrashCollisionPicksLive(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	stale := "implementor-53c4761c-r1"
	live := "implementor-53c4761c-r2"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO agents (id, tmux_session, tmux_window, role, status, started_at, last_seen)
	      VALUES ($1, 'maquinista', '@5', 'implementor', 'dead',    NOW() - INTERVAL '24 hours', NOW() - INTERVAL '24 hours')`, stale)
	exec(`INSERT INTO agents (id, tmux_session, tmux_window, role, status, started_at, last_seen)
	      VALUES ($1, 'maquinista', '@5', 'implementor', 'running', NOW(), NOW())`, live)

	got, err := resolveAgentFromWindow(ctx, pool, "@5")
	if err != nil {
		t.Fatal(err)
	}
	if got != live {
		t.Fatalf("resolve(@5) = %q, want live row %q (never the stale pre-restart id)", got, live)
	}

	// Exact-id lookup still wins outright — legacy callers and tests that
	// pass a real agent id are untouched by the ordering.
	got, err = resolveAgentFromWindow(ctx, pool, stale)
	if err != nil {
		t.Fatal(err)
	}
	if got != stale {
		t.Fatalf("resolve(%q) = %q, want exact-id passthrough", stale, got)
	}
}

// TestOutboxFlush_PostCrashCollisionAttribution is the AC 1 end-to-end
// seam: an outbox flush for a collided window id must write the row under
// the LIVE round's id — the new —rN — and under NO pre-restart id.
func TestOutboxFlush_PostCrashCollisionAttribution(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	stale := "implementor-6b44dc7e-r3"
	live := "implementor-6b44dc7e-r4"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	// The pre-crash corpse holds @5; the restarted round's row holds the
	// SAME @5 (the fresh pane drew the reused id).
	exec(`INSERT INTO agents (id, tmux_session, tmux_window, role, status, started_at, last_seen)
	      VALUES ($1, 'maquinista', '@5', 'implementor', 'dead',    NOW() - INTERVAL '24 hours', NOW() - INTERVAL '24 hours')`, stale)
	exec(`INSERT INTO agents (id, tmux_session, tmux_window, role, status, started_at, last_seen)
	      VALUES ($1, 'maquinista', '@5', 'implementor', 'running', NOW(), NOW())`, live)

	s := NewOutboxSink(pool, nil)
	s.Handle(AgentEvent{
		Kind:     AgentEventText,
		AgentID:  "@5", // what the transcript source knows: the window
		WindowID: "@5",
		Role:     "assistant",
		Text:     "round 4 work product — must never land under r3",
	})
	s.FlushSession("@5")

	var staleRows, liveRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_outbox WHERE agent_id = $1`, stale).Scan(&staleRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_outbox WHERE agent_id = $1`, live).Scan(&liveRows); err != nil {
		t.Fatal(err)
	}
	if staleRows != 0 {
		t.Fatalf("outbox rows under stale pre-restart id %s = %d, want 0", stale, staleRows)
	}
	if liveRows != 1 {
		t.Fatalf("outbox rows under live round id %s = %d, want 1", live, liveRows)
	}

	// And the transcript-liveness channel resolves through the same seam:
	// the freshness touch must feed the LIVE row too (before the fix, the
	// dead row from the previous day accumulated last_transcript_at while
	// the live round starved — the false-freeze premise).
	agentID, err := resolveAgentFromWindow(ctx, pool, "@5")
	if err != nil {
		t.Fatal(err)
	}
	if agentID != live {
		t.Fatalf("liveness touch would feed %q, want %q", agentID, live)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET last_transcript_at = NOW() WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	var staleTouch, liveTouch int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE id = $1 AND last_transcript_at > $2`, stale, time.Now().Add(-time.Minute)).Scan(&staleTouch); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE id = $1 AND last_transcript_at > $2`, live, time.Now().Add(-time.Minute)).Scan(&liveTouch); err != nil {
		t.Fatal(err)
	}
	if staleTouch != 0 || liveTouch != 1 {
		t.Fatalf("liveness touch landed wrong: stale=%d live=%d, want stale=0 live=1", staleTouch, liveTouch)
	}
}
