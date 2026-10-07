// MAQ-38 binding-sweep tests: the boot invariant — an agents row may only
// hold a tmux_window binding while a pane NAMED for that row's id exists.
// tmux window ids (@N) restart with the tmux server, so after a crash the
// pre-crash rows claim the same @N values fresh panes draw; the sweep
// clears stale/collided bindings before panes respawn so the monitor's
// attribution, transcript liveness and the freeze arms never hit a
// dead-row/live-row collision.
package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// noSuchSession is a session name that cannot exist on the test host — the
// sweep consults it (SessionExists) only when the injected lister errors.
const noSuchSession = "maquinista-sweep-test-no-such-session"

func setupBindings(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return context.Background(), pool
}

// win builds a Window list entry.
func win(id, name string) tmux.Window { return tmux.Window{ID: id, Name: name} }

func seedAgent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, status, window string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, status, started_at, last_seen)
		VALUES ($1, 'maquinista', $2, 'implementor', $3, NOW() - INTERVAL '24 hours', NOW() - INTERVAL '24 hours')
	`, id, window, status); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func bindingOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var w string
	if err := pool.QueryRow(ctx, `SELECT tmux_window FROM agents WHERE id = $1`, id).Scan(&w); err != nil {
		t.Fatalf("binding of %s: %v", id, err)
	}
	return w
}

func TestSweepStaleWindowBindings_StaleBindingCleared(t *testing.T) {
	ctx, pool := setupBindings(t)
	seedAgent(t, ctx, pool, "impl-crash-r1", "running", "@7") // pane died with the crash

	res, err := SweepStaleWindowBindings(ctx, pool, noSuchSession, func(string) ([]tmux.Window, error) {
		return nil, nil // post-restart: no windows at all yet
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared != 1 || res.Mismatched != 0 {
		t.Fatalf("res = %+v, want cleared=1 mismatched=0", res)
	}
	if got := bindingOf(t, ctx, pool, "impl-crash-r1"); got != "" {
		t.Fatalf("binding after sweep = %q, want empty (row must not collide with a future @7)", got)
	}
}

// TestSweepStaleWindowBindings_CollisionLiveRowReported is the AC 3
// diagnosis seam: a LIVE row whose @N now belongs to a pane named for
// another agent — the stale-id starvation signature. The binding is
// cleared AND the row is reported so the boot notify can name it; before
// this sweep the collision silently starved the watchdog until every
// round false-froze.
func TestSweepStaleWindowBindings_CollisionLiveRowReported(t *testing.T) {
	ctx, pool := setupBindings(t)
	// The fresh pane took @7; the pre-crash LIVE row still claims @7 —
	// but @7's pane is named for a different agent.
	seedAgent(t, ctx, pool, "impl-old-r1", "running", "@7")
	seedAgent(t, ctx, pool, "impl-other-r2", "running", "@9")

	res, err := SweepStaleWindowBindings(ctx, pool, noSuchSession, func(string) ([]tmux.Window, error) {
		return []tmux.Window{win("@7", "impl-other-r2"), win("@9", "impl-other-r2")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared != 1 || res.Mismatched != 1 || len(res.MismatchRows) != 1 {
		t.Fatalf("res = %+v, want cleared=1 mismatched=1 one row named", res)
	}
	if got := bindingOf(t, ctx, pool, "impl-old-r1"); got != "" {
		t.Fatalf("collided live binding = %q, want cleared", got)
	}
	// The healthy row is untouched.
	if got := bindingOf(t, ctx, pool, "impl-other-r2"); got != "@9" {
		t.Fatalf("healthy binding = %q, want @9 untouched", got)
	}
}

// TestSweepStaleWindowBindings_CollisionDeadRowSilent: a corpse holding a
// reused @N is cleared unconditionally (it must never shadow a live pane)
// but is not a live-row mismatch — no loud diagnosis, just the cleanup.
func TestSweepStaleWindowBindings_CollisionDeadRowSilent(t *testing.T) {
	ctx, pool := setupBindings(t)
	seedAgent(t, ctx, pool, "impl-corpse-r1", "dead", "@3")

	res, err := SweepStaleWindowBindings(ctx, pool, noSuchSession, func(string) ([]tmux.Window, error) {
		return []tmux.Window{win("@3", "impl-young-r2")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared != 1 || res.Mismatched != 0 || len(res.MismatchRows) != 0 {
		t.Fatalf("res = %+v, want cleared=1 mismatched=0", res)
	}
	if got := bindingOf(t, ctx, pool, "impl-corpse-r1"); got != "" {
		t.Fatalf("corpse binding = %q, want cleared", got)
	}
}

// TestSweepStaleWindowBindings_HealthyUntouched: a row whose pane exists
// and is named for the row's own id keeps its binding — this is the
// graceful-restart case for user agents whose panes survived.
func TestSweepStaleWindowBindings_HealthyUntouched(t *testing.T) {
	ctx, pool := setupBindings(t)
	seedAgent(t, ctx, pool, "reviewer-alive-r2", "running", "@12")

	res, err := SweepStaleWindowBindings(ctx, pool, noSuchSession, func(string) ([]tmux.Window, error) {
		return []tmux.Window{win("@12", "reviewer-alive-r2"), win("@13", "someone-else")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared != 0 || res.Mismatched != 0 {
		t.Fatalf("res = %+v, want nothing touched", res)
	}
	if got := bindingOf(t, ctx, pool, "reviewer-alive-r2"); got != "@12" {
		t.Fatalf("healthy binding = %q, want @12 kept", got)
	}
}

// TestSweepStaleWindowBindings_NoSessionClearsAll: post-restart there is
// no tmux session at all — every binding is stale and the sweep must still
// run (an error/abort here would leave every collision in place).
func TestSweepStaleWindowBindings_NoSessionClearsAll(t *testing.T) {
	ctx, pool := setupBindings(t)
	seedAgent(t, ctx, pool, "impl-r1", "running", "@1")
	seedAgent(t, ctx, pool, "impl-r2", "idle", "@2")

	res, err := SweepStaleWindowBindings(ctx, pool, noSuchSession, func(string) ([]tmux.Window, error) {
		return nil, errors.New("no such session")
	})
	if err != nil {
		t.Fatalf("sweep must tolerate a missing session: %v", err)
	}
	if res.Cleared != 2 {
		t.Fatalf("cleared = %d, want 2 (all bindings stale when the session is gone)", res.Cleared)
	}
	for _, id := range []string{"impl-r1", "impl-r2"} {
		if got := bindingOf(t, ctx, pool, id); got != "" {
			t.Fatalf("%s binding = %q, want empty", id, got)
		}
	}
}
