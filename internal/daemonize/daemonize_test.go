package daemonize

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// When this env var is set, TestMain hands control to the helper
// child mode instead of running the Go test suite. The detach tests
// (below) call this binary with the env set so the "daemon" is a
// controlled process we can inspect.
const testChildEnv = "DAEMONIZE_TEST_CHILD"

// TestMain wires the helper-child mode. If DAEMONIZE_TEST_CHILD=1 is
// set, we are the re-exec'd "daemon"; run in foreground mode and
// block until SIGTERM/SIGINT.
func TestMain(m *testing.M) {
	if os.Getenv(testChildEnv) == "1" {
		spec := Spec{
			Name:       "daemonize-test",
			LogPath:    os.Getenv("DAEMONIZE_TEST_LOG"),
			PIDPath:    os.Getenv("DAEMONIZE_TEST_PID"),
			Foreground: true,
		}
		err := Run(context.Background(), spec, func(ctx context.Context) error {
			// Print a sentinel line so the parent test can verify the
			// log pipe is wired.
			fmt.Println("child: started")
			<-ctx.Done()
			fmt.Println("child: stopping")
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "child error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// --- PID helpers ------------------------------------------------------------

func TestPIDRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	if pid, err := readPID(path); err != nil || pid != 0 {
		t.Fatalf("readPID on empty dir = (%d, %v); want (0, nil)", pid, err)
	}
	if err := writePID(path, 4242); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	pid, err := readPID(path)
	if err != nil {
		t.Fatalf("readPID: %v", err)
	}
	if pid != 4242 {
		t.Fatalf("readPID = %d; want 4242", pid)
	}
}

func TestReadPIDMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")
	if err := os.WriteFile(path, []byte("not-a-number"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := readPID(path); err == nil || !strings.Contains(err.Error(), "invalid PID file") {
		t.Fatalf("readPID on garbage = %v; want invalid PID error", err)
	}
}

func TestProcessAlive(t *testing.T) {
	if processAlive(0) {
		t.Fatal("processAlive(0) = true; want false")
	}
	if processAlive(-1) {
		t.Fatal("processAlive(-1) = true; want false")
	}
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(self) = false; want true")
	}
}

func TestHasForegroundFlag(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"start"}, false},
		{[]string{"start", "--foreground"}, true},
		{[]string{"start", "-F"}, true},
		{[]string{"start", "--foreground=true"}, true},
		{[]string{"start", "-F=true"}, true},
		{[]string{"start", "--listen", ":8900"}, false},
	}
	for _, tc := range cases {
		if got := hasForegroundFlag(tc.argv); got != tc.want {
			t.Errorf("hasForegroundFlag(%v) = %v; want %v", tc.argv, got, tc.want)
		}
	}
}

// --- Status -----------------------------------------------------------------

func TestStatus_NotRunning(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid")}
	pid, alive, err := Status(spec)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if pid != 0 || alive {
		t.Fatalf("Status = (%d, %v); want (0, false)", pid, alive)
	}
}

func TestStatus_StalePID(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid")}
	if err := writePID(spec.PIDPath, 2_000_000_000); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	pid, alive, err := Status(spec)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if pid != 0 || alive {
		t.Fatalf("Status = (%d, %v); want (0, false) for stale PID", pid, alive)
	}
}

func TestStatus_LiveSelf(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid")}
	if err := writePID(spec.PIDPath, os.Getpid()); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	pid, alive, err := Status(spec)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !alive || pid != os.Getpid() {
		t.Fatalf("Status = (%d, %v); want (%d, true)", pid, alive, os.Getpid())
	}
}

// --- Stop -------------------------------------------------------------------

func TestStop_NoPIDFile(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid")}
	if err := Stop(spec, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStop_StalePID(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid")}
	if err := writePID(spec.PIDPath, 2_000_000_000); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	if err := Stop(spec, time.Second); err != nil {
		t.Fatalf("Stop on stale PID: %v", err)
	}
	if _, err := os.Stat(spec.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("PID file still present after Stop: %v", err)
	}
}

func TestStop_SignalsLiveChild(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid")}

	proc, reaped := startSleepChild(t, 30)
	if err := writePID(spec.PIDPath, proc.Pid); err != nil {
		t.Fatalf("writePID: %v", err)
	}

	if err := Stop(spec, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-reaped:
	case <-time.After(3 * time.Second):
		t.Fatal("child not reaped after Stop")
	}
	if _, err := os.Stat(spec.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("PID file still present after Stop: %v", err)
	}
}

// --- Run foreground ---------------------------------------------------------

func TestRunForeground_WritesAndRemovesPID(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid"), Foreground: true}

	// work signals it has started, then blocks on ctx.
	started := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), spec, func(ctx context.Context) error {
			started <- os.Getpid()
			<-ctx.Done()
			return nil
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("work never started")
	}

	// PID file should be present with our PID.
	pid, err := readPID(spec.PIDPath)
	if err != nil {
		t.Fatalf("readPID: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("PID file = %d; want %d", pid, os.Getpid())
	}

	// SIGTERM ourselves via the signal path. Since that would kill
	// the test, we instead simulate ctx cancel by sending SIGTERM to
	// our own process… but that's risky. Easier: use Foreground=true
	// in a sub-process (see TestDetach_* below). For this test we
	// just verify the write; the sub-process test covers removal.
	_ = done
}

func TestRunForeground_RefusesWhenAlreadyRunning(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid"), Foreground: true}
	if err := writePID(spec.PIDPath, os.Getpid()); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	err := Run(context.Background(), spec, func(ctx context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Run with live PID = %v; want already-running error", err)
	}
}

func TestRunForeground_CleansStalePID(t *testing.T) {
	spec := Spec{Name: "x", PIDPath: filepath.Join(t.TempDir(), "x.pid"), Foreground: true}
	if err := writePID(spec.PIDPath, 2_000_000_000); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	ranCh := make(chan struct{}, 1)
	err := Run(context.Background(), spec, func(ctx context.Context) error {
		ranCh <- struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-ranCh:
	default:
		t.Fatal("work did not run after stale-PID cleanup")
	}
	// work returned cleanly → PID file removed on exit.
	if _, err := os.Stat(spec.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("PID file still present after Run exit: %v", err)
	}
}

// --- Detach (via child-mode TestMain) --------------------------------------

// TestDetach_EndToEnd exercises the full detach → child writes PID →
// log receives stdout → Stop terminates gracefully cycle using the
// test binary itself as the "daemon". Setsid + Release semantics are
// the production code path; no mocks.
func TestDetach_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	pidPath := filepath.Join(dir, "test.pid")

	spec := Spec{
		Name:    "daemonize-test",
		LogPath: logPath,
		PIDPath: pidPath,
	}

	// Override the exec builder to run the test binary in child
	// mode. env lives on the Cmd because the current process's env
	// does NOT have DAEMONIZE_TEST_CHILD set (otherwise we'd be in
	// child mode, not the test runner).
	prev := buildDetachCmd
	defer func() { buildDetachCmd = prev }()
	buildDetachCmd = func() (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			testChildEnv+"=1",
			"DAEMONIZE_TEST_LOG="+logPath,
			"DAEMONIZE_TEST_PID="+pidPath,
		)
		return cmd, nil
	}

	// Capture stdout so we don't pollute test output with the
	// "started (PID ...)" banner. (daemonize writes to os.Stdout
	// directly.)
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = origStdout })
	bannerDone := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		bannerDone <- buf.String()
	}()

	if err := Run(context.Background(), spec, func(ctx context.Context) error {
		t.Fatal("parent should not run work in detach path")
		return nil
	}); err != nil {
		t.Fatalf("Run (detach) returned: %v", err)
	}
	w.Close()
	banner := <-bannerDone
	if !strings.Contains(banner, "started (PID ") {
		t.Fatalf("banner = %q; want 'started (PID ...)'", banner)
	}

	// Wait for the child to register its PID.
	var childPID int
	waitUntil(t, 3*time.Second, func() bool {
		p, err := readPID(pidPath)
		if err != nil || p == 0 {
			return false
		}
		if !processAlive(p) {
			return false
		}
		childPID = p
		return true
	}, "child never wrote a live PID")

	// Reap the child in a goroutine. daemonize calls Release on the
	// parent, so Go isn't Waiting; the kernel still has us as parent
	// (Release doesn't change kernel relationships). Without this,
	// Stop's grace loop would wait on a zombie that processAlive
	// keeps reporting as alive. Production doesn't hit this because
	// the detach parent exits and init reaps. Drain non-blockingly.
	reapedCh := make(chan struct{})
	go func() {
		defer close(reapedCh)
		for {
			var ws syscall.WaitStatus
			wpid, err := syscall.Wait4(childPID, &ws, 0, nil)
			if err == nil && wpid == childPID {
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for the sentinel line to hit the log.
	waitUntil(t, 3*time.Second, func() bool {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return false
		}
		return strings.Contains(string(data), "child: started")
	}, "log never received 'child: started'")

	// Status should report running.
	pid, alive, err := Status(spec)
	if err != nil || !alive || pid != childPID {
		t.Fatalf("Status = (%d, %v, %v); want (%d, true, nil)", pid, alive, err, childPID)
	}

	// Stop should terminate the child gracefully.
	if err := Stop(spec, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("PID file still present after Stop: %v", err)
	}

	// The background reaper goroutine should have collected the
	// child's exit status by now (once SIGTERM propagated + work
	// returned). Wait for that.
	select {
	case <-reapedCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("child %d never exited", childPID)
	}

	// Log should contain both sentinel lines — proof the child
	// received SIGTERM and ran its shutdown path cleanly.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "child: stopping") {
		t.Fatalf("log missing 'child: stopping':\n%s", data)
	}
}

// TestDetach_RefusesWhenAlreadyRunning asserts the parent (not the
// child) returns an error if a live PID is on disk.
func TestDetach_RefusesWhenAlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	spec := Spec{
		Name:    "x",
		LogPath: filepath.Join(dir, "test.log"),
		PIDPath: filepath.Join(dir, "test.pid"),
	}
	if err := writePID(spec.PIDPath, os.Getpid()); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	err := Run(context.Background(), spec, func(ctx context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Run on live PID = %v; want already-running error", err)
	}
}

// --- helpers ----------------------------------------------------------------

func startSleepChild(t *testing.T, seconds int) (*os.Process, <-chan struct{}) {
	t.Helper()
	path, err := lookSleep()
	if err != nil {
		t.Skipf("sleep not found: %v", err)
	}
	proc, err := os.StartProcess(path,
		[]string{"sleep", fmt.Sprintf("%d", seconds)},
		&os.ProcAttr{Files: []*os.File{os.Stdin, nil, nil}})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		_, _ = proc.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		if processAlive(proc.Pid) {
			_ = proc.Signal(syscall.SIGKILL)
		}
		<-reaped
	})
	return proc, reaped
}

func lookSleep() (string, error) {
	for _, p := range []string{"/bin/sleep", "/usr/bin/sleep", "/usr/local/bin/sleep"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("sleep not found")
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("waitUntil: %s (after %s)", msg, timeout)
}

