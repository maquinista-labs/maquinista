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
		INSERT INTO ticket_issue_map
		       (issue_id, issue_key, team_id, task_id, pending_state, last_synced_state, attempts, next_attempt_at)
		VALUES ($1, $2, 'team-maq', $3, NULLIF($4, ''), NULLIF($5, ''), $6, NOW() + make_interval(secs => $7))`,
		issueID, "MAQ-"+issueID, taskID, pend, last, attempts, nextInSecs)
}

func mapRow(t *testing.T, pool *pgxpool.Pool, issueID string) (pend, last *string, attempts int, next time.Time, synced *time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT pending_state, last_synced_state, attempts, next_attempt_at, synced_at
		FROM ticket_issue_map WHERE issue_id = $1`, issueID,
	).Scan(&pend, &last, &attempts, &next, &synced)
	if err != nil {
		t.Fatalf("map row %s: %v", issueID, err)
	}
	return
}

func TestSync_DerivedState(t *testing.T) {
	cases := map[string]Column{
		"ready":            ColInProgress,
		"claimed":          ColInProgress,
		"review":           ColInReview,
		"pending_approval": ColNeedsHuman,
		"failed":           ColNeedsHuman,
		"done":             ColDone,
	}
	for status, want := range cases {
		got, ok := DerivedState(status)
		if !ok || got != want {
			t.Errorf("DerivedState(%q) = (%v, %v), want (%v, true)", status, got, ok, want)
		}
	}
	if got, ok := DerivedState("pending"); ok {
		t.Errorf("DerivedState(pending) = (%v, true), want unmapped (ok=false)", got)
	}
}

func TestSync_PushesDueRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeTickets{cols: fullCols()}
	seedMappedTask(t, pool, "tsk-push", "review", "uuid-push", ColInReview.String(), "", 0, -5)

	n, err := ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("pushed %d rows, want 1", n)
	}
	if len(fake.updates) != 1 || fake.updates[0].issueID != "uuid-push" || fake.updates[0].columnID != "s-ir" {
		t.Fatalf("updates = %+v, want one push of uuid-push to s-ir", fake.updates)
	}
	pend, last, attempts, _, synced := mapRow(t, pool, "uuid-push")
	if last == nil || *last != ColInReview.String() {
		t.Errorf("last_synced_state = %v, want %q", last, ColInReview.String())
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0 after success", attempts)
	}
	if synced == nil {
		t.Error("synced_at not stamped")
	}
	if pend == nil || *pend != ColInReview.String() {
		t.Errorf("pending_state = %v, want unchanged %q", pend, ColInReview.String())
	}
}

func TestSync_DerivedOverwriteRule(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeTickets{cols: fullCols()}

	// (a) Explicit unsynced pending wins over the derived value.
	seedMappedTask(t, pool, "tsk-explicit", "review", "uuid-explicit", ColChangesRequested.String(), ColInReview.String(), 0, -5)
	n, err := ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil || n != 1 {
		t.Fatalf("(a) pushed=%d err=%v, want 1/nil", n, err)
	}
	if len(fake.updates) != 1 || fake.updates[0].columnID != "s-cr" {
		t.Errorf("(a) updates = %+v, want the explicit Changes Requested push", fake.updates)
	}
	pend, last, _, _, _ := mapRow(t, pool, "uuid-explicit")
	if pend == nil || *pend != ColChangesRequested.String() || last == nil || *last != ColChangesRequested.String() {
		t.Errorf("(a) pend=%v last=%v, want both Changes Requested", pend, last)
	}

	// (b) Already-synced pending is replaced by the newer derived value.
	// Note: row (a) also re-derives here by design (AC 16): its explicit
	// push is now synced (pend==last) and its task is still 'review', so
	// 'In Review' takes over. Assert the full push set, order-agnostic.
	seedMappedTask(t, pool, "tsk-derived", "done", "uuid-derived", ColInReview.String(), ColInReview.String(), 0, -5)
	mark := len(fake.updates) // (a)'s pushes stay recorded; slice them off
	n, err = ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil || n != 2 {
		t.Fatalf("(b) pushed=%d err=%v, want 2/nil", n, err)
	}
	bUpdates := fake.updates[mark:]
	got := map[string]bool{}
	for _, u := range bUpdates {
		got[u.columnID] = true
	}
	if len(bUpdates) != 2 || !got["s-d"] || !got["s-ir"] {
		t.Errorf("(b) updates = %+v, want {s-d (uuid-derived), s-ir (uuid-explicit re-derived)}", bUpdates)
	}
	pend, last, _, _, _ = mapRow(t, pool, "uuid-derived")
	if pend == nil || *pend != ColDone.String() || last == nil || *last != ColDone.String() {
		t.Errorf("(b) pend=%v last=%v, want both Done", pend, last)
	}
}

func TestSync_BackoffOnFailure(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeTickets{
		cols:   map[Column]string{ColInReview: "s-ir"},
		updErr: errors.New("provider 502: upstream unavailable"),
	}
	seedMappedTask(t, pool, "tsk-boom", "review", "uuid-boom", ColInReview.String(), "", 0, -5)

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
	fake := &fakeTickets{cols: map[Column]string{ColInProgress: "s-ip"}}
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

func TestSync_SkipsNonCanonicalPending(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeTickets{cols: fullCols()}
	seedMappedTask(t, pool, "tsk-weird", "review", "uuid-weird", "Code Review", "", 0, -5)

	n, err := ReconcileOnce(ctx, pool, fake, "team-maq")
	if err != nil || n != 0 {
		t.Fatalf("pushed=%d err=%v, want 0/nil", n, err)
	}
	if len(fake.updates) != 0 {
		t.Errorf("non-canonical pending was pushed: %+v", fake.updates)
	}
	pend, last, _, _, _ := mapRow(t, pool, "uuid-weird")
	if pend == nil || *pend != "Code Review" || last != nil {
		t.Errorf("row mutated on non-canonical pending: pend=%v last=%v, want pend kept, last NULL", pend, last)
	}
}

// syncedPR reads ticket_issue_map.pr_url_synced for the issue.
func syncedPR(t *testing.T, pool *pgxpool.Pool, issueID string) *string {
	t.Helper()
	var synced *string
	if err := pool.QueryRow(context.Background(),
		`SELECT pr_url_synced FROM ticket_issue_map WHERE issue_id = $1`, issueID,
	).Scan(&synced); err != nil {
		t.Fatalf("pr_url_synced %s: %v", issueID, err)
	}
	return synced
}

// TestSync_IssueLinkExactlyOnce (MAQ-10): the PR URL is pushed to the issue
// exactly once per URL; later ticks and unchanged URLs never re-write.
func TestSync_IssueLinkExactlyOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := &fakeTickets{}
	seedMappedTask(t, pool, "tsk-link", "review", "uuid-link", "", "", 0, -5)

	// (a) No PR yet: nothing pushed, nothing booked.
	if n, err := SyncIssueLinks(ctx, pool, fake); err != nil || n != 0 {
		t.Fatalf("(a) pushed=%d err=%v, want 0/nil", n, err)
	}
	if len(fake.links) != 0 {
		t.Fatalf("(a) links = %+v, want none", fake.links)
	}

	// (b) PR opens: exactly one push, bookkeeping stamped.
	const pr1 = "https://github.com/maquinista-labs/maquinista/pull/11"
	execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`, "tsk-link", pr1)
	if n, err := SyncIssueLinks(ctx, pool, fake); err != nil || n != 1 {
		t.Fatalf("(b) pushed=%d err=%v, want 1/nil", n, err)
	}
	if len(fake.links) != 1 || fake.links[0].issueID != "uuid-link" || fake.links[0].url != pr1 {
		t.Fatalf("(b) links = %+v, want one push of %s", fake.links, pr1)
	}
	if got := syncedPR(t, pool, "uuid-link"); got == nil || *got != pr1 {
		t.Fatalf("(b) pr_url_synced = %v, want %q", got, pr1)
	}

	// (c) Idempotent across ticks: unchanged URL → no further writes.
	if n, err := SyncIssueLinks(ctx, pool, fake); err != nil || n != 0 {
		t.Fatalf("(c) pushed=%d err=%v, want 0/nil", n, err)
	}
	if len(fake.links) != 1 {
		t.Fatalf("(c) links = %+v, want still exactly 1 (no comment spam)", fake.links)
	}

	// (d) New PR on the task: exactly one more push.
	const pr2 = pr1 + "x"
	execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`, "tsk-link", pr2)
	if n, err := SyncIssueLinks(ctx, pool, fake); err != nil || n != 1 {
		t.Fatalf("(d) pushed=%d err=%v, want 1/nil", n, err)
	}
	if len(fake.links) != 2 || fake.links[1].url != pr2 {
		t.Fatalf("(d) links = %+v, want second push of %s", fake.links, pr2)
	}
}

// TestSync_IssueLinkRetriesAfterFailure: a provider failure is logged and the
// row stays unsynced, so the next tick retries — still exactly one successful
// write per URL.
func TestSync_IssueLinkRetriesAfterFailure(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const pr = "https://github.com/maquinista-labs/maquinista/pull/12"
	fake := &fakeTickets{linkErr: errors.New("linear down")}
	seedMappedTask(t, pool, "tsk-linkretry", "review", "uuid-linkretry", "", "", 0, -5)
	execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`, "tsk-linkretry", pr)

	if n, err := SyncIssueLinks(ctx, pool, fake); err != nil || n != 0 {
		t.Fatalf("failed tick: pushed=%d err=%v, want 0/nil", n, err)
	}
	if got := syncedPR(t, pool, "uuid-linkretry"); got != nil {
		t.Fatalf("pr_url_synced = %v, want NULL after failed push", got)
	}

	fake.linkErr = nil
	if n, err := SyncIssueLinks(ctx, pool, fake); err != nil || n != 1 {
		t.Fatalf("retry tick: pushed=%d err=%v, want 1/nil", n, err)
	}
	if got := syncedPR(t, pool, "uuid-linkretry"); got == nil || *got != pr {
		t.Fatalf("pr_url_synced = %v, want %q", got, pr)
	}

	// And a third tick stays quiet.
	if n, err := SyncIssueLinks(ctx, pool, fake); err != nil || n != 0 {
		t.Fatalf("post-success tick: pushed=%d err=%v, want 0/nil", n, err)
	}
	if len(fake.links) != 2 { // failed attempt + successful retry
		t.Fatalf("links = %+v, want 2 attempts total", fake.links)
	}
}
