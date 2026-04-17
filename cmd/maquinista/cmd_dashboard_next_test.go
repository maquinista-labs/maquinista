package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestDashboardBinary_RealNextServer is the Phase 1 Commit 1.6 gate:
// builds the Next.js standalone bundle, starts the dashboard with
// --no-embed pointing at it, and asserts /api/healthz responds with
// stub:false (proving we're on the real Next-served route handler,
// not the Phase 0 Node one-liner).
//
// This is an expensive test (~30-60 s on cold cache, ~10 s warm) so
// it runs behind MAQUINISTA_DASHBOARD_NEXT_E2E=1 by default. CI
// opts in explicitly.
func TestDashboardBinary_RealNextServer(t *testing.T) {
	if os.Getenv("MAQUINISTA_DASHBOARD_NEXT_E2E") != "1" {
		t.Skip("set MAQUINISTA_DASHBOARD_NEXT_E2E=1 to run the real-Next-server integration test")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}

	root := repoRoot()
	webDir := filepath.Join(root, "internal", "dashboard", "web")
	standaloneDir := filepath.Join(webDir, ".next", "standalone")

	// Build the bundle if missing (fresh clone) or stale (server.js
	// older than next.config.ts).
	if needsBuild(standaloneDir, webDir) {
		t.Logf("running `npm install` + `npm run build` in %s (may take ~60 s)", webDir)
		execShell(t, webDir, "npm", "install")
		execShell(t, webDir, "npm", "run", "build")
		// Copy public + .next/static into the standalone tree per
		// Next docs.
		copyDir(t, filepath.Join(webDir, "public"), filepath.Join(standaloneDir, "public"))
		copyDir(t, filepath.Join(webDir, ".next", "static"), filepath.Join(standaloneDir, ".next", "static"))
	}

	bin := buildMaquinistaBinary(t)
	home := t.TempDir()
	port := pickFreePort(t)
	listen := net.JoinHostPort("127.0.0.1", port)
	env := append(os.Environ(),
		"HOME="+home,
		"MAQUINISTA_DASHBOARD_LISTEN="+listen,
	)

	start := exec.Command(bin, "dashboard", "start", "--no-embed", standaloneDir)
	start.Env = env
	start.Stdout = os.Stdout
	start.Stderr = os.Stderr

	if err := start.Start(); err != nil {
		t.Fatalf("start.Start: %v", err)
	}

	reaped := make(chan struct{})
	go func() { _ = start.Wait(); close(reaped) }()
	t.Cleanup(func() {
		if start.Process != nil && dashboardProcessAlive(start.Process.Pid) {
			_ = start.Process.Signal(syscall.SIGTERM)
		}
		<-reaped
	})

	url := "http://" + listen + "/api/healthz"
	resp := waitForHealthz(t, url, 30*time.Second)
	defer resp.Body.Close()

	var parsed struct {
		OK      bool   `json:"ok"`
		Stub    bool   `json:"stub"`
		Version string `json:"version"`
		PID     int    `json:"pid"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal healthz body %q: %v", body, err)
	}
	if !parsed.OK {
		t.Fatalf("/api/healthz ok=false; body=%q", body)
	}
	if parsed.Stub {
		t.Fatalf("/api/healthz stub=true; expected real Next-served handler (stub:false)")
	}
	if parsed.PID == 0 {
		t.Fatalf("/api/healthz missing pid; body=%q", body)
	}

	// /agents should render an HTML shell with the bottom-nav tests ids.
	agentsURL := "http://" + listen + "/agents"
	ar, err := http.Get(agentsURL)
	if err != nil {
		t.Fatalf("GET /agents: %v", err)
	}
	defer ar.Body.Close()
	if ar.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents = %d; want 200", ar.StatusCode)
	}
	html, _ := io.ReadAll(ar.Body)
	for _, marker := range []string{"data-testid=\"dash-header\"", "data-testid=\"bottom-nav\"", "data-testid=\"agents-placeholder\""} {
		if !strings.Contains(string(html), marker) {
			t.Errorf("/agents HTML missing %s", marker)
		}
	}

	// / should redirect to /agents (Next returns 307 for app-router
	// redirect()).
	rc := http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	rr, err := rc.Get("http://" + listen + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer rr.Body.Close()
	if rr.StatusCode < 300 || rr.StatusCode >= 400 {
		t.Fatalf("GET / = %d; want 3xx redirect", rr.StatusCode)
	}
	if loc := rr.Header.Get("Location"); !strings.Contains(loc, "/agents") {
		t.Fatalf("GET / redirect Location = %q; want /agents", loc)
	}

	// Clean stop via the CLI.
	stop := exec.Command(bin, "dashboard", "stop")
	stop.Env = env
	if stopOut, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("dashboard stop: %v\n%s", err, stopOut)
	}

	select {
	case <-reaped:
	case <-time.After(15 * time.Second):
		t.Fatal("start process did not exit after `dashboard stop`")
	}
}

// --- helpers -----------------------------------------------------------------

func needsBuild(standaloneDir, webDir string) bool {
	serverJS := filepath.Join(standaloneDir, "server.js")
	serverStat, err := os.Stat(serverJS)
	if err != nil {
		return true
	}
	// Trigger a rebuild if next.config.ts is newer than server.js.
	if cfg, err := os.Stat(filepath.Join(webDir, "next.config.ts")); err == nil {
		if cfg.ModTime().After(serverStat.ModTime()) {
			return true
		}
	}
	return false
}

func execShell(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v in %s: %v", name, args, dir, err)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return // Next.js may omit public/ on projects without assets.
		}
		t.Fatalf("stat %s: %v", src, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", src)
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("readdir %s: %v", src, err)
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyDir(t, sp, dp)
			continue
		}
		b, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("read %s: %v", sp, err)
		}
		if err := os.WriteFile(dp, b, 0o644); err != nil {
			t.Fatalf("write %s: %v", dp, err)
		}
	}
}
