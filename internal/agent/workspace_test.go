package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceScope_Validate(t *testing.T) {
	cases := []struct {
		in      WorkspaceScope
		wantErr bool
	}{
		{ScopeShared, false},
		{ScopeAgent, false},
		{ScopeTask, false},
		{"", true},
		{"project", true},
		{"AGENT", true},
	}
	for _, c := range cases {
		err := c.in.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("Validate(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}

func TestResolveLayout_Shared(t *testing.T) {
	l, err := ResolveLayout(ScopeShared, "/tmp/proj", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.Scope != ScopeShared {
		t.Errorf("Scope = %q, want %q", l.Scope, ScopeShared)
	}
	if l.WorktreeDir != "" {
		t.Errorf("WorktreeDir = %q, want empty for shared", l.WorktreeDir)
	}
	if l.Branch != "" {
		t.Errorf("Branch = %q, want empty for shared", l.Branch)
	}
	if l.WindowCWD() != "/tmp/proj" {
		t.Errorf("WindowCWD() = %q, want %q", l.WindowCWD(), "/tmp/proj")
	}
}

func TestResolveLayout_Agent(t *testing.T) {
	l, err := ResolveLayout(ScopeAgent, "/tmp/proj", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.Scope != ScopeAgent {
		t.Errorf("Scope = %q, want %q", l.Scope, ScopeAgent)
	}
	want := filepath.Join("/tmp/proj", ".maquinista", "worktrees", "agent", "alice")
	if l.WorktreeDir != want {
		t.Errorf("WorktreeDir = %q, want %q", l.WorktreeDir, want)
	}
	if l.Branch != "maquinista/agent/alice" {
		t.Errorf("Branch = %q, want %q", l.Branch, "maquinista/agent/alice")
	}
	if l.WindowCWD() != want {
		t.Errorf("WindowCWD() = %q, want %q", l.WindowCWD(), want)
	}
}

func TestResolveLayout_Task(t *testing.T) {
	l, err := ResolveLayout(ScopeTask, "/tmp/proj", "", "t-42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.Scope != ScopeTask {
		t.Errorf("Scope = %q, want %q", l.Scope, ScopeTask)
	}
	want := filepath.Join("/tmp/proj", ".maquinista", "worktrees", "task", "t-42")
	if l.WorktreeDir != want {
		t.Errorf("WorktreeDir = %q, want %q", l.WorktreeDir, want)
	}
	if l.Branch != "maquinista/task/t-42" {
		t.Errorf("Branch = %q, want %q", l.Branch, "maquinista/task/t-42")
	}
}

func TestResolveLayout_MissingID(t *testing.T) {
	if _, err := ResolveLayout(ScopeAgent, "/tmp/proj", "", ""); err == nil {
		t.Error("ScopeAgent with empty agentID: expected error, got nil")
	}
	if _, err := ResolveLayout(ScopeTask, "/tmp/proj", "", ""); err == nil {
		t.Error("ScopeTask with empty taskID: expected error, got nil")
	}
}

func TestResolveLayout_MissingRepoRoot(t *testing.T) {
	if _, err := ResolveLayout(ScopeShared, "", "", ""); err == nil {
		t.Error("empty repoRoot: expected error, got nil")
	}
}

func TestResolveLayout_BadScope(t *testing.T) {
	if _, err := ResolveLayout("weird", "/tmp/proj", "alice", ""); err == nil {
		t.Error("unknown scope: expected error, got nil")
	}
}

func TestResolveLayout_RelativePath(t *testing.T) {
	// Relative repoRoot gets absolutized.
	l, err := ResolveLayout(ScopeAgent, ".", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(l.RepoRoot) {
		t.Errorf("RepoRoot = %q, want absolute path", l.RepoRoot)
	}
	if !strings.HasSuffix(l.WorktreeDir, filepath.Join(".maquinista", "worktrees", "agent", "alice")) {
		t.Errorf("WorktreeDir = %q, want suffix .maquinista/worktrees/agent/alice", l.WorktreeDir)
	}
}
