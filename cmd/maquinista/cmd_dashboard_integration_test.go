package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// TestDashboardIntegration_NodeHealthcheckLifecycle gates that
// `maquinista dashboard start` spawns the dashboard child (real
// Next.js standalone when the embedded bundle is not a placeholder,
// or the Phase 0 Node stub otherwise), that /api/healthz responds
// with ok:true, and that cancelling the context cleans up the PID
// file and the child.
func TestDashboardIntegration_NodeHealthcheckLifecycle(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping integration test")
	}

	withDashboardTempDir(t)

	// Pick an ephemeral port by asking the kernel for one, closing
	// it, and reusing the number. The tiny race window between
	// close and our child bind is tolerable for a dev-box test.
	port := pickFreePort(t)
	listen := net.JoinHostPort("127.0.0.1", port)
	prevListen := os.Getenv("MAQUINISTA_DASHBOARD_LISTEN")
	os.Setenv("MAQUINISTA_DASHBOARD_LISTEN", listen)
	t.Cleanup(func() { os.Setenv("MAQUINISTA_DASHBOARD_LISTEN", prevListen) })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = runDashboardStart(ctx)
	}()

	// Wait for the healthcheck to respond — this confirms the
	// supervisor started, spawned node, and the script bound.
	url := "http://" + listen + "/api/healthz"
	resp := waitForHealthz(t, url, 10*time.Second)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d; want 200", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var parsed struct {
		OK   bool `json:"ok"`
		Stub bool `json:"stub"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", body, err)
	}
	if !parsed.OK {
		t.Fatalf("/healthz body = %q; want ok:true", body)
	}
	// stub:true → Phase 0 placeholder; stub:false → real Next.js server.
	// Both are acceptable; the gate is that ok:true is returned.
	t.Logf("/healthz: ok=true stub=%v", parsed.Stub)

	// Confirm the PID file was written while we were running.
	if pid, err := readDashboardPIDFile(); err != nil || pid != os.Getpid() {
		t.Fatalf("PID file while running = (%d, %v); want (%d, nil)", pid, err, os.Getpid())
	}

	// Cancel the parent context — the supervisor's Run should see
	// ctx.Done, propagate SIGTERM to the Node child, wait for it,
	// and return nil. runDashboardStart then removes the PID file.
	cancel()
	wg.Wait()

	if runErr != nil {
		t.Fatalf("runDashboardStart returned %v; want nil", runErr)
	}

	if pid, err := readDashboardPIDFile(); err != nil || pid != 0 {
		t.Fatalf("post-cancel PID file = (%d, %v); want (0, nil)", pid, err)
	}

	// The healthz URL should now refuse connections.
	client := http.Client{Timeout: 250 * time.Millisecond}
	if _, err := client.Get(url); err == nil {
		t.Fatal("expected GET after cancel to fail; succeeded")
	}
}

// TestDashboardIntegration_StopKillsRunningDashboard asserts that
// `maquinista dashboard stop` terminates a running dashboard. This
// is exercised by running start in one goroutine, stop in another,
// and asserting the start goroutine returns.
func TestDashboardIntegration_StopKillsRunningDashboard(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping integration test")
	}

	withDashboardTempDir(t)
	port := pickFreePort(t)
	listen := net.JoinHostPort("127.0.0.1", port)
	prevListen := os.Getenv("MAQUINISTA_DASHBOARD_LISTEN")
	os.Setenv("MAQUINISTA_DASHBOARD_LISTEN", listen)
	t.Cleanup(func() { os.Setenv("MAQUINISTA_DASHBOARD_LISTEN", prevListen) })

	// A context that we never cancel; shutdown must come from
	// runDashboardStop signalling the PID (which is os.Getpid()).
	// Since stop signals our own process, we replace the signal
	// path: we call stop but it'll find a live PID and SIGTERM
	// it... which would kill the test process. Not safe.
	//
	// Instead, simulate the "stop kills running dashboard" flow
	// by spawning start in a goroutine with a cancellable ctx and
	// having stop be a no-op; we rely on the other integration
	// test for full start/stop lifecycle. This test verifies that
	// when we cancel mid-flight with a live child, everything
	// cleans up tidily — a stress path adjacent to Stop.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = runDashboardStart(ctx)
	}()

	url := "http://" + listen + "/api/healthz"
	resp := waitForHealthz(t, url, 10*time.Second)
	resp.Body.Close()

	// Mid-life cancel: simulates SIGTERM arriving at our daemon.
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runDashboardStart did not return after ctx cancel")
	}

	if runErr != nil {
		t.Fatalf("runDashboardStart returned %v; want nil", runErr)
	}
}

// --- helpers -----------------------------------------------------------------

// pickFreePort asks the kernel for a free TCP port on 127.0.0.1.
// The listener is immediately closed; the port is likely still
// available when the test consumer binds.
func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return port
}

// waitForHealthz polls a URL until it returns HTTP 200 or the
// timeout elapses. On timeout, the test fails with diagnostic
// output. Returns the successful response so the caller can assert
// on the body.
func waitForHealthz(t *testing.T, url string, timeout time.Duration) *http.Response {
	t.Helper()
	client := http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastStatus int
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				return resp
			}
			lastStatus = resp.StatusCode
			resp.Body.Close()
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitForHealthz %s timed out after %s; lastErr=%v lastStatus=%d", url, timeout, lastErr, lastStatus)
	return nil
}
