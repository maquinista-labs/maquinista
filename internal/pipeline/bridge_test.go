package pipeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// testPool spins a disposable Postgres with the full migration chain applied.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return pool
}

// execOK is the local mustExec (the db package one is not importable).
func execOK(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func seedIssue(n int) Issue {
	return Issue{
		ID:          fmt.Sprintf("uuid-%d", n),
		Identifier:  fmt.Sprintf("MAQ-%d", 100+n),
		Title:       fmt.Sprintf("Do thing %d", n),
		Description: "the body",
		URL:         fmt.Sprintf("https://linear.app/maq/MAQ-%d", 100+n),
	}
}

// updCall records one UpdateIssueState invocation.
type updCall struct {
	issueID, stateID string
}

// fakeLinear is the in-memory LinearAPI used by bridge and sync tests.
type fakeLinear struct {
	todo      []Issue
	todoErr   error
	states    map[string]string // state name -> id
	statesErr error
	updates   []updCall
	updErr    error
}

func (f *fakeLinear) TodoIssues(ctx context.Context, teamID string) ([]Issue, error) {
	return f.todo, f.todoErr
}

func (f *fakeLinear) WorkflowStates(ctx context.Context, teamID string) (map[string]string, error) {
	return f.states, f.statesErr
}

func (f *fakeLinear) UpdateIssueState(ctx context.Context, issueID, stateID string) (string, error) {
	f.updates = append(f.updates, updCall{issueID: issueID, stateID: stateID})
	if f.updErr != nil {
		return "", f.updErr
	}
	for name, id := range f.states {
		if id == stateID {
			return name, nil
		}
	}
	return "", nil
}

func TestClaim_InsertsTaskRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(1)
	created, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa")
	if err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}
	if !created {
		t.Fatal("first claim should create")
	}

	var status, title, project string
	var meta map[string]string
	if err := pool.QueryRow(ctx, `
		SELECT status, title, project_id, metadata FROM tasks
		WHERE metadata->>'linear_issue_id' = $1`, iss.ID,
	).Scan(&status, &title, &project, &meta); err != nil {
		t.Fatalf("task row: %v", err)
	}
	if status != "ready" {
		t.Errorf("status = %q, want ready (scheduler-claimable)", status)
	}
	if want := "[MAQ-101] Do thing 1"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	if project != "brisa" {
		t.Errorf("project_id = %q, want brisa", project)
	}
	if meta["linear_url"] != iss.URL {
		t.Errorf("metadata.linear_url = %q, want %q", meta["linear_url"], iss.URL)
	}
}

func TestClaim_InsertsMapRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(2)
	if created, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa"); err != nil || !created {
		t.Fatalf("ClaimIssue: created=%v err=%v", created, err)
	}

	var pend, last *string
	var attempts int
	if err := pool.QueryRow(ctx, `
		SELECT pending_state, last_synced_state, attempts
		FROM linear_issue_map WHERE linear_issue_id = $1`, iss.ID,
	).Scan(&pend, &last, &attempts); err != nil {
		t.Fatalf("map row: %v", err)
	}
	if pend == nil || *pend != "In Progress" {
		t.Errorf("pending_state = %v, want In Progress", pend)
	}
	if last != nil {
		t.Errorf("last_synced_state = %v, want NULL on a fresh claim", last)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
}

func TestClaim_Idempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(3)
	if _, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	created, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if created {
		t.Error("second claim should not create")
	}

	var tasks, maps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE metadata->>'linear_issue_id' = $1`, iss.ID).Scan(&tasks); err != nil || tasks != 1 {
		t.Errorf("tasks rows for issue = %d (err=%v), want exactly 1", tasks, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM linear_issue_map WHERE linear_issue_id = $1`, iss.ID).Scan(&maps); err != nil || maps != 1 {
		t.Errorf("map rows for issue = %d (err=%v), want exactly 1", maps, err)
	}
	var pend string
	if err := pool.QueryRow(ctx, `SELECT pending_state FROM linear_issue_map WHERE linear_issue_id = $1`, iss.ID).Scan(&pend); err != nil || pend != "In Progress" {
		t.Errorf("map row changed on re-claim: pend=%q err=%v", pend, err)
	}
}

func TestClaim_SchedulerQueryMatches(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(4)
	if _, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa"); err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}

	// The literal task-scheduler claim query (taskscheduler.go:94-105 shape).
	var taskID string
	var role *string
	err := pool.QueryRow(ctx, `
		SELECT id, metadata->>'role'
		FROM tasks t
		WHERE status = 'ready'
		  AND NOT EXISTS (
		        SELECT 1 FROM agents a
		        WHERE a.task_id = t.id AND a.status <> 'dead'
		      )
		ORDER BY priority DESC, created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&taskID, &role)
	if err != nil {
		t.Fatalf("scheduler query did not match the claimed task: %v", err)
	}
	var want string
	if err := pool.QueryRow(ctx, `SELECT id FROM tasks WHERE metadata->>'linear_issue_id' = $1`, iss.ID).Scan(&want); err != nil {
		t.Fatalf("seeded task: %v", err)
	}
	if taskID != want {
		t.Errorf("scheduler claimed %q, want the bridge task %q", taskID, want)
	}
	if role != nil {
		t.Errorf("bridge task carries role %q, want NULL (EX-02 seeds souls)", *role)
	}
}
