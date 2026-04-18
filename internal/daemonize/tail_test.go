package daemonize

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTailLogs_NoFile(t *testing.T) {
	spec := Spec{Name: "x", LogPath: filepath.Join(t.TempDir(), "missing.log")}
	var buf bytes.Buffer
	if err := TailLogs(context.Background(), spec, false, &buf); err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	if !strings.Contains(buf.String(), "no log file") {
		t.Fatalf("output = %q; want 'no log file' banner", buf.String())
	}
}

func TestTailLogs_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	content := "hello\nsecond line\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	spec := Spec{Name: "x", LogPath: path}
	var buf bytes.Buffer
	if err := TailLogs(context.Background(), spec, false, &buf); err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	if buf.String() != content {
		t.Fatalf("output = %q; want %q", buf.String(), content)
	}
}

func TestTailLogs_FollowStreamsAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	if err := os.WriteFile(path, []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	spec := Spec{Name: "x", LogPath: path}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buf := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- TailLogs(ctx, spec, true, buf) }()

	waitForSubstr(t, buf, "initial", 2*time.Second)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("appended\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	waitForSubstr(t, buf, "appended", 2*time.Second)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TailLogs returned %v; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TailLogs did not return after ctx cancel")
	}
}

func TestTailLogs_FollowWaitsForFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	spec := Spec{Name: "x", LogPath: path}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buf := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- TailLogs(ctx, spec, true, buf) }()

	waitForSubstr(t, buf, "waiting for", 1*time.Second)

	if err := os.WriteFile(path, []byte("after-create\n"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	waitForSubstr(t, buf, "after-create", 2*time.Second)

	cancel()
	<-done
}

// --- helpers ---------------------------------------------------------------

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForSubstr(t *testing.T, b *syncBuffer, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), needle) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("waiting for %q in buffer (got %q)", needle, b.String())
}
