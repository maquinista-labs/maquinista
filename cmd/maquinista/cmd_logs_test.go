package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/daemonize"
)

// TestLogsCommandRegistered asserts the top-level daemon tailer is
// installed on rootCmd.
func TestLogsCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "logs" {
			return
		}
	}
	t.Error("expected 'logs' command to be registered on rootCmd")
}

// TestAgentLogsCommandRegistered asserts the pre-D.5 tmux-pane
// capture moved under `maquinista agent logs`.
func TestAgentLogsCommandRegistered(t *testing.T) {
	for _, c := range agentCmd.Commands() {
		if c.Use == "logs <agent-id>" {
			return
		}
	}
	t.Error("expected 'agent logs' command to be registered")
}

func TestAgentLogsCommandFlags(t *testing.T) {
	if agentLogsCmd.Flags().Lookup("lines") == nil {
		t.Error("expected --lines flag on agent logs command")
	}
}

func TestLogsCommandFlags(t *testing.T) {
	if logsCmd.Flags().Lookup("follow") == nil {
		t.Error("expected --follow flag on logs command")
	}
	if logsCmd.Flags().Lookup("component") == nil {
		t.Error("expected --component flag on logs command")
	}
}

// TestRunDaemonLogs_ComponentOrchestrator tails only the orchestrator
// log when --component=orch. The output is verbatim (no interleave
// prefix / timestamp).
func TestRunDaemonLogs_ComponentOrchestrator(t *testing.T) {
	dir := t.TempDir()
	orchLog := filepath.Join(dir, "orch.log")
	dashLog := filepath.Join(dir, "dash.log")
	if err := os.WriteFile(orchLog, []byte("orch-line-one\norch-line-two\n"), 0o644); err != nil {
		t.Fatalf("seed orch: %v", err)
	}
	if err := os.WriteFile(dashLog, []byte("dash-line-one\n"), 0o644); err != nil {
		t.Fatalf("seed dash: %v", err)
	}

	orchSpec := daemonize.Spec{Name: "orch", LogPath: orchLog}
	dashSpec := daemonize.Spec{Name: "dash", LogPath: dashLog}

	var buf strings.Builder
	if err := runDaemonLogsFor(context.Background(), false, "orch", &buf, orchSpec, dashSpec); err != nil {
		t.Fatalf("runDaemonLogsFor: %v", err)
	}
	if !strings.Contains(buf.String(), "orch-line-one") || strings.Contains(buf.String(), "dash-line-one") {
		t.Fatalf("component=orch output = %q; want orch lines only", buf.String())
	}
}

// TestRunDaemonLogs_UnknownComponent returns an error on bad input.
func TestRunDaemonLogs_UnknownComponent(t *testing.T) {
	orchSpec := daemonize.Spec{Name: "orch", LogPath: filepath.Join(t.TempDir(), "orch.log")}
	dashSpec := daemonize.Spec{Name: "dash", LogPath: filepath.Join(t.TempDir(), "dash.log")}
	err := runDaemonLogsFor(context.Background(), false, "nope", nil, orchSpec, dashSpec)
	if err == nil || !strings.Contains(err.Error(), "unknown component") {
		t.Fatalf("err = %v; want unknown component error", err)
	}
}

// TestRunDaemonLogs_InterleavedPrefixesBothStreams writes to both
// log files and asserts every line in the captured output carries
// the expected `[HH:MM:SS tag] ` prefix.
func TestRunDaemonLogs_InterleavedPrefixesBothStreams(t *testing.T) {
	dir := t.TempDir()
	orchLog := filepath.Join(dir, "orch.log")
	dashLog := filepath.Join(dir, "dash.log")
	if err := os.WriteFile(orchLog, []byte("orch-alpha\norch-beta\n"), 0o644); err != nil {
		t.Fatalf("seed orch: %v", err)
	}
	if err := os.WriteFile(dashLog, []byte("dash-alpha\ndash-beta\n"), 0o644); err != nil {
		t.Fatalf("seed dash: %v", err)
	}

	orchSpec := daemonize.Spec{Name: "orch", LogPath: orchLog}
	dashSpec := daemonize.Spec{Name: "dash", LogPath: dashLog}

	buf := &syncBuffer{}
	if err := runDaemonLogsFor(context.Background(), false, "", buf, orchSpec, dashSpec); err != nil {
		t.Fatalf("runDaemonLogsFor: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %q", len(lines), buf.String())
	}

	gotOrch := 0
	gotDash := 0
	for _, line := range lines {
		if !strings.Contains(line, "[") || !strings.Contains(line, "] ") {
			t.Fatalf("line %q missing [...] prefix", line)
		}
		if strings.Contains(line, "orch") {
			gotOrch++
		}
		if strings.Contains(line, "dash") {
			gotDash++
		}
	}
	if gotOrch != 2 || gotDash != 2 {
		t.Fatalf("orch/dash counts = %d/%d; want 2/2; output:\n%s", gotOrch, gotDash, buf.String())
	}
}

// TestRunDaemonLogs_InterleavedFollowStopsOnCtx asserts ctx cancel
// cleanly tears down both tailer goroutines.
func TestRunDaemonLogs_InterleavedFollowStopsOnCtx(t *testing.T) {
	dir := t.TempDir()
	orchSpec := daemonize.Spec{Name: "orch", LogPath: filepath.Join(dir, "orch.log")}
	dashSpec := daemonize.Spec{Name: "dash", LogPath: filepath.Join(dir, "dash.log")}

	// Seed both files so TailLogs doesn't sit in the "waiting for
	// file to appear" branch — that's a separate path with its own
	// tests in the daemonize package.
	if err := os.WriteFile(orchSpec.LogPath, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("seed orch: %v", err)
	}
	if err := os.WriteFile(dashSpec.LogPath, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("seed dash: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	buf := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runDaemonLogsFor(ctx, true, "", buf, orchSpec, dashSpec) }()

	// Give it a moment to read the seeds and start polling.
	time.Sleep(200 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("returned %v; want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not return after ctx cancel")
	}
}
