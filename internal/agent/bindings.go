package agent

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// Window bindings are the join between the agents table and tmux panes.
// Every maquinista pane is created with -n <agentID>, and while the pane
// lives, its agents row carries the window reference (id @N or, for the
// orchestrator-spawned user agents, the name itself). The invariant this
// file protects (MAQ-38): a row may only hold a binding while its pane
// actually exists.
//
// The invariant breaks on a crash. tmux window ids restart with the tmux
// server, so after a crash/reboot the NEW panes get the SAME @N values
// the pre-crash rows still claim — and every window-scoped consumer
// (monitor resolution, outbox attribution, transcript-liveness touches,
// freeze-arm pane kills) then hits a collision where a dead row and a
// live row both match. Observed 2026-10-07: outbox rows for new rounds
// landed under stale pre-crash agent ids, transcript liveness fed a dead
// row from the previous day, and the freshness watchdog false-froze every
// round to the respawn cap. `maquinista stop` is safe (KillAll clears
// task rows and parks user rows with tmux_window=''), a crash is not.
//
// SweepStaleWindowBindings runs once per boot, before panes respawn and
// before the scheduler/monitor start: any row whose binding does not
// resolve to a live pane — or resolves to a pane NAMED FOR ANOTHER AGENT
// (the collision signature) — gets tmux_window cleared. Statuses are
// untouched: the state machine owns them (user rows are respawned by
// reconcileAgentPanes, task rows re-dispatch via the freeze arms/reaper).
// Live-row identity mismatches are returned so the caller can fail loud
// (Pipeline-topic diagnosis) instead of silently starving the watchdog.

// BindingSweepResult summarizes one SweepStaleWindowBindings pass.
type BindingSweepResult struct {
	// Cleared is how many rows lost a dead binding (pane gone).
	Cleared int
	// Mismatched is how many LIVE rows pointed at a pane named for a
	// different agent — the window-id-reuse collision signature.
	Mismatched int
	// MismatchRows names the live rows involved, for the loud diagnosis.
	MismatchRows []string
}

// WindowLister lists the windows of a tmux session. Production passes
// tmux.ListWindows; tests inject a fake.
type WindowLister func(session string) ([]tmux.Window, error)

// SweepStaleWindowBindings clears every agents.tmux_window binding that
// does not resolve to a live pane named for the row's own id. See the
// package-comment above for the failure this closes. sessionName is only
// a label here (list is injected); pass cfg.TmuxSessionName.
func SweepStaleWindowBindings(ctx context.Context, pool *pgxpool.Pool, sessionName string, list WindowLister) (BindingSweepResult, error) {
	var res BindingSweepResult
	if pool == nil {
		return res, nil
	}

	rows, err := pool.Query(ctx, `
		SELECT id, status, tmux_window
		FROM agents
		WHERE tmux_window <> ''
		  AND role <> 'notifier' -- migration 036's synthetic pipeline agent carries placeholder session/window values that every daemon ignores; the sweep does too
	`)
	if err != nil {
		return res, fmt.Errorf("list bound agents: %w", err)
	}
	type boundRow struct{ id, status, window string }
	var rowsByID []boundRow
	for rows.Next() {
		var r boundRow
		if err := rows.Scan(&r.id, &r.status, &r.window); err != nil {
			rows.Close()
			return res, err
		}
		rowsByID = append(rowsByID, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	if len(rowsByID) == 0 {
		return res, nil
	}

	windows, err := list(sessionName)
	if err != nil {
		// No session at all is the normal post-restart shape: every
		// binding is stale, and the sweep must still run (an error here
		// would leave the collisions in place). Empty-set semantics.
		if !tmux.SessionExists(sessionName) {
			windows = nil
		} else {
			return res, fmt.Errorf("list tmux windows: %w", err)
		}
	}
	byID := make(map[string]tmux.Window, len(windows))
	byName := make(map[string]tmux.Window, len(windows))
	for _, w := range windows {
		byID[w.ID] = w
		byName[w.Name] = w
	}

	// paneFor returns the window a row's binding resolves to, or ok=false
	// when no live pane carries it (by id or by name — rows hold both
	// forms; the orchestrator-spawned user rows hold the name itself).
	paneFor := func(binding string) (tmux.Window, bool) {
		if w, ok := byID[binding]; ok {
			return w, true
		}
		w, ok := byName[binding]
		return w, ok
	}

	live := map[string]bool{"running": true, "working": true, "idle": true, "spawning": true}

	for _, r := range rowsByID {
		w, ok := paneFor(r.window)
		if !ok {
			// Pane gone (crash death or graceful kill): the binding is
			// dead weight that can only collide with a future pane.
			if _, err := pool.Exec(ctx, `UPDATE agents SET tmux_window='' WHERE id=$1`, r.id); err != nil {
				return res, fmt.Errorf("clear stale binding for %s: %w", r.id, err)
			}
			res.Cleared++
			log.Printf("reconcile: cleared stale window binding %s -> %q (no such pane)", r.id, r.window)
			continue
		}
		if w.Name == r.id {
			continue // healthy binding: the pane is this agent's
		}
		// The pane exists but is named for another agent — the row's
		// window id was reused after a tmux restart. Clear unconditionally
		// (a dead row must never shadow a live pane), and report loudly
		// when a LIVE row was involved: its monitor attribution, outbox
		// rows and watchdog freshness were feeding the wrong pane.
		if _, err := pool.Exec(ctx, `UPDATE agents SET tmux_window='' WHERE id=$1`, r.id); err != nil {
			return res, fmt.Errorf("clear collided binding for %s: %w", r.id, err)
		}
		res.Cleared++
		if live[r.status] {
			res.Mismatched++
			res.MismatchRows = append(res.MismatchRows,
				fmt.Sprintf("%s (status %s) claimed %s but the pane is %q", r.id, r.status, r.window, w.Name))
			log.Printf("reconcile: WINDOW ID COLLISION: %s (status %s) claimed %s but the pane is named %q — binding cleared",
				r.id, r.status, r.window, w.Name)
		} else {
			log.Printf("reconcile: cleared dead-row binding %s -> %q (pane %q belongs to another agent)", r.id, r.window, w.Name)
		}
	}
	sort.Strings(res.MismatchRows)
	return res, nil
}
