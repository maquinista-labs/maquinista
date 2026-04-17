package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setupAgent(t *testing.T, id string) context.Context {
	t.Helper()
	p, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := p.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ($1,'s','w')`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {})
	// Stash pool in context via a helper keyed string — but simpler:
	// just return ctx and let caller use shared pool below.
	_ = id
	return ctx
}

func TestAutoFlush_RememberPattern(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('af','s','w')`); err != nil {
		t.Fatal(err)
	}

	id, fact, ok := AutoFlush(ctx, pool, "af", "please remember that I prefer pt-BR for replies")
	if !ok || id == 0 {
		t.Fatalf("expected match; got ok=%v id=%d", ok, id)
	}
	if !strings.Contains(fact, "pt-BR") {
		t.Errorf("fact=%q", fact)
	}

	// The row exists and has the expected shape.
	got, err := Get(ctx, pool, "af", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dimension != "user" || got.Tier != "long_term" || got.Category != "preference" || got.Source != "auto_flush" {
		t.Errorf("unexpected shape: %+v", got)
	}
}

func TestAutoFlush_PreferencePattern(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('af2','s','w')`); err != nil {
		t.Fatal(err)
	}

	_, fact, ok := AutoFlush(ctx, pool, "af2", "I prefer terse responses without emoji")
	if !ok {
		t.Fatal("expected match on 'I prefer …'")
	}
	if !strings.Contains(fact, "terse") {
		t.Errorf("fact=%q", fact)
	}
}

func TestAutoFlush_NoMatch(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('af3','s','w')`); err != nil {
		t.Fatal(err)
	}

	if id, _, ok := AutoFlush(ctx, pool, "af3", "just a normal message about the build status"); ok || id != 0 {
		t.Errorf("unexpected match on non-memory text")
	}
}

func TestAutoFlush_DontForgetPattern(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('af4','s','w')`); err != nil {
		t.Fatal(err)
	}
	_, fact, ok := AutoFlush(ctx, pool, "af4", "don't forget that production migrations need --no-verify")
	if !ok {
		t.Fatal("expected match on 'don't forget …'")
	}
	if !strings.Contains(fact, "production migrations") {
		t.Errorf("fact=%q", fact)
	}
}
