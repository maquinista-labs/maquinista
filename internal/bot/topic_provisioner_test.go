package bot

// Regression coverage for the 2026-10-06 incident (MAQ-32): a pane-liveness
// hygiene sweep flipped the synthetic 'pipeline' notifier row to 'dead' and
// closeOrphanedTopics — treating ANY dead agent as orphanable — closed the
// Pipeline topic and deleted its owner binding, silently dropping every
// pipeline notification. The notifier is not orphanable: only role='user'
// agents lose topics here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// seedAgent inserts (or resets — migration 036 seeds 'pipeline') an agents
// row with only the NOT NULL columns + role.
func seedAgent(t *testing.T, pool *pgxpool.Pool, id, role, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window, role, status) VALUES ($1, 'maquinista', $2, $3, $4)
		 ON CONFLICT (id) DO UPDATE SET role = EXCLUDED.role, status = EXCLUDED.status`,
		id, id, role, status); err != nil {
		t.Fatalf("seed agent %s: %v", id, err)
	}
}

// seedBinding inserts an owner binding for an agent.
func seedBinding(t *testing.T, pool *pgxpool.Pool, topic int64, agentID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
		 VALUES ($1, $2, 'owner', '100', $3, -100)`,
		topic, agentID, fmt.Sprintf("%d", topic)); err != nil {
		t.Fatalf("seed binding %s: %v", agentID, err)
	}
}

func TestCloseOrphanedTopics_SparesNotifierAndTaskRoles(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	// pipeline notifier swept dead (the incident's precondition), a retired
	// user agent (genuinely orphanable), and a live user agent (must keep
	// its topic).
	seedAgent(t, pool, "pipeline", "notifier", "dead")
	seedAgent(t, pool, "user-old", "user", "archived")
	seedAgent(t, pool, "user-live", "user", "working")
	seedBinding(t, pool, 11228, "pipeline")
	seedBinding(t, pool, 200, "user-old")
	seedBinding(t, pool, 300, "user-live")

	// Fake Telegram API: record closeForumTopic calls, succeed at everything.
	// getMe (fired by NewBotAPIWithClient) must return a User object.
	var closedTopics []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var result any = true
		if method == "getMe" {
			result = map[string]any{"id": 42, "is_bot": true, "first_name": "testbot", "username": "testbot"}
		} else if method == "closeForumTopic" {
			_ = r.ParseForm()
			closedTopics = append(closedTopics, r.FormValue("message_thread_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer srv.Close()

	api, err := tgbotapi.NewBotAPIWithClient("test", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatalf("fake bot api: %v", err)
	}
	b := &Bot{
		config: &config.Config{AllowedUsers: []int64{100}, AllowedGroups: []int64{-100}},
		state:  state.NewState(),
		api:    api,
	}

	if err := b.closeOrphanedTopics(ctx, pool); err != nil {
		t.Fatalf("closeOrphanedTopics: %v", err)
	}

	// The notifier keeps its binding — the Pipeline topic must stay open.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM topic_agent_bindings WHERE agent_id = 'pipeline' AND binding_type = 'owner'`).Scan(&n); err != nil {
		t.Fatalf("pipeline binding query: %v", err)
	}
	if n != 1 {
		t.Fatalf("pipeline notifier binding count = %d, want 1 (binding must survive the agent being dead)", n)
	}

	// The live user agent keeps its binding too.
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM topic_agent_bindings WHERE agent_id = 'user-live' AND binding_type = 'owner'`).Scan(&n); err != nil {
		t.Fatalf("user-live binding query: %v", err)
	}
	if n != 1 {
		t.Fatalf("live user binding count = %d, want 1", n)
	}

	// The retired user agent loses binding and topic — the only close.
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM topic_agent_bindings WHERE agent_id = 'user-old'`).Scan(&n); err != nil {
		t.Fatalf("user-old binding query: %v", err)
	}
	if n != 0 {
		t.Fatalf("archived user binding count = %d, want 0 (binding must be removed)", n)
	}
	if len(closedTopics) != 1 || closedTopics[0] != "200" {
		t.Fatalf("closed topics = %v, want exactly [200]", closedTopics)
	}
}
