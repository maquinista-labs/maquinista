package jobreg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestShippedHookYAML_Parses sanity-checks the YAML fixtures under
// config/hooks/ that Phase 3.5 ships. Parse failures surface here rather
// than at `maquinista start` boot.
func TestShippedHookYAML_Parses(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skip(err)
	}
	hookDir := filepath.Join(root, "config", "hooks")
	entries, err := os.ReadDir(hookDir)
	if err != nil {
		t.Skipf("no config/hooks: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("no hook yaml files")
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(hookDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var h Hook
		if err := yaml.Unmarshal(b, &h); err != nil {
			t.Errorf("%s unmarshal: %v", e.Name(), err)
			continue
		}
		// Enabled=false is fine for CHANGEME placeholders; validateHook
		// still runs structural checks.
		if err := validateHook(h); err != nil {
			t.Errorf("%s invalid: %v", e.Name(), err)
		}
	}
}

// TestShippedHookYAML_ReconcileAgainstRealDB reconciles the fixtures
// against a test Postgres — catches schema regressions against the
// actual jobreg CRUD path.
func TestShippedHookYAML_ReconcileAgainstDB(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skip(err)
	}
	pool := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window) VALUES
			('reviewer','s','w1'),
			('pr-closer','s','w2')
	`); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(ctx, pool, filepath.Join(root, "config", "schedules"), filepath.Join(root, "config", "hooks")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	hooks, _ := ListHooks(ctx, pool)
	if len(hooks) < 2 {
		t.Errorf("expected at least 2 shipped hooks, got %d", len(hooks))
	}
}

// repoRoot walks up from the test's cwd to the module root (go.mod).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", os.ErrNotExist
}
