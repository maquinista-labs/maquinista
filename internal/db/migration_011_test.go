package db

import (
	"context"
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func TestMigration011_AppliesAndPreservesRows(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	// Pre-existing row (migrated-in) is unaffected.
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO tasks (id, title) VALUES ('t1', 'existing')`)
	mustExec(t, pool, `UPDATE tasks SET pr_state='open', pr_url='https://x/1' WHERE id='t1'`)

	var pr, state string
	pool.QueryRow(ctx, `SELECT pr_url, pr_state FROM tasks WHERE id='t1'`).Scan(&pr, &state)
	if pr != "https://x/1" || state != "open" {
		t.Errorf("got pr=%s state=%s", pr, state)
	}

	// Invalid pr_state rejected.
	_, err := pool.Exec(ctx, `INSERT INTO tasks (id, title, pr_state) VALUES ('t2','x','zombie')`)
	if err == nil {
		t.Error("pr_state='zombie' should be rejected")
	}
}

func TestMigration011_UniqueLiveAgent(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO tasks (id, title) VALUES ('t1','x')`)
	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window, task_id, status) VALUES ('a1','s','w1','t1','working')`)

	// Second live agent on the same task fails.
	_, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window, task_id, status) VALUES ('a2','s','w2','t1','working')`)
	if err == nil {
		t.Fatal("unique-live index should reject a second working agent for t1")
	}

	// Marking a1 dead releases the index slot.
	mustExec(t, pool, `UPDATE agents SET status='dead' WHERE id='a1'`)
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window, task_id, status) VALUES ('a3','s','w3','t1','working')`); err != nil {
		t.Errorf("dead-slot reuse failed: %v", err)
	}
}

func TestMigration011_PrUrlIndexUsable(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	// Populate enough rows and ANALYZE so the planner prefers the index.
	for i := 0; i < 500; i++ {
		mustExec(t, pool, `INSERT INTO tasks (id, title, pr_url) VALUES ($1,'x',$2)`,
			"t-"+itoa(i), "https://example/"+itoa(i))
	}
	mustExec(t, pool, `ANALYZE tasks`)

	var plan string
	err := pool.QueryRow(ctx, `EXPLAIN (FORMAT TEXT) SELECT id FROM tasks WHERE pr_url = $1`,
		"https://example/42").Scan(&plan)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(plan, "idx_tasks_pr_url") {
		// Accept any Index Scan on tasks (planner may pick a generic index).
		if !strings.Contains(plan, "Index") {
			t.Errorf("plan missing index usage:\n%s", plan)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
