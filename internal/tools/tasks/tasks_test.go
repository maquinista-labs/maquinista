package tasks

import (
	"context"
	"strings"
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
	return pool
}

func TestCreateTask_RequiresTitleAndID(t *testing.T) {
	pool := setup(t)
	err := CreateTask(context.Background(), pool, Task{Title: "x"})
	if err == nil {
		t.Error("should reject missing id")
	}
	err = CreateTask(context.Background(), pool, Task{ID: "t1"})
	if err == nil {
		t.Error("should reject missing title")
	}
}

func TestCreateTask_ImplementorRequiresWorktree(t *testing.T) {
	pool := setup(t)
	err := CreateTask(context.Background(), pool, Task{
		ID: "t1", Title: "impl", Role: "implementor",
	})
	if err == nil || !strings.Contains(err.Error(), "worktree_path") {
		t.Errorf("expected worktree_path error, got %v", err)
	}

	wt := "/tmp/wt"
	err = CreateTask(context.Background(), pool, Task{
		ID: "t2", Title: "impl", Role: "implementor", WorktreePath: &wt,
	})
	if err != nil {
		t.Errorf("valid implementor rejected: %v", err)
	}
}

func TestValidateDAG_DetectsCycle(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, id := range []string{"A", "B", "C"} {
		if err := CreateTask(ctx, pool, Task{ID: id, Title: id}); err != nil {
			t.Fatal(err)
		}
	}

	// Valid DAG: A → B → C.
	if err := AddDep(ctx, pool, "B", "A"); err != nil {
		t.Fatal(err)
	}
	if err := AddDep(ctx, pool, "C", "B"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDAG(ctx, pool); err != nil {
		t.Errorf("expected clean DAG, got %v", err)
	}

	// Introduce cycle C → A.
	if err := AddDep(ctx, pool, "A", "C"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDAG(ctx, pool); err == nil {
		t.Error("expected cycle error")
	}
}

func TestMarkMerged_UnblocksDependents(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := CreateTask(ctx, pool, Task{ID: "A", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := CreateTask(ctx, pool, Task{ID: "B", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := AddDep(ctx, pool, "B", "A"); err != nil {
		t.Fatal(err)
	}

	// B starts 'pending' (blocked on A). Merge A.
	if err := SetPRUrl(ctx, pool, "A", "https://x/1"); err != nil {
		t.Fatal(err)
	}
	if err := MarkMerged(ctx, pool, "A"); err != nil {
		t.Fatal(err)
	}

	var bStatus, aStatus, aPR string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='A'`).Scan(&aStatus)
	pool.QueryRow(ctx, `SELECT pr_state FROM tasks WHERE id='A'`).Scan(&aPR)
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='B'`).Scan(&bStatus)

	if aStatus != "done" || aPR != "merged" {
		t.Errorf("A status=%q pr_state=%q", aStatus, aPR)
	}
	if bStatus != "ready" {
		t.Errorf("B status=%q, want ready (trigger should cascade)", bStatus)
	}
}

// MAQ-22: the PR-opened flip announces exactly once per transition —
// idempotent re-set with the same URL is silent, a new URL re-announces.
func TestSetPRUrl_NotifyOncePerTransition(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := CreateTask(ctx, pool, Task{ID: "PR1", Title: "pr opener"}); err != nil {
		t.Fatal(err)
	}
	notifyTexts := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT content->>'text' FROM agent_outbox
			WHERE agent_id = 'pipeline'
			ORDER BY created_at, id
		`)
		if err != nil {
			t.Fatalf("query outbox: %v", err)
		}
		defer rows.Close()
		var texts []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			texts = append(texts, s)
		}
		return texts
	}

	// First set: the transition fires and announces once, with the URL.
	if err := SetPRUrl(ctx, pool, "PR1", "https://x/pull/1"); err != nil {
		t.Fatalf("first SetPRUrl: %v", err)
	}
	texts := notifyTexts()
	if len(texts) != 1 {
		t.Fatalf("after first set: %d notes, want 1", len(texts))
	}
	for _, want := range []string{"📤", "PR opened", "pr opener", "https://x/pull/1"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("note %q missing %q", texts[0], want)
		}
	}

	// Idempotent re-set (same URL, same state): no new note.
	if err := SetPRUrl(ctx, pool, "PR1", "https://x/pull/1"); err != nil {
		t.Fatalf("idempotent SetPRUrl: %v", err)
	}
	if texts := notifyTexts(); len(texts) != 1 {
		t.Fatalf("after re-set: %d notes, want still 1", len(texts))
	}

	// A NEW url is a new PR-opened transition: announces again.
	if err := SetPRUrl(ctx, pool, "PR1", "https://x/pull/2"); err != nil {
		t.Fatalf("new-url SetPRUrl: %v", err)
	}
	texts = notifyTexts()
	if len(texts) != 2 || !strings.Contains(texts[1], "https://x/pull/2") {
		t.Fatalf("after new url: %d notes %q, want second note with new url", len(texts), texts)
	}

	// Unknown task still errors.
	if err := SetPRUrl(ctx, pool, "nope", "https://x/pull/3"); err == nil || !strings.Contains(err.Error(), "no task") {
		t.Errorf("unknown task err = %v, want 'no task'", err)
	}
	if texts := notifyTexts(); len(texts) != 2 {
		t.Fatalf("failed set produced a note: %d notes, want 2", len(texts))
	}
}

func TestTaskByPR_LookupAndMiss(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := CreateTask(ctx, pool, Task{ID: "A", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := SetPRUrl(ctx, pool, "A", "https://github.com/x/y/pull/42"); err != nil {
		t.Fatal(err)
	}
	id, err := TaskByPR(ctx, pool, "https://github.com/x/y/pull/42")
	if err != nil || id != "A" {
		t.Errorf("got id=%q err=%v", id, err)
	}
	if _, err := TaskByPR(ctx, pool, "https://example/nope"); err == nil {
		t.Error("expected miss")
	}
}

func TestMarkClosed_FailsTask(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := CreateTask(ctx, pool, Task{ID: "A", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := SetPRUrl(ctx, pool, "A", "https://x/1"); err != nil {
		t.Fatal(err)
	}
	if err := MarkClosed(ctx, pool, "A"); err != nil {
		t.Fatal(err)
	}
	var status, pr string
	pool.QueryRow(ctx, `SELECT status, pr_state FROM tasks WHERE id='A'`).Scan(&status, &pr)
	if status != "failed" || pr != "closed" {
		t.Errorf("status=%q pr_state=%q", status, pr)
	}
}

func TestRelease_RetiresImplementorOnly(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := CreateTask(ctx, pool, Task{ID: "R", Title: "R"}); err != nil {
		t.Fatal(err)
	}
	seedAgent := func(id, role, status, taskID string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
			                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
			VALUES ($1, 'sess', $1, $2, $4, $3, 'pi', '/tmp/wt', $1, NOW(), NOW(), FALSE)
		`, id, role, status, taskID)
		if err != nil {
			t.Fatal(err)
		}
	}
	// uq_agents_task_live is per-task: the decoy reviewer lives on its own task.
	if err := CreateTask(ctx, pool, Task{ID: "R2", Title: "R2"}); err != nil {
		t.Fatal(err)
	}
	seedAgent("implementor-R", "implementor", "running", "R")
	seedAgent("reviewer-R", "reviewer", "running", "R2")

	n, err := Release(ctx, pool, "R")
	if err != nil || n != 1 {
		t.Fatalf("Release = (%d,%v), want (1,nil)", n, err)
	}
	var implStatus, revStatus string
	pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='implementor-R'`).Scan(&implStatus)
	pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='reviewer-R'`).Scan(&revStatus)
	if implStatus != "dead" {
		t.Errorf("implementor status = %q, want dead", implStatus)
	}
	if revStatus != "running" {
		t.Errorf("reviewer status = %q, want running (untouched)", revStatus)
	}
	var taskStatus string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='R'`).Scan(&taskStatus)
	if taskStatus != "pending" {
		t.Errorf("task status = %q, want pending (untouched)", taskStatus)
	}

	// Idempotent: second release retires nothing.
	if n, err := Release(ctx, pool, "R"); err != nil || n != 0 {
		t.Fatalf("second Release = (%d,%v), want (0,nil)", n, err)
	}
}
