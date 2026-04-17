package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// withDashboardTempDir sets dashboardDir to a fresh t.TempDir() for
// the duration of the test. Used by every dashboard-subcommand test
// so they don't trample the user's real ~/.maquinista.
func withDashboardTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := dashboardDir
	SetDashboardDir(dir)
	t.Cleanup(func() { SetDashboardDir(prev) })
	return dir
}

func TestDashboardPIDFile_WriteReadRoundtrip(t *testing.T) {
	dir := withDashboardTempDir(t)

	if got, err := readDashboardPIDFile(); err != nil || got != 0 {
		t.Fatalf("readDashboardPIDFile() on empty dir = %d, %v; want 0, nil", got, err)
	}

	if err := writeDashboardPIDFile(4242); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}

	got, err := readDashboardPIDFile()
	if err != nil {
		t.Fatalf("readDashboardPIDFile: %v", err)
	}
	if got != 4242 {
		t.Fatalf("readDashboardPIDFile = %d; want 4242", got)
	}

	// Verify the file lives under the dashboardDir override.
	want := filepath.Join(dir, "dashboard.pid")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("PID file at %s: %v", want, err)
	}
}

func TestDashboardPIDFile_RemoveIsIdempotent(t *testing.T) {
	withDashboardTempDir(t)

	// remove-before-exist must not error.
	removeDashboardPIDFile()

	if err := writeDashboardPIDFile(7); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}
	removeDashboardPIDFile()

	got, err := readDashboardPIDFile()
	if err != nil {
		t.Fatalf("readDashboardPIDFile after remove: %v", err)
	}
	if got != 0 {
		t.Fatalf("readDashboardPIDFile after remove = %d; want 0", got)
	}
}

func TestDashboardPIDFile_MalformedErrors(t *testing.T) {
	dir := withDashboardTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "dashboard.pid"), []byte("not-a-number"), 0o644); err != nil {
		t.Fatalf("seeding malformed PID: %v", err)
	}
	_, err := readDashboardPIDFile()
	if err == nil || !strings.Contains(err.Error(), "invalid PID file") {
		t.Fatalf("readDashboardPIDFile on malformed = %v; want invalid PID error", err)
	}
}

func TestDashboardProcessAlive_DeadPID(t *testing.T) {
	// A PID of 1 is the init process and will never match a user-
	// spawned maquinista dashboard; using INT_MAX as a "known dead"
	// PID is more reliable. We pick a high value unlikely to exist.
	if dashboardProcessAlive(0) {
		t.Fatal("dashboardProcessAlive(0) = true; want false")
	}
	if dashboardProcessAlive(-1) {
		t.Fatal("dashboardProcessAlive(-1) = true; want false")
	}
	// MaxInt32-ish — vanishingly unlikely to exist on a dev box.
	if dashboardProcessAlive(2_000_000_000) {
		t.Skip("process 2B exists; skip (CI oddity)")
	}
}

func TestDashboardProcessAlive_SelfAlive(t *testing.T) {
	if !dashboardProcessAlive(os.Getpid()) {
		t.Fatal("dashboardProcessAlive(self) = false; want true")
	}
}

func TestRunDashboardStatus_NotRunning(t *testing.T) {
	withDashboardTempDir(t)
	running, pid, err := runDashboardStatus()
	if err != nil {
		t.Fatalf("runDashboardStatus: %v", err)
	}
	if running || pid != 0 {
		t.Fatalf("runDashboardStatus = (%v, %d); want (false, 0)", running, pid)
	}
}

func TestRunDashboardStatus_StalePIDReportsNotRunning(t *testing.T) {
	withDashboardTempDir(t)
	// A dead PID on disk must not look "running".
	if err := writeDashboardPIDFile(2_000_000_000); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}
	running, pid, err := runDashboardStatus()
	if err != nil {
		t.Fatalf("runDashboardStatus: %v", err)
	}
	if running || pid != 0 {
		t.Fatalf("runDashboardStatus = (%v, %d); want (false, 0) for stale PID", running, pid)
	}
}

func TestRunDashboardStatus_LiveSelf(t *testing.T) {
	withDashboardTempDir(t)
	if err := writeDashboardPIDFile(os.Getpid()); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}
	running, pid, err := runDashboardStatus()
	if err != nil {
		t.Fatalf("runDashboardStatus: %v", err)
	}
	if !running || pid != os.Getpid() {
		t.Fatalf("runDashboardStatus = (%v, %d); want (true, %d)", running, pid, os.Getpid())
	}
}

// TestRunDashboardStart_RefusesWhenAlreadyRunning asserts the start
// command returns a "already running" error when the PID file holds
// a live PID. Uses our own PID as the "live" PID — safe and
// portable.
func TestRunDashboardStart_RefusesWhenAlreadyRunning(t *testing.T) {
	withDashboardTempDir(t)
	if err := writeDashboardPIDFile(os.Getpid()); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so runDashboardStart won't block if it slips past the guard.

	err := runDashboardStart(ctx)
	if err == nil {
		t.Fatal("runDashboardStart with live PID = nil; want already-running error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("runDashboardStart error = %v; want 'already running'", err)
	}
}

// TestRunDashboardStart_CleansStalePID asserts start succeeds (does
// not error on entry) when the PID file holds a dead PID, and that
// the stale PID is replaced by our own. Since runDashboardStart
// blocks on SIGTERM we spawn it in a goroutine and cancel the parent
// context to shut it down.
func TestRunDashboardStart_CleansStalePID(t *testing.T) {
	withDashboardTempDir(t)
	if err := writeDashboardPIDFile(2_000_000_000); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = runDashboardStart(ctx)
	}()

	// Wait for runDashboardStart to have written its own PID.
	waitUntil(t, 2*time.Second, func() bool {
		pid, err := readDashboardPIDFile()
		return err == nil && pid == os.Getpid()
	}, "PID file never updated to self PID")

	cancel()
	wg.Wait()

	if runErr != nil {
		t.Fatalf("runDashboardStart returned %v; want nil", runErr)
	}

	// After clean shutdown the PID file should be gone.
	if pid, err := readDashboardPIDFile(); err != nil || pid != 0 {
		t.Fatalf("post-shutdown PID file = (%d, %v); want (0, nil)", pid, err)
	}
}

// TestRunDashboardStop_NoPIDFileIsOK asserts stop is a no-op when
// there's nothing running.
func TestRunDashboardStop_NoPIDFileIsOK(t *testing.T) {
	withDashboardTempDir(t)
	if err := runDashboardStop(); err != nil {
		t.Fatalf("runDashboardStop on clean dir: %v", err)
	}
}

// TestRunDashboardStop_CleansStalePID asserts stop removes a stale
// PID file without signalling.
func TestRunDashboardStop_CleansStalePID(t *testing.T) {
	withDashboardTempDir(t)
	if err := writeDashboardPIDFile(2_000_000_000); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}
	if err := runDashboardStop(); err != nil {
		t.Fatalf("runDashboardStop on stale PID: %v", err)
	}
	if pid, err := readDashboardPIDFile(); err != nil || pid != 0 {
		t.Fatalf("post-stop PID file = (%d, %v); want (0, nil)", pid, err)
	}
}

// TestRunDashboardStop_SignalsLiveChild asserts stop sends SIGTERM
// to a live child. Uses `sleep 30` as a stand-in for the (future)
// Node server — it respects SIGTERM and exits.
func TestRunDashboardStop_SignalsLiveChild(t *testing.T) {
	withDashboardTempDir(t)

	// Spawn a child that will outlive a quick test if not signalled.
	// Using /bin/sh so SIGTERM propagates predictably.
	proc, err := startTestChild(t, "sleep", "30")
	if err != nil {
		t.Fatalf("startTestChild: %v", err)
	}

	if err := writeDashboardPIDFile(proc.Pid); err != nil {
		t.Fatalf("writeDashboardPIDFile: %v", err)
	}

	if err := runDashboardStop(); err != nil {
		t.Fatalf("runDashboardStop on live child: %v", err)
	}

	// After stop, the PID file is gone and the child is dead.
	if pid, err := readDashboardPIDFile(); err != nil || pid != 0 {
		t.Fatalf("post-stop PID file = (%d, %v); want (0, nil)", pid, err)
	}

	// startTestChild installed a t.Cleanup that Waits; confirm the
	// child is no longer alive before we return.
	if dashboardProcessAlive(proc.Pid) {
		t.Fatalf("child %d still alive after runDashboardStop", proc.Pid)
	}
}

func TestRunDashboardLogs_NoFile(t *testing.T) {
	withDashboardTempDir(t)
	var buf bytes.Buffer
	if err := runDashboardLogs(context.Background(), &buf, false); err != nil {
		t.Fatalf("runDashboardLogs: %v", err)
	}
	if !strings.Contains(buf.String(), "no log file") {
		t.Fatalf("output = %q; want 'no log file' message", buf.String())
	}
}

func TestRunDashboardLogs_ReadsFile(t *testing.T) {
	dir := withDashboardTempDir(t)
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	content := "hello from the dashboard\nsecond line\n"
	if err := os.WriteFile(filepath.Join(logDir, "dashboard.log"), []byte(content), 0o644); err != nil {
		t.Fatalf("seeding log: %v", err)
	}
	var buf bytes.Buffer
	if err := runDashboardLogs(context.Background(), &buf, false); err != nil {
		t.Fatalf("runDashboardLogs: %v", err)
	}
	if buf.String() != content {
		t.Fatalf("output = %q; want %q", buf.String(), content)
	}
}

// --- helpers -----------------------------------------------------------------

// startTestChild forks a real child process and registers a cleanup
// that ensures it's terminated. A background goroutine Waits on the
// child immediately so it's reaped the moment it dies (otherwise a
// signal-0 probe sees the zombie as "alive" and confuses
// runDashboardStop's termination loop).
func startTestChild(t *testing.T, name string, args ...string) (*os.Process, error) {
	t.Helper()

	attr := &os.ProcAttr{Files: []*os.File{os.Stdin, nil, nil}}
	path, err := osLookPath(name)
	if err != nil {
		return nil, err
	}

	proc, err := os.StartProcess(path, append([]string{name}, args...), attr)
	if err != nil {
		return nil, err
	}

	reaped := make(chan struct{})
	go func() {
		_, _ = proc.Wait()
		close(reaped)
	}()

	t.Cleanup(func() {
		if dashboardProcessAlive(proc.Pid) {
			_ = proc.Signal(syscall.SIGKILL)
		}
		<-reaped
	})

	return proc, nil
}

// osLookPath is a tiny wrapper so the test file doesn't import
// os/exec just to find `sleep`. Falls back to /bin/<name> and
// /usr/bin/<name>.
func osLookPath(name string) (string, error) {
	for _, prefix := range []string{"/bin/", "/usr/bin/", "/usr/local/bin/"} {
		candidate := prefix + name
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", name)
}

// waitUntil polls fn every 20 ms until it returns true or the
// timeout elapses. Fails the test with msg on timeout.
func waitUntil(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitUntil: %s (after %s)", msg, timeout)
}

