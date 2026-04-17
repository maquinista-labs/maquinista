// Package dashboard supervises the Next.js child process that serves
// the dashboard UI. See plans/active/dashboard.md.
//
// Phase 0 Commit 0.2 — Supervisor type with bounded-backoff restart,
// SIGTERM/SIGKILL stop cascade, and append-only log pipe. Commit 0.3
// wires this into cmd/maquinista/cmd_dashboard.go; Commit 1.6 swaps
// the child from a Node healthcheck stub to the extracted Next.js
// standalone server.
package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config configures a Supervisor. Fields are read at Start time; do
// not mutate after calling Start.
type Config struct {
	// Bin is the absolute path (or PATH-resolvable name) of the
	// executable to spawn.
	Bin string
	// Args is the argv slice passed to Bin (excluding argv[0]).
	Args []string
	// Env is a list of KEY=VALUE strings appended to the parent's
	// environment. Duplicate keys in Env win over inherited ones
	// (matches exec.Cmd semantics).
	Env []string
	// WorkDir is the working directory for the child. Defaults to
	// the parent's if empty.
	WorkDir string
	// LogPath receives appended stdout+stderr from the child. If
	// empty, output is discarded. The file is opened with O_APPEND
	// so multiple runs concatenate rather than overwrite.
	LogPath string
	// MaxRestarts bounds the number of restarts allowed inside
	// RestartWindow. Zero means the child is never restarted (one
	// crash stops the supervisor).
	MaxRestarts int
	// RestartWindow is the sliding window over which MaxRestarts
	// applies. Defaults to 60 s.
	RestartWindow time.Duration
	// RestartBackoff is the initial delay before a restart. Each
	// successive crash doubles the delay, capped at 30 s.
	// Defaults to 500 ms.
	RestartBackoff time.Duration
	// OnChildExit is invoked (if non-nil) after each child exit
	// with the exit error (nil for clean exits). Useful for tests.
	OnChildExit func(err error)
}

// Supervisor manages the lifecycle of a child process.
//
// Typical usage:
//
//	sup := dashboard.New(cfg)
//	go sup.Run(ctx) // blocks until ctx.Done() or restart budget exhausted
//	// ... elsewhere:
//	sup.Stop(ctx)
type Supervisor struct {
	cfg Config

	mu       sync.Mutex
	cmd      *exec.Cmd
	logFile  *os.File
	restarts []time.Time
	stopping atomic.Bool
	pid      atomic.Int64
	runErr   error
	doneCh   chan struct{}
}

// New constructs a Supervisor with defaults filled in.
func New(cfg Config) *Supervisor {
	if cfg.RestartWindow == 0 {
		cfg.RestartWindow = 60 * time.Second
	}
	if cfg.RestartBackoff == 0 {
		cfg.RestartBackoff = 500 * time.Millisecond
	}
	return &Supervisor{cfg: cfg, doneCh: make(chan struct{})}
}

// Run spawns the child and supervises it until ctx is cancelled, Stop
// is called, or the restart budget is exhausted. Returns nil on
// orderly shutdown; returns an error if the child failed to spawn or
// crashed past the restart budget.
//
// Run must be called at most once per Supervisor. Calling Run a
// second time returns an error.
func (s *Supervisor) Run(ctx context.Context) error {
	select {
	case <-s.doneCh:
		return errors.New("supervisor: Run already completed")
	default:
	}
	defer close(s.doneCh)

	var backoff = s.cfg.RestartBackoff
	attempt := 0

	for {
		if s.stopping.Load() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			s.killChild()
			return nil
		}

		// Budget check: prune restart timestamps older than the
		// window, then reject if we've hit the cap. Attempt 0 is
		// always free (the initial spawn is not a restart).
		s.pruneRestarts()
		if attempt > 0 && s.cfg.MaxRestarts > 0 && len(s.restarts) >= s.cfg.MaxRestarts {
			return fmt.Errorf("supervisor: restart budget exhausted (%d restarts in %s)", s.cfg.MaxRestarts, s.cfg.RestartWindow)
		}

		if err := s.spawnChild(); err != nil {
			if s.cfg.OnChildExit != nil {
				s.cfg.OnChildExit(err)
			}
			return fmt.Errorf("supervisor: spawn failed: %w", err)
		}

		waitErr := s.waitChild(ctx)
		if s.cfg.OnChildExit != nil {
			s.cfg.OnChildExit(waitErr)
		}

		if s.stopping.Load() || ctx.Err() != nil {
			// Stop / cancel path — the wait error is either nil
			// (clean SIGTERM) or a "signal: killed"-ish thing we
			// don't want to bubble up as supervisor failure.
			return nil
		}

		// Unexpected exit → count, backoff, restart.
		s.restarts = append(s.restarts, time.Now())
		if s.cfg.MaxRestarts == 0 {
			return fmt.Errorf("supervisor: child exited: %w", waitErr)
		}
		attempt++

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// Stop signals the child with SIGTERM, waits up to 10 s for a clean
// exit, then SIGKILLs. Returns nil if there is no running child.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.stopping.Store(true)

	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// ESRCH means the process already exited; treat as success.
		if !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("supervisor: SIGTERM: %w", err)
		}
	}

	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if s.pid.Load() == 0 {
			return nil
		}
		select {
		case <-deadline:
			_ = cmd.Process.Signal(syscall.SIGKILL)
			// One more wait-cycle for the kernel to reap via our Wait.
			waitCycle := time.After(2 * time.Second)
			for s.pid.Load() != 0 {
				select {
				case <-waitCycle:
					return nil
				case <-ticker.C:
				}
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Status reports whether a child is currently running and its PID.
// Returns (false, 0) when the supervisor is between restarts or
// after Stop.
func (s *Supervisor) Status() (running bool, pid int) {
	p := int(s.pid.Load())
	return p != 0, p
}

// Restarts returns how many restarts have occurred (for tests /
// metrics). The count does not decay with the sliding window; callers
// wanting "recent restarts" should use their own time math.
func (s *Supervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.restarts)
}

// --- internal ---------------------------------------------------------------

func (s *Supervisor) spawnChild() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Important: when cmd.Stdout/Stderr is an io.Writer that is NOT
	// an *os.File, exec.Cmd creates a pipe and spawns a copy
	// goroutine; Wait() won't return until the pipe's write end is
	// closed. If the child spawns a grandchild that inherits those
	// fds and outlives its parent (e.g. `sh -c "trap '' TERM; sleep
	// 120"` after SIGKILL), the pipe stays open forever and Wait
	// blocks indefinitely. Attaching *os.File (the log file or
	// /dev/null) gives Go the fd directly with no copy goroutine.
	var logOut *os.File
	if s.cfg.LogPath != "" {
		f, err := os.OpenFile(s.cfg.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open log %s: %w", s.cfg.LogPath, err)
		}
		if s.logFile != nil {
			_ = s.logFile.Close()
		}
		s.logFile = f
		logOut = f
	} else {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", os.DevNull, err)
		}
		s.logFile = f
		logOut = f
	}

	cmd := exec.Command(s.cfg.Bin, s.cfg.Args...)
	cmd.Env = append(os.Environ(), s.cfg.Env...)
	cmd.Dir = s.cfg.WorkDir
	cmd.Stdout = logOut
	cmd.Stderr = logOut

	if err := cmd.Start(); err != nil {
		if s.logFile != nil {
			_ = s.logFile.Close()
			s.logFile = nil
		}
		return err
	}

	s.cmd = cmd
	s.pid.Store(int64(cmd.Process.Pid))
	return nil
}

// waitChild blocks until the current child exits. Returns the exit
// error (nil for clean). Honors ctx cancellation by sending SIGTERM
// to the child; the subsequent Wait still returns.
func (s *Supervisor) waitChild(ctx context.Context) error {
	s.mu.Lock()
	cmd := s.cmd
	logFile := s.logFile
	s.mu.Unlock()

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-doneCh:
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		// Give the child 10 s to exit on its own terms.
		select {
		case waitErr = <-doneCh:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGKILL)
			waitErr = <-doneCh
		}
	}

	s.mu.Lock()
	s.pid.Store(0)
	s.cmd = nil
	if logFile != nil {
		_ = logFile.Close()
		s.logFile = nil
	}
	s.mu.Unlock()

	return waitErr
}

func (s *Supervisor) killChild() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-done
	}
}

func (s *Supervisor) pruneRestarts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-s.cfg.RestartWindow)
	keep := s.restarts[:0]
	for _, t := range s.restarts {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	s.restarts = keep
}
