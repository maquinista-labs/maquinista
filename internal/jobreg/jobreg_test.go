package jobreg

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('alpha','s','w'),('beta','s','w2')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool
}

func TestSchedule_AddListRm(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	_, err := AddSchedule(ctx, pool, Schedule{
		Name:   "daily-reel",
		Cron:   "0 8 * * *",
		AgentID: "alpha",
		Prompt: map[string]any{"type": "command", "text": "/publish-reel"},
	})
	if err != nil {
		t.Fatal(err)
	}

	list, err := ListSchedules(ctx, pool)
	if err != nil || len(list) != 1 || list[0].Name != "daily-reel" {
		t.Fatalf("list=%v err=%v", list, err)
	}

	if err := RmSchedule(ctx, pool, "daily-reel"); err != nil {
		t.Fatal(err)
	}
	list, _ = ListSchedules(ctx, pool)
	if len(list) != 0 {
		t.Errorf("after rm list=%v", list)
	}
}

func TestSchedule_RejectsBadCron(t *testing.T) {
	pool := setup(t)
	_, err := AddSchedule(context.Background(), pool, Schedule{
		Name:    "bad",
		Cron:    "not a cron",
		AgentID: "alpha",
		Prompt:  map[string]any{"text": "x"},
	})
	if err == nil {
		t.Fatal("expected rejection on bad cron")
	}
}

func TestHook_AddListEnableDisableRm(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if _, err := AddHook(ctx, pool, Hook{
		Name:           "gh-pr",
		Path:           "/hooks/gh/pr",
		Secret:         "s3cr3t",
		AgentID:        "beta",
		PromptTemplate: "/review-pr {{.number}}",
	}); err != nil {
		t.Fatal(err)
	}

	list, _ := ListHooks(ctx, pool)
	if len(list) != 1 || list[0].Name != "gh-pr" {
		t.Fatalf("list=%v", list)
	}

	if err := SetHookEnabled(ctx, pool, "gh-pr", false); err != nil {
		t.Fatal(err)
	}
	list, _ = ListHooks(ctx, pool)
	if list[0].Enabled {
		t.Error("expected disabled")
	}
	if err := SetHookEnabled(ctx, pool, "gh-pr", true); err != nil {
		t.Fatal(err)
	}
	if err := RmHook(ctx, pool, "gh-pr"); err != nil {
		t.Fatal(err)
	}
}

func TestHook_RejectsBadPath(t *testing.T) {
	pool := setup(t)
	_, err := AddHook(context.Background(), pool, Hook{
		Name:           "bad",
		Path:           "/nope",
		Secret:         "s",
		AgentID:        "alpha",
		PromptTemplate: "x",
	})
	if err == nil {
		t.Fatal("expected rejection on non /hooks/ path")
	}
}

// TestAddSchedule_SoulTemplateID: valid schedule with soul_template_id instead
// of agent_id passes validation and round-trips correctly.
func TestAddSchedule_SoulTemplateID(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Insert a soul_template row required by the FK.
	if _, err := pool.Exec(ctx, `
		INSERT INTO soul_templates (id, name, role, goal)
		VALUES ('tpl-test', 'Test', 'executor', 'do things')
	`); err != nil {
		t.Fatalf("seed soul_template: %v", err)
	}

	_, err := AddSchedule(ctx, pool, Schedule{
		Name:           "soul-job",
		Cron:           "0 8 * * *",
		SoulTemplateID: "tpl-test",
		Prompt:         map[string]any{"type": "text", "text": "run!"},
	})
	if err != nil {
		t.Fatalf("AddSchedule: %v", err)
	}

	list, err := ListSchedules(ctx, pool)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if list[0].SoulTemplateID != "tpl-test" {
		t.Errorf("SoulTemplateID=%q, want tpl-test", list[0].SoulTemplateID)
	}
	if list[0].SoulTemplateName != "Test" {
		t.Errorf("SoulTemplateName=%q, want Test", list[0].SoulTemplateName)
	}
}

// TestValidateSchedule_NeitherAgentNorTemplate: both empty → error.
func TestValidateSchedule_NeitherAgentNorTemplate(t *testing.T) {
	_, err := AddSchedule(context.Background(), nil, Schedule{
		Name:   "bad",
		Cron:   "0 8 * * *",
		Prompt: map[string]any{"text": "x"},
	})
	if err == nil {
		t.Fatal("expected error when neither agent_id nor soul_template_id set")
	}
}

// TestValidateSchedule_BothSet: having both agent_id and soul_template_id is OK.
func TestValidateSchedule_BothSet(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO soul_templates (id, name, role, goal)
		VALUES ('tpl-both', 'Both', 'executor', 'go')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := AddSchedule(ctx, pool, Schedule{
		Name:           "both-job",
		Cron:           "0 8 * * *",
		AgentID:        "alpha",
		SoulTemplateID: "tpl-both",
		Prompt:         map[string]any{"text": "x"},
	})
	if err != nil {
		t.Fatalf("expected no error with both set: %v", err)
	}
}

func TestReconcile_UpsertsAndSoftDisablesStale(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	schedDir := filepath.Join(dir, "schedules")
	hookDir := filepath.Join(dir, "hooks")
	os.MkdirAll(schedDir, 0o755)
	os.MkdirAll(hookDir, 0o755)

	os.WriteFile(filepath.Join(schedDir, "a.yaml"), []byte(`
name: nightly
cron: "0 2 * * *"
agent_id: alpha
prompt:
  type: command
  text: /nightly
`), 0o644)

	os.WriteFile(filepath.Join(hookDir, "pr.yaml"), []byte(`
name: pr-opened
path: /hooks/github/pr
secret: s
agent_id: beta
prompt_template: "/review-pr {{.number}}"
`), 0o644)

	if err := Reconcile(ctx, pool, schedDir, hookDir); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	list, _ := ListSchedules(ctx, pool)
	if len(list) != 1 || list[0].Name != "nightly" {
		t.Fatalf("schedules=%v", list)
	}
	hooks, _ := ListHooks(ctx, pool)
	if len(hooks) != 1 || hooks[0].Name != "pr-opened" {
		t.Fatalf("hooks=%v", hooks)
	}

	// Remove the yaml files — reconcile should SOFT-disable, not delete.
	os.Remove(filepath.Join(schedDir, "a.yaml"))
	os.Remove(filepath.Join(hookDir, "pr.yaml"))
	if err := Reconcile(ctx, pool, schedDir, hookDir); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	list, _ = ListSchedules(ctx, pool)
	if len(list) != 1 || list[0].Enabled {
		t.Errorf("schedule should be soft-disabled, got %+v", list)
	}
	hooks, _ = ListHooks(ctx, pool)
	if len(hooks) != 1 || hooks[0].Enabled {
		t.Errorf("hook should be soft-disabled, got %+v", hooks)
	}
}
