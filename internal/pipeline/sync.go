package pipeline

// linearSync (ADR-0005): the mirror half of the bridge. Every tick derives
// the target Linear column from tasks.status and pushes pending transitions
// with exponential backoff. The bookkeeping is diff-based — pending_state is
// the desired column, last_synced_state the last one successfully pushed —
// so a missed tick or a Linear outage self-heals without losing transitions.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const syncInterval = 10 * time.Second

// DerivedState maps tasks.status → the MAQ Linear column it mirrors.
// Unknown statuses return "" (skip). Future pipeline code (review loop,
// EX-03+) writes explicit pending_state values — e.g. "Changes Requested" —
// which take precedence over the derived value until synced.
func DerivedState(status string) string {
	switch status {
	case "ready", "claimed":
		return "In Progress"
	case "review":
		return "In Review"
	case "pending_approval", "failed":
		return "Needs Human"
	case "done":
		return "Done"
	default:
		return ""
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
func RunSync(ctx context.Context, pool *pgxpool.Pool, client LinearAPI, teamID string, interval time.Duration) error {
	if interval <= 0 {
		interval = syncInterval
	}
	log.Printf("pipeline: linearSync reconciling every %s (team %s)", interval, teamID)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n, err := ReconcileOnce(ctx, pool, client, teamID); err != nil {
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
func ReconcileOnce(ctx context.Context, pool *pgxpool.Pool, client LinearAPI, teamID string) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT m.linear_issue_id, COALESCE(m.pending_state, ''), COALESCE(m.last_synced_state, ''),
		       m.attempts, m.next_attempt_at, t.status
		FROM   linear_issue_map m
		JOIN   tasks t ON t.id = m.task_id`)
	if err != nil {
		return 0, fmt.Errorf("pipeline: sync select: %w", err)
	}
	defer rows.Close()

	type pushJob struct {
		issueID, state string
		attempts       int // failures seen before this push attempt
	}
	var (
		derivedUpdates [][2]string // issueID, derived state to write into pending_state
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
		want := pend
		if pend == "" || pend == last {
			// Nothing unsynced is pending: the derived value may fill in.
			d := DerivedState(status)
			if d == "" || d == last {
				continue // unmapped status, or already mirrored
			}
			want = d
			derivedUpdates = append(derivedUpdates, [2]string{issueID, d})
		}
		if want == last || time.Now().Before(nextAttempt) {
			continue
		}
		pushes = append(pushes, pushJob{issueID: issueID, state: want, attempts: attempts})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pipeline: sync rows: %w", err)
	}
	rows.Close()

	for _, u := range derivedUpdates {
		if _, err := pool.Exec(ctx, `
			UPDATE linear_issue_map SET pending_state = $2, updated_at = NOW()
			WHERE linear_issue_id = $1`, u[0], u[1]); err != nil {
			return 0, fmt.Errorf("pipeline: sync derived update: %w", err)
		}
	}
	if len(pushes) == 0 {
		return 0, nil
	}

	states, err := client.WorkflowStates(ctx, teamID)
	if err != nil {
		return 0, fmt.Errorf("pipeline: sync workflow states: %w", err)
	}

	pushed := 0
	for _, p := range pushes {
		stateID, ok := states[p.state]
		if !ok {
			// Operator renamed/removed the column; treat as a failure so the
			// row backs off instead of hot-looping.
			log.Printf("pipeline: sync %s: state %q not found on team", p.issueID, p.state)
			stateID = ""
		} else if _, err := client.UpdateIssueState(ctx, p.issueID, stateID); err != nil {
			log.Printf("pipeline: sync %s: push %q: %v", p.issueID, p.state, err)
			stateID = ""
		}
		if stateID == "" {
			if _, err := pool.Exec(ctx, `
				UPDATE linear_issue_map
				SET    attempts = attempts + 1,
				       next_attempt_at = NOW() + make_interval(secs => $2)
				WHERE  linear_issue_id = $1`, p.issueID, backoffDelay(p.attempts+1).Seconds()); err != nil {
				return pushed, fmt.Errorf("pipeline: sync backoff update: %w", err)
			}
			continue
		}
		if _, err := pool.Exec(ctx, `
			UPDATE linear_issue_map
			SET    last_synced_state = $2, attempts = 0, next_attempt_at = NOW(),
			       synced_at = NOW(), updated_at = NOW()
			WHERE  linear_issue_id = $1`, p.issueID, p.state); err != nil {
			return pushed, fmt.Errorf("pipeline: sync success update: %w", err)
		}
		pushed++
	}
	return pushed, nil
}
