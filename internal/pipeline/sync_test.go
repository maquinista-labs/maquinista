package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedMappedTask(t *testing.T, pool *pgxpool.Pool, taskID, status, issueID, pend, last string, attempts int, nextInSecs float64) {
	t.Helper()
	execOK(t, pool, `INSERT INTO tasks (id, title, status) VALUES ($1, $2, $3)`, taskID, "task "+taskID, status)
	execOK(t, pool, `
		INSERT INTO linear_issue_map
		       (linear_issue_id, identifier, team_id, task_id, pending_state, last_synced_state, attempts, next_attempt_at)
		VALUES ($1, $2, 'team-maq', $3, NULLIF($4, ''), NULLIF($5, ''), $6, NOW() + make_interval(secs => $7))`,
		issueID, "MAQ-"+issueID, taskID, pend, last, attempts, nextInSecs)
}

func mapRow(t *testing.T, pool *pgxpool.Pool, issueID string) (pend, last *string, attempts int, next time.Time, synced *time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT pending_state, last_synced_state, attempts, next_attempt_at, synced_at
		FROM linear_issue_map WHERE linear_issue_id = $1`, issueID,
	).Scan(&pend, &last, &attempts, &next, &synced)
	if err != nil {
		t.Fatalf("map row %s: %v", issueID, err)
	}
	return
}

func TestSync_DerivedState(t *testing.T) {
	cases := map[string]string{
		"ready":            "In Progress",
		"claimed":          "In Progress",
		"review":           "In Review",
		"pending_approval": "Needs Human",
		"failed":           "Needs Human",
		"done":             "Done",
		"pending":          "", // unmapped: skip
	}
	for status, want := range cases {
		if got := DerivedState(status); got != want {
			t.Errorf("DerivedState(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestSync_PushesDueRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeLinear{states: map[string]string{
		"In Progress": "s-ip", "In Review": "s-ir", "Needs Human": "s-nh", "Done": "s-d", "Changes Requested": "s-cr",
	}}
	seedMappedTask(t, pool, "tsk-push", "review", "uuid-push", "In Review", "", 0, -5)

	n, err := ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("pushed %d rows, want 1", n)
	}
	if len(fake.updates) != 1 || fake.updates[0].issueID != "uuid-push" || fake.updates[0].stateID != "s-ir" {
		t.Fatalf("updates = %+v, want one push of uuid-push to s-ir", fake.updates)
	}
	pend, last, attempts, _, synced := mapRow(t, pool, "uuid-push")
	if last == nil || *last != "In Review" {
		t.Errorf("last_synced_state = %v, want In Review", last)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0 after success", attempts)
	}
	if synced == nil {
		t.Error("synced_at not stamped")
	}
	if pend == nil || *pend != "In Review" {
		t.Errorf("pending_state = %v, want unchanged In Review", pend)
	}
}

func TestSync_DerivedOverwriteRule(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeLinear{states: map[string]string{
		"In Progress": "s-ip", "In Review": "s-ir", "Needs Human": "s-nh", "Done": "s-d", "Changes Requested": "s-cr",
	}}

	// (a) Explicit unsynced pending wins over the derived value.
	seedMappedTask(t, pool, "tsk-explicit", "review", "uuid-explicit", "Changes Requested", "In Review", 0, -5)
	n, err := ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil || n != 1 {
		t.Fatalf("(a) pushed=%d err=%v, want 1/nil", n, err)
	}
	if len(fake.updates) != 1 || fake.updates[0].stateID != "s-cr" {
		t.Errorf("(a) updates = %+v, want the explicit Changes Requested push", fake.updates)
	}
	pend, last, _, _, _ := mapRow(t, pool, "uuid-explicit")
	if pend == nil || *pend != "Changes Requested" || last == nil || *last != "Changes Requested" {
		t.Errorf("(a) pend=%v last=%v, want both Changes Requested", pend, last)
	}

	// (b) Already-synced pending is replaced by the newer derived value.
	// Note: row (a) also re-derives here by design (AC 16): its explicit
	// push is now synced (pend==last) and its task is still 'review', so
	// 'In Review' takes over. Assert the full push set, order-agnostic.
	seedMappedTask(t, pool, "tsk-derived", "done", "uuid-derived", "In Review", "In Review", 0, -5)
	mark := len(fake.updates) // (a)'s pushes stay recorded; slice them off
	n, err = ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil || n != 2 {
		t.Fatalf("(b) pushed=%d err=%v, want 2/nil", n, err)
	}
	bUpdates := fake.updates[mark:]
	got := map[string]bool{}
	for _, u := range bUpdates {
		got[u.stateID] = true
	}
	if len(bUpdates) != 2 || !got["s-d"] || !got["s-ir"] {
		t.Errorf("(b) updates = %+v, want {s-d (uuid-derived), s-ir (uuid-explicit re-derived)}", bUpdates)
	}
	pend, last, _, _, _ = mapRow(t, pool, "uuid-derived")
	if pend == nil || *pend != "Done" || last == nil || *last != "Done" {
		t.Errorf("(b) pend=%v last=%v, want both Done", pend, last)
	}
}

func TestSync_BackoffOnFailure(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeLinear{
		states: map[string]string{"In Review": "s-ir"},
		updErr: errors.New("linear 502: upstream unavailable"),
	}
	seedMappedTask(t, pool, "tsk-boom", "review", "uuid-boom", "In Review", "", 0, -5)

	n, err := ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil {
		t.Fatalf("ReconcileOnce: %v (row errors must be bookkept, not returned)", err)
	}
	if n != 0 {
		t.Errorf("pushed=%d, want 0", n)
	}
	if len(fake.updates) != 1 {
		t.Fatalf("the push must still have been attempted: %+v", fake.updates)
	}
	_, last, attempts, next, _ := mapRow(t, pool, "uuid-boom")
	if last != nil {
		t.Errorf("last_synced_state = %v, want unchanged NULL", last)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if delay := time.Until(next); delay < 12*time.Second || delay > 18*time.Second {
		t.Errorf("next_attempt_at in %s, want ~15s (15 s base)", delay)
	}

	// Not due yet: the second tick must not re-attempt.
	n, err = ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil || n != 0 {
		t.Fatalf("second tick: pushed=%d err=%v, want 0/nil", n, err)
	}
	if len(fake.updates) != 1 {
		t.Errorf("second tick re-attempted a backoff row: %+v", fake.updates)
	}
	if _, _, attempts, _, _ = mapRow(t, pool, "uuid-boom"); attempts != 1 {
		t.Errorf("attempts after skipped tick = %d, want still 1", attempts)
	}
}

func TestSync_SkipsUnmappedStatus(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeLinear{states: map[string]string{"In Progress": "s-ip"}}
	seedMappedTask(t, pool, "tsk-skip", "pending", "uuid-skip", "", "", 0, -5)

	n, err := ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil || n != 0 {
		t.Fatalf("pushed=%d err=%v, want 0/nil", n, err)
	}
	if len(fake.updates) != 0 {
		t.Errorf("unmapped status was pushed: %+v", fake.updates)
	}
	pend, last, _, _, _ := mapRow(t, pool, "uuid-skip")
	if pend != nil || last != nil {
		t.Errorf("row touched on skip: pend=%v last=%v, want both NULL", pend, last)
	}
}
