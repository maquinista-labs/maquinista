package routing

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window) VALUES
			('alpha','s','wa'),
			('beta','s','wb'),
			('gamma','s','wg')
	`)
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func TestParseMention(t *testing.T) {
	cases := []struct {
		in       string
		wantID   string
		wantRest string
		wantOK   bool
	}{
		{"@alpha hello", "alpha", "hello", true},
		{"  @beta-42 with rest", "beta-42", "with rest", true},
		{"no mention here", "", "no mention here", false},
		{"@ alpha missing", "", "@ alpha missing", false},
		{"mid-text @alpha", "", "mid-text @alpha", false},
		{"@a-b_c", "a-b_c", "", true},
	}
	for _, c := range cases {
		id, rest, ok := ParseMention(c.in)
		if id != c.wantID || rest != c.wantRest || ok != c.wantOK {
			t.Errorf("ParseMention(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, id, rest, ok, c.wantID, c.wantRest, c.wantOK)
		}
	}
}

func TestResolve_AllTiers(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	t.Run("tier1_mention_no_binding", func(t *testing.T) {
		res, err := Resolve(ctx, pool, "u1", "100", ptrInt64(-1001), "@beta hello")
		if err != nil {
			t.Fatal(err)
		}
		if res.Tier != TierMention || res.AgentID != "beta" || res.Text != "hello" {
			t.Errorf("got %+v", res)
		}
		// Mention must NOT write a binding.
		var count int
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM topic_agent_bindings WHERE user_id='u1' AND thread_id='100' AND binding_type='owner'`).Scan(&count)
		if count != 0 {
			t.Errorf("mention wrote %d owner rows, want 0", count)
		}
	})

	t.Run("tier2_owner_binding", func(t *testing.T) {
		exec(t, pool, `INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id) VALUES (200, 'alpha', 'owner', 'u2', '200', -2001)`)
		res, err := Resolve(ctx, pool, "u2", "200", ptrInt64(-2001), "plain text")
		if err != nil {
			t.Fatal(err)
		}
		if res.Tier != TierOwnerBinding || res.AgentID != "alpha" || res.Text != "plain text" {
			t.Errorf("got %+v", res)
		}
		if res.BindingSet {
			t.Error("should not re-write existing owner binding")
		}
	})

	t.Run("tier3_global_default_writes_owner", func(t *testing.T) {
		// seed agent_settings.is_default for gamma
		exec(t, pool, `INSERT INTO agent_settings (agent_id, is_default) VALUES ('gamma', TRUE)`)
		res, err := Resolve(ctx, pool, "u3", "300", ptrInt64(-3001), "hi")
		if err != nil {
			t.Fatal(err)
		}
		if res.Tier != TierGlobalDefault || res.AgentID != "gamma" {
			t.Errorf("got %+v", res)
		}
		if !res.BindingSet {
			t.Error("tier3 should write owner binding on first use")
		}
		// Second call now hits tier2.
		res2, err := Resolve(ctx, pool, "u3", "300", ptrInt64(-3001), "again")
		if err != nil {
			t.Fatal(err)
		}
		if res2.Tier != TierOwnerBinding || res2.AgentID != "gamma" {
			t.Errorf("second call tier=%s agent=%s", res2.Tier, res2.AgentID)
		}
	})

	t.Run("tier4_picker_required", func(t *testing.T) {
		// Remove global default so no tier matches.
		exec(t, pool, `UPDATE agent_settings SET is_default=FALSE`)
		_, err := Resolve(ctx, pool, "u4", "400", ptrInt64(-4001), "no binding")
		if !errors.Is(err, ErrRequirePicker) {
			t.Errorf("err=%v, want ErrRequirePicker", err)
		}
	})
}

func TestConfirmPickerChoice_WritesBinding(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	res, err := ConfirmPickerChoice(ctx, pool, "u5", "500", ptrInt64(-5001), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != TierPicker || res.AgentID != "beta" || !res.BindingSet {
		t.Errorf("got %+v", res)
	}

	// Subsequent Resolve should go through tier-2.
	r2, err := Resolve(ctx, pool, "u5", "500", nil, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Tier != TierOwnerBinding || r2.AgentID != "beta" {
		t.Errorf("follow-up routing: %+v", r2)
	}
}

func TestSetUserDefault_OverwritesOwner(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := SetUserDefault(ctx, pool, "u6", "600", ptrInt64(-6001), "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := SetUserDefault(ctx, pool, "u6", "600", ptrInt64(-6001), "beta"); err != nil {
		t.Fatal(err)
	}
	var agentID string
	pool.QueryRow(ctx, `SELECT agent_id FROM topic_agent_bindings WHERE binding_type='owner' AND user_id='u6' AND thread_id='600'`).Scan(&agentID)
	if agentID != "beta" {
		t.Errorf("agent=%q, want beta", agentID)
	}
}

func TestSetGlobalDefault_UniqueDefault(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	exec(t, pool, `INSERT INTO agent_settings (agent_id) VALUES ('alpha'), ('beta')`)

	if err := SetGlobalDefault(ctx, pool, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := SetGlobalDefault(ctx, pool, "beta"); err != nil {
		t.Fatal(err)
	}

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_settings WHERE is_default=TRUE`).Scan(&count)
	if count != 1 {
		t.Errorf("defaults=%d, want 1", count)
	}
	var id string
	pool.QueryRow(ctx, `SELECT agent_id FROM agent_settings WHERE is_default=TRUE`).Scan(&id)
	if id != "beta" {
		t.Errorf("default=%q, want beta", id)
	}
}

// TestResolve_Tier3_RaceOneWinner: two goroutines resolve tier-3 for the
// same (user, thread). The partial unique index forces one writer, the
// other reads the committed row.
func TestResolve_Tier3_RaceOneWinner(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	exec(t, pool, `INSERT INTO agent_settings (agent_id, is_default) VALUES ('gamma', TRUE)`)

	var wg sync.WaitGroup
	results := make([]*Resolution, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := Resolve(ctx, pool, "race", "777", ptrInt64(-7001), "go")
			if err != nil {
				t.Errorf("resolve %d: %v", i, err)
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	writers := 0
	for _, r := range results {
		if r != nil && r.BindingSet {
			writers++
		}
		if r != nil && r.AgentID != "gamma" {
			t.Errorf("agent=%q, want gamma", r.AgentID)
		}
	}
	if writers != 1 {
		t.Errorf("writers=%d, want exactly 1", writers)
	}

	// Exactly one owner row in the DB.
	var rows int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM topic_agent_bindings WHERE binding_type='owner' AND user_id='race' AND thread_id='777'`).Scan(&rows)
	if rows != 1 {
		t.Errorf("owner rows=%d, want 1", rows)
	}
}

func ptrInt64(v int64) *int64 { return &v }
