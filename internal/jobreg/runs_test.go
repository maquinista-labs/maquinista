package jobreg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func runsSetup(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('alpha','s','w')`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	id, err := AddSchedule(context.Background(), pool, Schedule{
		Name: "daily-reel", Cron: "0 8 * * *", AgentID: "alpha",
		Prompt: map[string]any{"type": "command", "text": "/run"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool, id
}

func TestJobRunsView_JoinsInboxAndOutbox(t *testing.T) {
	pool, jobID := runsSetup(t)
	ctx := context.Background()

	// Fire the job manually by inserting a scheduled inbox row whose
	// from_id matches the job uuid.
	inboxID := uuid.New()
	outboxID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_inbox (id, agent_id, from_kind, from_id, origin_channel, external_msg_id, content, status, processed_at)
		VALUES ($1, 'alpha', 'scheduled', $2, 'scheduled', $3, '{"type":"command","text":"x"}'::jsonb, 'processed', NOW())
	`, inboxID, jobID, "sched:"+jobID+":x"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_outbox (id, agent_id, in_reply_to, content)
		VALUES ($1, 'alpha', $2, '{"text":"done"}'::jsonb)
	`, outboxID, inboxID); err != nil {
		t.Fatal(err)
	}

	runs, err := JobRunsByName(ctx, pool, "daily-reel", 25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs=%d, want 1", len(runs))
	}
	if runs[0].OutboxID == nil || *runs[0].OutboxID != outboxID.String() {
		t.Errorf("outbox join wrong: %v", runs[0].OutboxID)
	}
	if runs[0].Status != "processed" {
		t.Errorf("status=%q", runs[0].Status)
	}
}

func TestJobRunsView_FailedJobShowsError(t *testing.T) {
	pool, jobID := runsSetup(t)
	ctx := context.Background()

	msg := "boom"
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_inbox (agent_id, from_kind, from_id, origin_channel, external_msg_id, content, status, last_error, attempts, max_attempts)
		VALUES ('alpha', 'scheduled', $1, 'scheduled', 'sched:1', '{"text":"x"}'::jsonb, 'failed', $2, 5, 5)
	`, jobID, msg); err != nil {
		t.Fatal(err)
	}

	runs, err := JobRunsByName(ctx, pool, "daily-reel", 25, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%d err=%v", len(runs), err)
	}
	if runs[0].Status != "failed" || runs[0].LastError == nil || *runs[0].LastError != msg {
		t.Errorf("got %+v", runs[0])
	}
}

// TestJobRunsView_Pagination: 60 runs, pages of 25 → 25/25/10.
func TestJobRunsView_Pagination(t *testing.T) {
	pool, jobID := runsSetup(t)
	ctx := context.Background()

	base := time.Now().Add(-time.Hour)
	for i := 0; i < 60; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_inbox (agent_id, from_kind, from_id, origin_channel, external_msg_id, content, enqueued_at)
			VALUES ('alpha', 'scheduled', $1, 'scheduled', $2, '{"text":"x"}'::jsonb, $3)
		`, jobID, fmt.Sprintf("sched:%d", i), base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	page1, _ := JobRunsByName(ctx, pool, "daily-reel", 25, 0)
	page2, _ := JobRunsByName(ctx, pool, "daily-reel", 25, 25)
	page3, _ := JobRunsByName(ctx, pool, "daily-reel", 25, 50)

	if len(page1) != 25 || len(page2) != 25 || len(page3) != 10 {
		t.Errorf("pages = %d/%d/%d, want 25/25/10", len(page1), len(page2), len(page3))
	}
	// Pages are ordered newest first — enqueued_at on page1[0] > page2[0].
	if !page1[0].EnqueuedAt.After(page2[0].EnqueuedAt) {
		t.Error("pagination order wrong")
	}
}
