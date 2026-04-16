package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setupPool(t *testing.T) (ctx context.Context, pool interface {
	Close()
}, agentID string, tearDown func()) {
	t.Helper()
	p, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(p); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx = context.Background()
	agentID = "mem-test-" + t.Name()
	if _, err := p.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ($1, 's', 'w')
	`, agentID); err != nil {
		t.Fatal(err)
	}
	return ctx, p, agentID, func() {}
}

func TestSeedDefaultBlocks_CreatesAll(t *testing.T) {
	ctx, pool, agentID, tearDown := setupPool(t)
	defer tearDown()
	p := pool.(interface {
		Close()
	})
	_ = p

	pp, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pp); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pp.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaultBlocks(ctx, pp, "a", "I am a helpful engineer."); err != nil {
		t.Fatalf("seed: %v", err)
	}
	blocks, err := LoadAllBlocks(ctx, pp, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != len(DefaultBlocks) {
		t.Errorf("got %d blocks, want %d", len(blocks), len(DefaultBlocks))
	}

	// Persona should be seeded; human and task-note empty.
	var personaSeen bool
	for _, b := range blocks {
		if b.Label == "persona" {
			personaSeen = true
			if b.Value != "I am a helpful engineer." {
				t.Errorf("persona value = %q", b.Value)
			}
		}
		if b.Label == "human" && b.Value != "" {
			t.Errorf("human block should seed empty, got %q", b.Value)
		}
	}
	if !personaSeen {
		t.Error("persona block missing")
	}

	// Idempotent — second call is a no-op.
	if err := SeedDefaultBlocks(ctx, pp, "a", "different seed"); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	after, _ := LoadAllBlocks(ctx, pp, "a")
	if len(after) != len(DefaultBlocks) {
		t.Errorf("idempotent seed created extras: %d", len(after))
	}
	_ = agentID
}

func TestAppendBlock_EnforcesCharLimit(t *testing.T) {
	ctx, _, _, tearDown := setupPool(t)
	defer tearDown()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`); err != nil {
		t.Fatal(err)
	}
	// Tiny block so we can exceed it easily.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_blocks (agent_id, label, value, char_limit)
		VALUES ('a', 'small', '', 20)
	`); err != nil {
		t.Fatal(err)
	}

	if _, err := AppendBlock(ctx, pool, "a", "small", "hi"); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendBlock(ctx, pool, "a", "small", "there"); err != nil {
		t.Fatal(err)
	}
	// This append would push past 20 chars — should fail.
	_, err := AppendBlock(ctx, pool, "a", "small", strings.Repeat("x", 30))
	if !errors.Is(err, ErrBlockCharLimit) {
		t.Errorf("want ErrBlockCharLimit, got %v", err)
	}

	// Verify version bumped after successful appends.
	b, _ := LoadBlock(ctx, pool, "a", "small")
	if b.Version < 2 {
		t.Errorf("version = %d, expected at least 2 after two appends", b.Version)
	}
}

func TestReplaceBlock_ExactMatch(t *testing.T) {
	ctx, _, _, tearDown := setupPool(t)
	defer tearDown()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_blocks (agent_id, label, value, char_limit)
		VALUES ('a', 'persona', 'I like cats and dogs', 100)
	`); err != nil {
		t.Fatal(err)
	}

	got, err := ReplaceBlock(ctx, pool, "a", "persona", "cats", "birds")
	if err != nil {
		t.Fatal(err)
	}
	if got != "I like birds and dogs" {
		t.Errorf("got %q", got)
	}

	// Missing old content → error.
	_, err = ReplaceBlock(ctx, pool, "a", "persona", "zebras", "lions")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want 'not found' err, got %v", err)
	}
}

func TestRememberGetListForget(t *testing.T) {
	ctx, _, _, tearDown := setupPool(t)
	defer tearDown()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`); err != nil {
		t.Fatal(err)
	}

	id, err := Remember(ctx, pool, Memory{
		AgentID: "a", Dimension: "user", Tier: "long_term", Category: "preference",
		Title: "Prefers terse replies", Body: "Operator asks for short messages in pt-BR.",
		Source: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	got, err := Get(ctx, pool, "a", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Prefers terse replies" {
		t.Errorf("title mismatch: %q", got.Title)
	}

	list, err := List(ctx, pool, "a", ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("list len = %d, want 1", len(list))
	}

	if err := Forget(ctx, pool, "a", id); err != nil {
		t.Fatal(err)
	}
	_, err = Get(ctx, pool, "a", id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound after forget, got %v", err)
	}
}

func TestRemember_ValidatesFields(t *testing.T) {
	ctx, _, _, tearDown := setupPool(t)
	defer tearDown()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`); err != nil {
		t.Fatal(err)
	}

	// Bad tier
	_, err := Remember(ctx, pool, Memory{
		AgentID: "a", Dimension: "user", Tier: "weird", Category: "fact",
		Title: "x", Body: "y", Source: "test",
	})
	if err == nil {
		t.Error("expected tier validation error")
	}

	// Missing source
	_, err = Remember(ctx, pool, Memory{
		AgentID: "a", Dimension: "user", Tier: "long_term", Category: "fact",
		Title: "x", Body: "y", Source: "",
	})
	if err == nil {
		t.Error("expected missing source error")
	}
}

func TestSearch_FTSMatchesKeywords(t *testing.T) {
	ctx, _, _, tearDown := setupPool(t)
	defer tearDown()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`); err != nil {
		t.Fatal(err)
	}

	for _, m := range []Memory{
		{AgentID: "a", Dimension: "user", Tier: "long_term", Category: "preference",
			Title: "Uses pt-BR", Body: "Operator speaks Portuguese and wants concise answers.", Source: "operator"},
		{AgentID: "a", Dimension: "agent", Tier: "long_term", Category: "project",
			Title: "Postgres at 5434", Body: "Local maquinistadb listens on port 5434 under docker.", Source: "agent"},
		{AgentID: "a", Dimension: "agent", Tier: "daily", Category: "fact",
			Title: "Red herring", Body: "Go slices are reference types.", Source: "agent"},
	} {
		if _, err := Remember(ctx, pool, m); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := Search(ctx, pool, "a", "postgres docker", ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("FTS found nothing for 'postgres docker'")
	}
	if !strings.Contains(hits[0].Title, "Postgres") {
		t.Errorf("top hit = %q, want Postgres row", hits[0].Title)
	}
}

func TestFetchForInjection_PinnedAndLongTerm(t *testing.T) {
	ctx, _, _, tearDown := setupPool(t)
	defer tearDown()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`); err != nil {
		t.Fatal(err)
	}

	pinnedID, _ := Remember(ctx, pool, Memory{
		AgentID: "a", Dimension: "user", Tier: "daily", Category: "preference",
		Title: "daily-but-pinned", Body: "pinned sticks.", Source: "operator", Pinned: true,
	})
	_, _ = Remember(ctx, pool, Memory{
		AgentID: "a", Dimension: "agent", Tier: "long_term", Category: "fact",
		Title: "lt", Body: "long_term should be injected.", Source: "agent",
	})
	_, _ = Remember(ctx, pool, Memory{
		AgentID: "a", Dimension: "agent", Tier: "signal", Category: "other",
		Title: "signal", Body: "noise.", Source: "agent",
	})

	got, err := FetchForInjection(ctx, pool, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected some rows")
	}
	// Pinned row should come first.
	if got[0].ID != pinnedID {
		t.Errorf("first row id = %d, want pinned %d", got[0].ID, pinnedID)
	}
	// Signal-tier row should NOT be in injection set.
	for _, m := range got {
		if m.Tier == "signal" {
			t.Error("signal-tier row should not be returned by FetchForInjection")
		}
	}
}
