package scheduler

import (
	"context"
	"fmt"
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
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('alpha','s','w')`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return pool
}

func TestNextAfter_BasicAndTZ(t *testing.T) {
	// UTC half-hour: 12:00 → 12:30 → 13:00.
	ref := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	next, err := nextAfter("*/30 * * * *", "UTC", ref)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(time.Date(2026, 4, 12, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("UTC next = %s, want 12:30", next)
	}

	// São Paulo: midnight-local schedule resolves to correct UTC instant.
	next2, err := nextAfter("0 0 * * *", "America/Sao_Paulo", ref)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	if next2.In(loc).Hour() != 0 {
		t.Errorf("SP next hour = %d, want 0", next2.In(loc).Hour())
	}
}

func TestFireOne_EnqueuesInboxAndAdvances(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Seed a job whose next_run_at is in the past.
	past := time.Now().Add(-5 * time.Minute).UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_jobs (name, cron_expr, agent_id, prompt, next_run_at)
		VALUES ('daily-reel', '0 8 * * *', 'alpha',
		        '{"type":"command","text":"/publish-reel"}'::jsonb, $1)
	`, past); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.Now() }
	fired, err := FireOne(ctx, pool, cfg)
	if err != nil || !fired {
		t.Fatalf("fired=%v err=%v", fired, err)
	}

	// Inbox row exists with expected from_kind + external_msg_id prefix.
	var count int
	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_inbox
		WHERE from_kind='scheduled' AND external_msg_id LIKE 'sched:%'
	`).Scan(&count)
	if count != 1 {
		t.Errorf("inbox rows=%d, want 1", count)
	}

	// next_run_at advanced to a future time.
	var next time.Time
	pool.QueryRow(ctx, `SELECT next_run_at FROM scheduled_jobs WHERE name='daily-reel'`).Scan(&next)
	if !next.After(time.Now()) {
		t.Errorf("next_run_at=%s, want future", next)
	}
}

// TestFireOne_MissedWindow_SingleCatchup — a job that was scheduled every
// 30 min and has been unfired for 3 hours fires exactly once on recovery.
func TestFireOne_MissedWindow_SingleCatchup(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Last scheduled fire 3h ago, cron every 30m.
	past := time.Now().Add(-3 * time.Hour).UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_jobs (name, cron_expr, agent_id, prompt, next_run_at)
		VALUES ('heartbeat', '*/30 * * * *', 'alpha',
		        '{"type":"command","text":"/hb"}'::jsonb, $1)
	`, past); err != nil {
		t.Fatal(err)
	}

	// Fire repeatedly until drain reports no work.
	cfg := DefaultConfig()
	fires := 0
	for {
		fired, err := FireOne(ctx, pool, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !fired {
			break
		}
		fires++
		if fires > 2 {
			t.Fatalf("too many fires (%d) — catch-up did not collapse", fires)
		}
	}
	if fires != 1 {
		t.Errorf("fires=%d, want 1 (single catch-up)", fires)
	}

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE from_kind='scheduled'`).Scan(&count)
	if count != 1 {
		t.Errorf("inbox rows=%d, want 1", count)
	}
}

// TestFireOne_Idempotent_OnConflict — re-insert with the same external_msg_id
// (simulating a scheduler restart that fires the same ts twice) collapses
// to one inbox row via ON CONFLICT DO NOTHING.
func TestFireOne_Idempotent_OnConflict(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Use a fixed clock so both fires generate the same external_msg_id.
	fixed := time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)
	past := fixed.Add(-time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_jobs (name, cron_expr, agent_id, prompt, next_run_at)
		VALUES ('heartbeat', '*/30 * * * *', 'alpha',
		        '{"type":"command","text":"/hb"}'::jsonb, $1)
	`, past); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return fixed }

	fired, err := FireOne(ctx, pool, cfg)
	if err != nil || !fired {
		t.Fatalf("first: fired=%v err=%v", fired, err)
	}

	// Reset next_run_at to the past again, still at the same (past) fire_ts.
	if _, err := pool.Exec(ctx, `UPDATE scheduled_jobs SET next_run_at=$1 WHERE name='heartbeat'`, past); err != nil {
		t.Fatal(err)
	}

	// Second fire — same external_msg_id (derived from the same nextRunAt)
	// — should collapse to no new inbox row.
	if _, err := FireOne(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE from_kind='scheduled'`).Scan(&count)
	if count != 1 {
		t.Errorf("inbox rows=%d, want 1 (idempotent)", count)
	}
}

// TestFireOne_WarmSpawn_CalledWhenWithinWindow: a job with
// warm_spawn_before=10m whose next_run_at is within that window calls
// EnsureLive at fire time.
func TestFireOne_WarmSpawn_CalledWhenWithinWindow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Minute).UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_jobs (name, cron_expr, agent_id, prompt, next_run_at, warm_spawn_before)
		VALUES ('warm', '*/30 * * * *', 'alpha',
		        '{"type":"command","text":"/x"}'::jsonb, $1, INTERVAL '10 minutes')
	`, past); err != nil {
		t.Fatal(err)
	}

	called := ""
	cfg := DefaultConfig()
	cfg.EnsureLive = func(ctx context.Context, id string) error {
		called = id
		return nil
	}
	if _, err := FireOne(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	if called != "alpha" {
		t.Errorf("EnsureLive called with %q, want alpha", called)
	}
}

func TestParsePgInterval(t *testing.T) {
	d, err := parsePgInterval("00:10:00")
	if err != nil || d != 10*time.Minute {
		t.Errorf("hh:mm:ss parse: d=%s err=%v", d, err)
	}
}

// TestFireOne_SoulTemplateID_CallsSpawnFunc: when a job has soul_template_id
// set, SpawnFunc is called with correct FiredJob fields.
func TestFireOne_SoulTemplateID_CallsSpawnFunc(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Insert a soul_template first (FK requirement).
	if _, err := pool.Exec(ctx, `
		INSERT INTO soul_templates (id, name, role, goal)
		VALUES ('tpl-1', 'Test Template', 'executor', 'do stuff')
	`); err != nil {
		t.Fatalf("seed soul_template: %v", err)
	}

	past := time.Now().Add(-5 * time.Minute).UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_jobs (name, cron_expr, soul_template_id, prompt, next_run_at)
		VALUES ('fresh-job', '0 8 * * *', 'tpl-1', '{"type":"text","text":"hello"}'::jsonb, $1)
	`, past); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	var gotJob FiredJob
	cfg := DefaultConfig()
	cfg.SpawnFunc = func(ctx context.Context, job FiredJob) error {
		gotJob = job
		return nil
	}

	fired, err := FireOne(ctx, pool, cfg)
	if err != nil || !fired {
		t.Fatalf("fired=%v err=%v", fired, err)
	}
	if gotJob.SoulTemplateID != "tpl-1" {
		t.Errorf("SoulTemplateID = %q, want tpl-1", gotJob.SoulTemplateID)
	}
	if gotJob.Name != "fresh-job" {
		t.Errorf("Name = %q, want fresh-job", gotJob.Name)
	}
	// next_run_at should have advanced.
	var next time.Time
	pool.QueryRow(ctx, `SELECT next_run_at FROM scheduled_jobs WHERE name='fresh-job'`).Scan(&next)
	if !next.After(time.Now()) {
		t.Errorf("next_run_at=%s, want future", next)
	}
}

// TestFireOne_SoulTemplateID_NoSpawnFunc_Skips: SpawnFunc=nil → no panic,
// job advances next_run_at.
func TestFireOne_SoulTemplateID_NoSpawnFunc_Skips(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO soul_templates (id, name, role, goal)
		VALUES ('tpl-2', 'T2', 'executor', 'x')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	past := time.Now().Add(-5 * time.Minute).UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_jobs (name, cron_expr, soul_template_id, prompt, next_run_at)
		VALUES ('skip-job', '0 8 * * *', 'tpl-2', '{"type":"text","text":"hi"}'::jsonb, $1)
	`, past); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	cfg := DefaultConfig() // SpawnFunc = nil
	fired, err := FireOne(ctx, pool, cfg)
	if err != nil || !fired {
		t.Fatalf("fired=%v err=%v", fired, err)
	}
	// next_run_at should have advanced even though SpawnFunc was nil.
	var next time.Time
	pool.QueryRow(ctx, `SELECT next_run_at FROM scheduled_jobs WHERE name='skip-job'`).Scan(&next)
	if !next.After(time.Now()) {
		t.Errorf("next_run_at=%s, want future", next)
	}
}

// TestFireOne_AgentID_Legacy_InboxInject: existing agent_id path still works.
func TestFireOne_AgentID_Legacy_InboxInject(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	past := time.Now().Add(-5 * time.Minute).UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_jobs (name, cron_expr, agent_id, prompt, next_run_at)
		VALUES ('legacy-job', '0 8 * * *', 'alpha', '{"type":"text","text":"go"}'::jsonb, $1)
	`, past); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	cfg := DefaultConfig()
	fired, err := FireOne(ctx, pool, cfg)
	if err != nil || !fired {
		t.Fatalf("fired=%v err=%v", fired, err)
	}

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE from_kind='scheduled'`).Scan(&count)
	if count != 1 {
		t.Errorf("inbox rows=%d, want 1", count)
	}
}

// Compile-time shims to keep the mailbox import in use across refactors.
var _ = fmt.Sprintf
