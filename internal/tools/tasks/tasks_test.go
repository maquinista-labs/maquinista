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
