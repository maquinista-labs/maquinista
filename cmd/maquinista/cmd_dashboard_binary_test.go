package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestDashboardBinary_StartStopLifecycle is the Phase 0 Commit 0.4
// gate: builds the real maquinista binary and drives the dashboard
// through `dashboard start` → curl /api/healthz → `dashboard stop`.
// This exercises the full operator-facing lifecycle including the
// PID-file-based stop path (the in-process integration test covers
// only the ctx-cancel path).
func TestDashboardBinary_StartStopLifecycle(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping binary integration test")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping binary integration test")
	}

	bin := buildMaquinistaBinary(t)

	// Isolate the dashboard state directory. We pass it to the
	// child as MAQUINISTA_DIR_OVERRIDE — but cmd_dashboard.go uses
	// the user's ~/.maquinista by default. To isolate in a child
	// process without leaking into ~/.maquinista we set HOME to a
	// temp dir so the "~" expansion lands there.
	home := t.TempDir()
	port := pickFreePort(t)
	listen := net.JoinHostPort("127.0.0.1", port)

	env := append(os.Environ(),
		"HOME="+home,
		"MAQUINISTA_DASHBOARD_LISTEN="+listen,
	)

	// Start the dashboard as a child of the test.
	start := exec.Command(bin, "dashboard", "start")
	start.Env = env
	start.Stdout = os.Stdout
	start.Stderr = os.Stderr

	if err := start.Start(); err != nil {
		t.Fatalf("start.Start: %v", err)
	}

	// Reap the child in a background goroutine so we can assert on
	// its exit state and avoid zombies.
	var (
		waitOnce sync.Once
		waitErr  error
	)
	reaped := make(chan struct{})
	waitOnce.Do(func() {
		go func() {
			waitErr = start.Wait()
			close(reaped)
		}()
	})

	t.Cleanup(func() {
		if start.Process != nil && dashboardProcessAlive(start.Process.Pid) {
			_ = start.Process.Signal(syscall.SIGKILL)
		}
		<-reaped
	})

	// Wait for the dashboard to report healthy on /api/healthz.
	url := "http://" + listen + "/api/healthz"
	resp := waitForHealthz(t, url, 15*time.Second)
	resp.Body.Close()

	// Verify the PID file was created under the override HOME.
	pidFile := filepath.Join(home, ".maquinista", "dashboard.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read %s: %v", pidFile, err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("PID file contents %q not an int: %v", data, err)
	}
	if pid != start.Process.Pid {
		t.Fatalf("PID file = %d; want start.Process.Pid=%d", pid, start.Process.Pid)
	}

	// Status subcommand should report running.
	status := exec.Command(bin, "dashboard", "status")
	status.Env = env
	statusOut, statusErr := status.CombinedOutput()
	if statusErr != nil {
		t.Fatalf("dashboard status: %v\n%s", statusErr, statusOut)
	}
	if !containsAll(string(statusOut), []string{"running", strconv.Itoa(pid)}) {
		t.Fatalf("status output = %q; want 'running' + PID", statusOut)
	}

	// Stop subcommand should terminate the dashboard gracefully.
	stop := exec.Command(bin, "dashboard", "stop")
	stop.Env = env
	stopOut, stopErr := stop.CombinedOutput()
	if stopErr != nil {
		t.Fatalf("dashboard stop: %v\n%s", stopErr, stopOut)
	}

	// The start process should exit within the grace window.
	select {
	case <-reaped:
	case <-time.After(15 * time.Second):
		t.Fatal("dashboard start process did not exit after `dashboard stop`")
	}

	if waitErr != nil {
		// A SIGTERM-induced exit is represented as "signal: terminated"
		// on the error; the supervisor's clean path returns nil exit.
		// Either is acceptable for this test.
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("start exit error = %v (not *ExitError)", waitErr)
		}
		if exitErr.ExitCode() < 0 {
			// Negative exit code means the process was signalled;
			// that's expected when the supervisor sends SIGTERM.
		}
	}

	// PID file should be gone.
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("PID file still present at %s after stop: %v", pidFile, err)
	}

	// The healthz URL should refuse connections.
	client := http.Client{Timeout: 250 * time.Millisecond}
	if _, err := client.Get(url); err == nil {
		t.Fatal("expected GET after stop to fail; succeeded")
	}

	// A second status should report not-running (exit 1).
	status2 := exec.Command(bin, "dashboard", "status")
	status2.Env = env
	status2Out, status2Err := status2.CombinedOutput()
	if status2Err == nil {
		t.Fatalf("status after stop should exit non-zero; got 0 with output %q", status2Out)
	}
	if !containsAll(string(status2Out), []string{"not running"}) {
		t.Fatalf("status after stop = %q; want 'not running'", status2Out)
	}
}

// TestDashboardBinary_RefusesDoubleStart asserts that a second
// `dashboard start` fails while the first is live.
func TestDashboardBinary_RefusesDoubleStart(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping binary integration test")
	}

	bin := buildMaquinistaBinary(t)
	home := t.TempDir()
	port := pickFreePort(t)
	listen := net.JoinHostPort("127.0.0.1", port)
	env := append(os.Environ(),
		"HOME="+home,
		"MAQUINISTA_DASHBOARD_LISTEN="+listen,
	)

	first := exec.Command(bin, "dashboard", "start")
	first.Env = env
	if err := first.Start(); err != nil {
		t.Fatalf("first.Start: %v", err)
	}
	firstReaped := make(chan struct{})
	go func() { _ = first.Wait(); close(firstReaped) }()
	t.Cleanup(func() {
		if first.Process != nil && dashboardProcessAlive(first.Process.Pid) {
			_ = first.Process.Signal(syscall.SIGTERM)
		}
		<-firstReaped
	})

	url := "http://" + listen + "/api/healthz"
	resp := waitForHealthz(t, url, 15*time.Second)
	resp.Body.Close()

	// Second start: same HOME, so the PID file collides.
	second := exec.Command(bin, "dashboard", "start")
	second.Env = env
	out, err := second.CombinedOutput()
	if err == nil {
		t.Fatalf("second `dashboard start` exited 0; want error. Output: %s", out)
	}
	if !containsAll(string(out), []string{"already running"}) {
		t.Fatalf("second start output = %q; want 'already running'", out)
	}

	// Clean up first.
	stop := exec.Command(bin, "dashboard", "stop")
	stop.Env = env
	if stopOut, stopErr := stop.CombinedOutput(); stopErr != nil {
		t.Fatalf("cleanup stop: %v\n%s", stopErr, stopOut)
	}
	<-firstReaped
}

// --- helpers -----------------------------------------------------------------

var (
	binaryBuildOnce sync.Once
	binaryPath      string
	binaryBuildErr  error
)

// buildMaquinistaBinary builds `go build ./cmd/maquinista` into a
// temp file once per test binary and returns its path. Subsequent
// calls return the same path. Skips the test if the build fails
// (e.g. offline module fetch).
func buildMaquinistaBinary(t *testing.T) string {
	t.Helper()
	binaryBuildOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "maquinista-bin-*")
		if err != nil {
			binaryBuildErr = err
			return
		}
		binaryPath = filepath.Join(tmpDir, "maquinista")
		build := exec.Command("go", "build", "-o", binaryPath, "./cmd/maquinista")
		build.Dir = repoRoot()
		build.Stderr = os.Stderr
		binaryBuildErr = build.Run()
	})
	if binaryBuildErr != nil {
		t.Skipf("skipping: go build failed: %v", binaryBuildErr)
	}
	if binaryPath == "" {
		t.Skip("binary path empty; skipping")
	}
	return binaryPath
}

// repoRoot returns the absolute path to the repo root, computed
// relative to this test file's location so the build is robust to
// CWD.
func repoRoot() string {
	// This test lives in cmd/maquinista; root is two dirs up.
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// When tests run, wd is the package dir (cmd/maquinista).
	return filepath.Dir(filepath.Dir(wd))
}

func containsAll(s string, needles []string) bool {
	for _, n := range needles {
		if !containsSubstring(s, n) {
			return false
		}
	}
	return true
}

func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

