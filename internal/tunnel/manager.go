// Package tunnel manages an ephemeral Cloudflare Quick Tunnel that exposes a
// local port to a public HTTPS URL. No Cloudflare account is required — Quick
// Tunnels are anonymous and temporary by design.
//
// Usage:
//
//	m := tunnel.NewManager(notify)
//	url, err := m.Start(ctx, "127.0.0.1:8900", 15*time.Minute)
//	// … later …
//	m.Stop()
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

// reURL matches the Quick Tunnel URL printed to stderr by cloudflared.
var reURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// ErrCloudflaredNotFound is returned when cloudflared is not on PATH.
var ErrCloudflaredNotFound = errors.New("cloudflared not found on PATH — install it with: curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared && chmod +x /usr/local/bin/cloudflared")

// ErrURLTimeout is returned when cloudflared starts but the public URL is not
// printed within the startup deadline (10 s).
var ErrURLTimeout = errors.New("cloudflared started but did not print a public URL within 10 s")

// Manager owns the lifecycle of a single cloudflared tunnel process.
// It is safe for concurrent use.
type Manager struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc // cancels the process context (and optional TTL)
	url     string
	startAt time.Time
	dur     time.Duration // 0 means no expiry
	notify  func(string)  // called on expiry with a human-readable message
}

// NewManager creates a Manager. notify is an optional callback invoked when the
// tunnel expires due to a duration limit; pass nil to skip expiry notifications.
func NewManager(notify func(string)) *Manager {
	if notify == nil {
		notify = func(string) {}
	}
	return &Manager{notify: notify}
}

// Start opens a Quick Tunnel to localAddr (e.g. "127.0.0.1:8900") and returns
// the public HTTPS URL. If dur > 0 the tunnel is automatically torn down after
// that duration and notify is called. If a tunnel is already running, Start
// returns an error — callers should check IsRunning first.
func (m *Manager) Start(ctx context.Context, localAddr string, dur time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil {
		return "", fmt.Errorf("tunnel already running at %s", m.url)
	}

	// Verify cloudflared is available before spawning.
	cfPath, err := lookupCloudflared()
	if err != nil {
		return "", ErrCloudflaredNotFound
	}

	// Build process context. We use a separate cancelable context so the
	// process can be stopped independently of the caller's ctx.
	procCtx, cancel := context.WithCancel(ctx)

	cmd := exec.CommandContext(procCtx, cfPath, "tunnel",
		"--no-autoupdate",
		"--edge-ip-version", "6",
		"--url", "http://"+localAddr,
	)

	// cloudflared prints the URL to stderr.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("stderr pipe: %w", err)
	}
	// Drain stdout to avoid blocking the process.
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("starting cloudflared: %w", err)
	}

	// Scan stderr for the public URL with a 10 s deadline.
	urlCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			log.Printf("cloudflared: %s", line)
			if m := reURL.FindString(line); m != "" {
				urlCh <- m
				return
			}
		}
		close(urlCh)
	}()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()

	var publicURL string
	select {
	case u, ok := <-urlCh:
		if !ok {
			_ = cmd.Process.Kill()
			cancel()
			return "", ErrURLTimeout
		}
		publicURL = u
	case <-deadline.C:
		_ = cmd.Process.Kill()
		cancel()
		return "", ErrURLTimeout
	case <-procCtx.Done():
		cancel()
		return "", procCtx.Err()
	}

	m.cmd = cmd
	m.cancel = cancel
	m.url = publicURL
	m.startAt = time.Now()
	m.dur = dur

	// If a duration is set, schedule auto-stop in the background.
	if dur > 0 {
		go func() {
			select {
			case <-time.After(dur):
				m.Stop()
				m.notify(fmt.Sprintf("Tunnel expired after %s. Send /dashboard to reopen.", dur.Round(time.Second)))
			case <-procCtx.Done():
				// Stopped manually — no notification.
			}
		}()
	}

	// Reap the child when it exits so we don't leave a zombie.
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		// Only clear state if this is still the same command (not a race with a
		// concurrent Start after Stop).
		if m.cmd == cmd {
			m.cmd = nil
			m.url = ""
		}
		m.mu.Unlock()
	}()

	return publicURL, nil
}

// Stop kills the tunnel process. It is idempotent.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil {
		return
	}
	m.cancel()
	// Process will be reaped by the goroutine in Start.
	m.cmd = nil
	m.url = ""
}

// IsRunning reports whether a tunnel process is alive.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil
}

// URL returns the current public HTTPS URL, or empty string if not running.
func (m *Manager) URL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.url
}

// RemainingTime returns how long until the tunnel expires. Returns 0 if no
// expiry is set or if the tunnel is not running.
func (m *Manager) RemainingTime() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.dur == 0 {
		return 0
	}
	elapsed := time.Since(m.startAt)
	if elapsed >= m.dur {
		return 0
	}
	return m.dur - elapsed
}

// lookupCloudflared finds the cloudflared binary. It first tries PATH, then
// falls back to common install locations that may not be in the daemon's PATH.
func lookupCloudflared() (string, error) {
	for _, candidate := range []string{
		"cloudflared",
		"/usr/local/bin/cloudflared",
		"/usr/bin/cloudflared",
		"/opt/homebrew/bin/cloudflared",
	} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p, nil
		}
	}
	return "", ErrCloudflaredNotFound
}
