package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// findBin returns an absolute path for a common shell tool so tests
// are portable across /bin vs /usr/bin installs.
func findBin(t *testing.T, name string) string {
	t.Helper()
	for _, prefix := range []string{"/bin/", "/usr/bin/", "/usr/local/bin/"} {
		p := prefix + name
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skipf("%s not found on PATH", name)
	return ""
}

func TestSupervisor_CleanShutdown_ViaStop(t *testing.T) {
	sleep := findBin(t, "sleep")
	s := New(Config{
		Bin:         sleep,
		Args:        []string{"30"},
		MaxRestarts: 3,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool {
		running, _ := s.Status()
		return running
	}, "child never became alive")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run after Stop returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop")
	}

	if running, _ := s.Status(); running {
		t.Fatal("Status reports running after Stop")
	}
}

func TestSupervisor_CleanShutdown_ViaCtxCancel(t *testing.T) {
	sleep := findBin(t, "sleep")
	s := New(Config{
		Bin:         sleep,
		Args:        []string{"30"},
		MaxRestarts: 3,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool {
		running, _ := s.Status()
		return running
	}, "child never became alive")

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run after ctx cancel = %v; want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestSupervisor_RestartOnUnexpectedExit(t *testing.T) {
	sh := findBin(t, "sh")

	var exitCount atomic.Int32
	s := New(Config{
		Bin:            sh,
		Args:           []string{"-c", "exit 2"},
		MaxRestarts:    5,
		RestartWindow:  60 * time.Second,
		RestartBackoff: 10 * time.Millisecond,
		OnChildExit: func(err error) {
			exitCount.Add(1)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()

	// Let the supervisor hit the restart budget (5 restarts = 6
	// total spawns).
	select {
	case err := <-runDone:
		if err == nil {
			t.Fatal("Run returned nil; want restart-budget-exhausted error")
		}
		if !strings.Contains(err.Error(), "restart budget exhausted") {
			t.Fatalf("Run error = %v; want restart budget exhausted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after hitting restart budget")
	}

	got := exitCount.Load()
	// Exit fires once per spawn. MaxRestarts=5 means after 5
	// restarts we bail; combined with the initial spawn that's 6.
	if got < 5 || got > 7 {
		t.Fatalf("OnChildExit fired %d times; want 5-7", got)
	}

	if s.Restarts() < 5 {
		t.Fatalf("Restarts() = %d; want ≥5", s.Restarts())
	}
}

func TestSupervisor_NoRestartWhenMaxRestartsZero(t *testing.T) {
	sh := findBin(t, "sh")

	s := New(Config{
		Bin:         sh,
		Args:        []string{"-c", "exit 3"},
		MaxRestarts: 0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.Run(ctx)
	if err == nil {
		t.Fatal("Run with MaxRestarts=0 returned nil; want child-exit error")
	}
	if !strings.Contains(err.Error(), "child exited") {
		t.Fatalf("Run error = %v; want 'child exited'", err)
	}
}

func TestSupervisor_SpawnErrorBubblesUp(t *testing.T) {
	s := New(Config{
		Bin:         "/no/such/binary/for/real",
		Args:        nil,
		MaxRestarts: 3,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.Run(ctx)
	if err == nil {
		t.Fatal("Run for nonexistent bin returned nil; want spawn error")
	}
	if !strings.Contains(err.Error(), "spawn failed") {
		t.Fatalf("Run error = %v; want 'spawn failed'", err)
	}
}

func TestSupervisor_LogPiping(t *testing.T) {
	sh := findBin(t, "sh")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "child.log")

	s := New(Config{
		Bin:     sh,
		Args:    []string{"-c", "echo hello-stdout; echo hello-stderr 1>&2"},
		LogPath: logPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil; want 'child exited' since MaxRestarts=0 and child exits cleanly")
	}
	// The child exited cleanly (status 0) but MaxRestarts=0 means we
	// still treat "child gone" as terminal. That's fine — we just
	// need the log.

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if !strings.Contains(string(data), "hello-stdout") {
		t.Fatalf("log missing stdout line; got %q", data)
	}
	if !strings.Contains(string(data), "hello-stderr") {
		t.Fatalf("log missing stderr line; got %q", data)
	}
}

func TestSupervisor_StatusBeforeStart(t *testing.T) {
	s := New(Config{Bin: "/bin/true"})
	running, pid := s.Status()
	if running || pid != 0 {
		t.Fatalf("Status before Run = (%v, %d); want (false, 0)", running, pid)
	}
}

func TestSupervisor_SecondRunIsRejected(t *testing.T) {
	s := New(Config{Bin: "/bin/true", MaxRestarts: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First Run exits with 'child exited' error since /bin/true
	// completes immediately.
	_ = s.Run(ctx)

	// Second Run should be a no-op error.
	err := s.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("second Run = %v; want already-completed error", err)
	}
}

func TestSupervisor_StopEscalatesToSIGKILL(t *testing.T) {
	// Spawn a process that ignores SIGTERM; Stop must escalate to
	// SIGKILL within the 10 s grace window.
	sh := findBin(t, "sh")
	s := New(Config{
		Bin: sh,
		// Trap SIGTERM (15) to do nothing, then sleep.
		Args:        []string{"-c", "trap '' TERM; sleep 120"},
		MaxRestarts: 1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool {
		running, _ := s.Status()
		return running
	}, "child never became alive")

	// Stop must complete within ~12 s: 10 s SIGTERM grace + 2 s
	// SIGKILL cushion. We give the test harness 15 s.
	stopStart := time.Now()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(stopStart)
	if elapsed < 10*time.Second {
		t.Fatalf("Stop returned in %s; expected ≥10 s (SIGTERM ignored must require SIGKILL escalation)", elapsed)
	}

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop escalation")
	}
}

func TestSupervisor_EnvPropagatesToChild(t *testing.T) {
	sh := findBin(t, "sh")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "env.log")

	s := New(Config{
		Bin:     sh,
		Args:    []string{"-c", "echo FOO=$MAQUINISTA_SUPERVISOR_TEST_FOO"},
		Env:     []string{"MAQUINISTA_SUPERVISOR_TEST_FOO=bar-baz"},
		LogPath: logPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.Run(ctx)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "FOO=bar-baz") {
		t.Fatalf("env did not propagate to child; got %q", data)
	}
}

func TestSupervisor_ConcurrentStatusIsSafe(t *testing.T) {
	// Race-detector smoke test: hammer Status() from multiple
	// goroutines while Run is active. Pass if -race stays quiet.
	sleep := findBin(t, "sleep")
	s := New(Config{
		Bin:  sleep,
		Args: []string{"5"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Run(ctx)
	}()

	waitFor(t, 2*time.Second, func() bool {
		running, _ := s.Status()
		return running
	}, "child never became alive")

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = s.Status()
				_ = s.Restarts()
			}
		}()
	}

	// Let the hammer goroutines finish, then stop.
	time.Sleep(50 * time.Millisecond)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = s.Stop(stopCtx)
	wg.Wait()
}

// --- helpers -----------------------------------------------------------------

func waitFor(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor: %s (after %s)", msg, timeout)
}

