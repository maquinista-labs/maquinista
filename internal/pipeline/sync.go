package pipeline

// sync (ADR-0005/0006): the mirror half of the bridge. Every tick derives
// the target canonical column from tasks.status and pushes pending
// transitions with exponential backoff. The bookkeeping is diff-based —
// pending_state is the desired canonical column, last_synced_state the last
// one successfully pushed — so a missed tick or a provider outage self-heals
// without losing transitions. Only canonical column names are ever stored.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const syncInterval = 10 * time.Second

// DerivedState maps tasks.status → the canonical column it mirrors. The bool
// is false for unmapped statuses (skip). Review-loop statuses (EX-03) derive
// directly: `changes_requested` and `ready_to_merge` are written by the
// dispatch loop's verdict transitions, `review` by the done-path branch. An
// operator's explicit pending_state (set via the map row) still wins over
// every derived value until synced.
func DerivedState(status string) (Column, bool) {
	switch status {
	case "ready", "claimed":
		return ColInProgress, true
	case "review":
		return ColInReview, true
	case "changes_requested":
		return ColChangesRequested, true
	case "pending_approval", "failed":
		return ColNeedsHuman, true
	case "ready_to_merge":
		return ColReadyToMerge, true
	case "done":
		return ColDone, true
	default:
		return 0, false
	}
}

// backoffDelay is the retry delay after `failures` consecutive failures:
// 15 s doubling, capped at 10 minutes.
func backoffDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	shift := failures - 1
	if shift > 6 {
		shift = 6
	}
	d := 15 * time.Second << uint(shift)
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// RunSync reconciles the map every syncInterval until ctx is cancelled.
func RunSync(ctx context.Context, pool *pgxpool.Pool, prov TicketProvider, teamID string, interval time.Duration) error {
	if interval <= 0 {
		interval = syncInterval
	}
	log.Printf("pipeline: sync reconciling every %s (team %s)", interval, teamID)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n, err := ReconcileOnce(ctx, pool, prov, teamID); err != nil {
			log.Printf("pipeline: sync tick: %v", err)
		} else if n > 0 {
			log.Printf("pipeline: synced %d transition(s)", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ReconcileOnce is one mirror tick: fill derived pending states where
// nothing unsynced is explicit, then push every due transition. Returns the
// number of successful pushes; infra failures (reading rows) return an
// error, per-row push failures are bookkept with backoff and logged.
func ReconcileOnce(ctx context.Context, pool *pgxpool.Pool, prov TicketProvider, teamID string) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT m.issue_id, COALESCE(m.pending_state, ''), COALESCE(m.last_synced_state, ''),
		       m.attempts, m.next_attempt_at, t.status
		FROM   ticket_issue_map m
		JOIN   tasks t ON t.id = m.task_id`)
	if err != nil {
		return 0, fmt.Errorf("pipeline: sync select: %w", err)
	}
	defer rows.Close()

	type pushJob struct {
		issueID  string
		col      Column
		attempts int // failures seen before this push attempt
	}
	var (
		derivedUpdates [][2]string // issueID, derived canonical name → pending_state
		pushes         []pushJob
	)
	for rows.Next() {
		var (
			issueID, pend, last, status string
			attempts                    int
			nextAttempt                 time.Time
		)
		if err := rows.Scan(&issueID, &pend, &last, &attempts, &nextAttempt, &status); err != nil {
			return 0, fmt.Errorf("pipeline: sync scan: %w", err)
		}
		var wantCol Column
		if pend == "" || pend == last {
			// Nothing unsynced is pending: the derived value may fill in.
			d, ok := DerivedState(status)
			if !ok || d.String() == last {
				continue // unmapped status, or already mirrored
			}
			wantCol = d
			derivedUpdates = append(derivedUpdates, [2]string{issueID, d.String()})
		} else {
			// Explicit pending wins. A stored value that is not a canonical
			// column name cannot be pushed — leave the row untouched.
			c, ok := columnFromName(pend)
			if !ok {
				continue
			}
			wantCol = c
		}
		if wantCol.String() == last || time.Now().Before(nextAttempt) {
			continue
		}
		pushes = append(pushes, pushJob{issueID: issueID, col: wantCol, attempts: attempts})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pipeline: sync rows: %w", err)
	}
	rows.Close()

	for _, u := range derivedUpdates {
		if _, err := pool.Exec(ctx, `
			UPDATE ticket_issue_map SET pending_state = $2, updated_at = NOW()
			WHERE issue_id = $1`, u[0], u[1]); err != nil {
			return 0, fmt.Errorf("pipeline: sync derived update: %w", err)
		}
	}
	if len(pushes) == 0 {
		return 0, nil
	}

	cols, err := prov.Columns(ctx, teamID)
	if err != nil {
		return 0, fmt.Errorf("pipeline: sync provider columns: %w", err)
	}

	pushed := 0
	for _, p := range pushes {
		colID, ok := cols[p.col]
		if !ok {
			// The board lost this canonical column (renamed/removed); treat
			// as a failure so the row backs off instead of hot-looping.
			log.Printf("pipeline: sync %s: column %q not mapped by provider", p.issueID, p.col.String())
			colID = ""
		} else if err := prov.SetIssueColumn(ctx, p.issueID, colID); err != nil {
			log.Printf("pipeline: sync %s: push %q: %v", p.issueID, p.col.String(), err)
			colID = ""
		}
		if colID == "" {
			if _, err := pool.Exec(ctx, `
				UPDATE ticket_issue_map
				SET    attempts = attempts + 1,
				       next_attempt_at = NOW() + make_interval(secs => $2)
				WHERE  issue_id = $1`, p.issueID, backoffDelay(p.attempts+1).Seconds()); err != nil {
				return pushed, fmt.Errorf("pipeline: sync backoff update: %w", err)
			}
			continue
		}
		if _, err := pool.Exec(ctx, `
			UPDATE ticket_issue_map
			SET    last_synced_state = $2, attempts = 0, next_attempt_at = NOW(),
			       synced_at = NOW(), updated_at = NOW()
			WHERE  issue_id = $1`, p.issueID, p.col.String()); err != nil {
			return pushed, fmt.Errorf("pipeline: sync success update: %w", err)
		}
		pushed++
	}
	return pushed, nil
}
