package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window) VALUES
			('alpha','s','wa'),
			('beta','s','wb')
	`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// simulateBetaReplies watches for new inbox rows addressed to beta and
// emits an outbox row in response. This mimics what the sidecar/relay
// would do once the agent responds.
func simulateBetaReplies(ctx context.Context, t *testing.T, pool *pgxpool.Pool, answer string) {
	t.Helper()
	go func() {
		seen := map[string]bool{}
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			rows, err := pool.Query(ctx, `
				SELECT id, conversation_id FROM agent_inbox
				WHERE agent_id='beta' AND status='pending'
			`)
			if err != nil {
				return
			}
			var pending []struct {
				ID, ConvoID any
			}
			for rows.Next() {
				var e struct{ ID, ConvoID any }
				_ = rows.Scan(&e.ID, &e.ConvoID)
				pending = append(pending, e)
			}
			rows.Close()
			for _, p := range pending {
				key := asString(p.ID)
				if seen[key] {
					continue
				}
				seen[key] = true
				out, _ := json.Marshal(map[string]string{"type": "text", "text": answer})
				_, _ = pool.Exec(ctx, `
					INSERT INTO agent_outbox
						(agent_id, conversation_id, in_reply_to, content)
					VALUES ('beta', $2, $1, $3::jsonb)
				`, p.ID, p.ConvoID, out)
			}
		}
	}()
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestAskAgent_ReturnsReply(t *testing.T) {
	pool := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	simulateBetaReplies(ctx, t, pool, "42")

	rep, err := AskAgent(ctx, pool, "alpha", "beta", "what's the answer?", 5*time.Second)
	if err != nil {
		t.Fatalf("AskAgent: %v", err)
	}
	if rep == nil || rep.Text == "" {
		t.Fatalf("empty reply: %+v", rep)
	}
	if rep.Text != "42" {
		t.Errorf("reply text = %q, want 42", rep.Text)
	}
}

func TestAskAgent_HandleResolution(t *testing.T) {
	pool := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Give beta a handle.
	if _, err := pool.Exec(ctx, `UPDATE agents SET handle='reviewer' WHERE id='beta'`); err != nil {
		t.Fatal(err)
	}

	simulateBetaReplies(ctx, t, pool, "ok")

	rep, err := AskAgent(ctx, pool, "alpha", "reviewer", "lgtm?", 5*time.Second)
	if err != nil {
		t.Fatalf("AskAgent by handle: %v", err)
	}
	if rep.Text != "ok" {
		t.Errorf("want ok, got %q", rep.Text)
	}
}

func TestAskAgent_Timeout(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// No simulated replier — AskAgent must time out.
	_, err := AskAgent(ctx, pool, "alpha", "beta", "never answered", 1*time.Second)
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("want ErrTimeout, got %v", err)
	}
}

func TestAskAgent_UnknownTarget(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	_, err := AskAgent(ctx, pool, "alpha", "ghost", "hi", 1*time.Second)
	if !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("want ErrUnknownAgent, got %v", err)
	}
}
