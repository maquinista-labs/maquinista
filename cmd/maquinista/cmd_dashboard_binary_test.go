package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestDashboardBinary_StartStopLifecycle is the Phase 0 Commit 0.4
// gate: builds the real maquinista binary and drives the dashboard
// through `dashboard start` → curl /api/healthz → `dashboard stop`.
// Post-D.2 `dashboard start` detaches by default: the invoked
// process exits immediately after writing the child PID, and the
// PID file names the detached child — not the CLI we spawned.
func TestDashboardBinary_StartStopLifecycle(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping binary integration test")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping binary integration test")
	}

	bin := buildMaquinistaBinary(t)

	// HOME override isolates ~/.maquinista to the test's temp dir.
	home := t.TempDir()
	port := pickFreePort(t)
	listen := net.JoinHostPort("127.0.0.1", port)

	env := append(os.Environ(),
		"HOME="+home,
		"MAQUINISTA_DASHBOARD_LISTEN="+listen,
	)

	// `dashboard start` detaches; its own Wait returns after the
	// parent prints the banner.
	start := exec.Command(bin, "dashboard", "start")
	start.Env = env
	start.Stdout = os.Stdout
	start.Stderr = os.Stderr
	if err := start.Run(); err != nil {
		t.Fatalf("dashboard start: %v", err)
	}

	// Wait for the dashboard to report healthy on /api/healthz —
	// proves the detached child is alive and bound.
	url := "http://" + listen + "/api/healthz"
	resp := waitForHealthz(t, url, 15*time.Second)
	resp.Body.Close()

	// Read the PID file — it now holds the detached child's PID,
	// not start.Process.Pid (that parent already exited).
	pidFile := filepath.Join(home, ".maquinista", "dashboard.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read %s: %v", pidFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("PID file contents %q not an int: %v", data, err)
	}
	if !dashboardProcessAlive(pid) {
		t.Fatalf("detached child PID %d from %s is not alive", pid, pidFile)
	}

	// Reap the detached child in case `dashboard stop` fails below.
	// The kernel parent is this test process (detach's Release only
	// disowns Go bookkeeping), so we need Wait4 to reap its zombie
	// when it exits.
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		for {
			var ws syscall.WaitStatus
			wpid, err := syscall.Wait4(pid, &ws, 0, nil)
			if err != nil {
				return
			}
			if wpid == pid {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if dashboardProcessAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
		}
	})

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

	// Stop subcommand should terminate the detached child.
	stop := exec.Command(bin, "dashboard", "stop")
	stop.Env = env
	stopOut, stopErr := stop.CombinedOutput()
	if stopErr != nil {
		t.Fatalf("dashboard stop: %v\n%s", stopErr, stopOut)
	}

	// The detached child should exit within the grace window. We
	// observe via the reaper goroutine (the child becomes a zombie,
	// Wait4 succeeds, reaped closes).
	select {
	case <-reaped:
	case <-time.After(15 * time.Second):
		t.Fatal("detached dashboard child did not exit after `dashboard stop`")
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
// `dashboard start` fails while the first is live. Post-D.2 the
// "first" is a detached child; the CLI process that spawned it
// exits immediately.
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
	if err := first.Run(); err != nil {
		t.Fatalf("first dashboard start: %v", err)
	}

	url := "http://" + listen + "/api/healthz"
	resp := waitForHealthz(t, url, 15*time.Second)
	resp.Body.Close()

	// Reap the detached child on cleanup.
	pidFile := filepath.Join(home, ".maquinista", "dashboard.pid")
	pidData, _ := os.ReadFile(pidFile)
	childPID, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if childPID > 0 {
		t.Cleanup(func() {
			if dashboardProcessAlive(childPID) {
				_ = syscall.Kill(childPID, syscall.SIGTERM)
			}
			// Best-effort reap so the child doesn't linger as a
			// zombie under the test process.
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(childPID, &ws, 0, nil)
		})
	}

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
}

// --- helpers -----------------------------------------------------------------

var (
	binaryBuildOnce sync.Once
	binaryPath      string
	binaryDir       string // dir holding binaryPath; TestMain removes it
	binaryBuildErr  error
)

// TestMain tears the shared binary build down at process exit.
// The binary is built once per test-binary run and shared across
// every binary-using test, so per-test t.Cleanup would delete it
// under later tests' feet — only after m.Run() returns is removal
// safe. Without this every `go test ./cmd/maquinista` leaked a
// ~70MB /tmp/maquinista-bin-* dir; enough runs ENOSPC'd the /tmp
// partition and mass-failed unrelated suites (infra red).
func TestMain(m *testing.M) {
	// Dirs orphaned by killed test runs (SIGKILL, crash) never see
	// the teardown below; sweep them, age-gated so a live concurrent
	// run's binary is not removed under it.
	pruneStaleBinaryDirs(os.TempDir(), time.Hour)

	code := m.Run()

	if binaryDir != "" {
		if err := os.RemoveAll(binaryDir); err != nil {
			log.Printf("cmd/maquinista tests: removing %s: %v", binaryDir, err)
		}
	}
	os.Exit(code)
}

// pruneStaleBinaryDirs removes maquinista-bin-* temp dirs under root
// older than maxAge, warning for each. A dir younger than maxAge may
// belong to a concurrently running test binary (two `go test` invocations
// can race on this box — gate vs human); those are left alone.
func pruneStaleBinaryDirs(root string, maxAge time.Duration) {
	matches, err := filepath.Glob(filepath.Join(root, "maquinista-bin-*"))
	if err != nil {
		return
	}
	for _, dir := range matches {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		age := time.Since(info.ModTime())
		if age < maxAge {
			continue
		}
		log.Printf("cmd/maquinista tests: WARNING removing stale %s (age %s) — orphaned by a killed test run", dir, age.Round(time.Second))
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("cmd/maquinista tests: removing %s: %v", dir, err)
		}
	}
}

// buildMaquinistaBinary builds `go build ./cmd/maquinista` into a
// temp file once per test binary and returns its path. Subsequent
// calls return the same path. The temp dir is removed by TestMain
// at process exit — never by a per-test cleanup, which would race
// with later tests still exec'ing the binary. Skips the test if the
// build fails (e.g. offline module fetch).
func buildMaquinistaBinary(t *testing.T) string {
	t.Helper()
	binaryBuildOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "maquinista-bin-*")
		if err != nil {
			binaryBuildErr = err
			return
		}
		binaryDir = tmpDir
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
