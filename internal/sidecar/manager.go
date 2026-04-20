package sidecar

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// noopTailer blocks until ctx is cancelled. Phase 1 uses this because the
// monitor still owns transcript observation; Phase 2 replaces it with a
// real per-agent tailer that moves transcript tailing into the sidecar.
type noopTailer struct{}

func (noopTailer) Tail(ctx context.Context, ch chan<- TranscriptEvent) error {
	defer close(ch)
	<-ctx.Done()
	return ctx.Err()
}

// isTmuxWindowMissing returns true when tmux can't find the target window
// (killed, renamed, or session recreated).
func isTmuxWindowMissing(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "can't find window") ||
		strings.Contains(s, "no such window") ||
		strings.Contains(s, "no current window")
}

// managedSidecar tracks a running SidecarRunner goroutine.
type managedSidecar struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// Manager supervises one SidecarRunner per live agent. It starts sidecars
// for all live agents at boot and allows dynamic Spawn/Teardown as the
// fleet changes. Sync can be called periodically to pick up agents that
// came online after Boot.
type Manager struct {
	pool     *pgxpool.Pool
	session  string // tmux session name (e.g. "maquinista")
	onClaim  func(agentID, inboxID string) // forwarded to each sidecar's Config.OnClaim

	mu        sync.Mutex
	running   map[string]*managedSidecar
	parentCtx context.Context
}

// NewManager creates a Manager. onClaim is called by each sidecar just
// before it drives an inbox row into the PTY; the monitor's OutboxSink
// reads this mapping to stamp in_reply_to on outbox rows. May be nil.
func NewManager(pool *pgxpool.Pool, tmuxSession string, onClaim func(agentID, inboxID string)) *Manager {
	return &Manager{
		pool:    pool,
		session: tmuxSession,
		onClaim: onClaim,
		running: make(map[string]*managedSidecar),
	}
}

// Boot calls Sync once after storing the parent context. Call this at
// daemon startup after reconcileAgentPanes has brought agents online.
func (m *Manager) Boot(ctx context.Context) (int, error) {
	m.mu.Lock()
	m.parentCtx = ctx
	m.mu.Unlock()
	return m.Sync(ctx)
}

// Sync queries all live agents and starts a sidecar for any that don't
// already have one. Safe to call repeatedly; already-running sidecars are
// untouched. Returns the number of new sidecars started.
func (m *Manager) Sync(ctx context.Context) (int, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id FROM agents
		WHERE status IN ('running','idle','working')
		  AND tmux_window <> ''
		  AND stop_requested = FALSE
	`)
	if err != nil {
		return 0, fmt.Errorf("sidecar manager sync: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	started := 0
	for _, id := range ids {
		if _, exists := m.running[id]; !exists {
			m.spawnLocked(id)
			started++
		}
	}
	if started > 0 {
		log.Printf("sidecar manager: started %d new sidecar(s) (%d total)",
			started, len(m.running))
	}
	return started, nil
}

// Spawn starts a sidecar for agentID immediately without querying the DB.
// Use this when you know an agent just came online. No-op if already running.
func (m *Manager) Spawn(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.running[agentID]; exists {
		return
	}
	m.spawnLocked(agentID)
}

// Teardown stops the sidecar for agentID and blocks until it exits.
// No-op if no sidecar is running for agentID.
func (m *Manager) Teardown(agentID string) {
	m.mu.Lock()
	ms, ok := m.running[agentID]
	m.mu.Unlock()
	if !ok {
		return
	}
	ms.cancel()
	<-ms.done
	log.Printf("sidecar manager: tore down sidecar for agent %s", agentID)
}

// ActiveCount returns the number of currently running sidecars.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.running)
}

func (m *Manager) spawnLocked(agentID string) {
	ctx, cancel := context.WithCancel(m.parentCtx)
	done := make(chan struct{})

	cfg := DefaultConfig(agentID)
	cfg.OnClaim = m.onClaim

	driver := m.makeTmuxDriver(agentID)
	s := New(m.pool, cfg, driver, noopTailer{})

	go func() {
		defer close(done)
		defer func() {
			m.mu.Lock()
			delete(m.running, agentID)
			m.mu.Unlock()
		}()
		if err := s.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("sidecar %s: exited unexpectedly: %v", agentID, err)
		}
	}()

	m.running[agentID] = &managedSidecar{cancel: cancel, done: done}
	log.Printf("sidecar manager: spawned sidecar for agent %s", agentID)
}

// makeTmuxDriver returns a PtyDriver that resolves the agent's current
// tmux_window at drive time and forwards text via SendKeysWithDelay.
// If the window has vanished the agent is marked stopped in the DB.
func (m *Manager) makeTmuxDriver(agentID string) PtyDriver {
	pool := m.pool
	session := m.session
	return PtyDriverFunc(func(ctx context.Context, text string) error {
		var tmuxWindow string
		if err := pool.QueryRow(ctx,
			`SELECT tmux_window FROM agents WHERE id=$1`, agentID,
		).Scan(&tmuxWindow); err != nil {
			return fmt.Errorf("sidecar %s: lookup tmux_window: %w", agentID, err)
		}
		if tmuxWindow == "" {
			return fmt.Errorf("sidecar %s: no tmux_window set", agentID)
		}
		driveErr := tmux.SendKeysWithDelay(session, tmuxWindow, text, 500)
		if driveErr != nil && isTmuxWindowMissing(driveErr) {
			if _, err := pool.Exec(ctx,
				`UPDATE agents SET status='stopped', stop_requested=TRUE,
				 last_seen=NOW() WHERE id=$1`, agentID); err != nil {
				log.Printf("sidecar %s: mark-stopped: %v", agentID, err)
			} else {
				log.Printf("sidecar %s: window %s vanished — marked stopped",
					agentID, tmuxWindow)
			}
		}
		return driveErr
	})
}
