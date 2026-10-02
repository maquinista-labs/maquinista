package soul

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// pipelineTemplateIDs is the exact template set EX-02 seeds.
var pipelineTemplateIDs = []string{
	"pipeline-worker",
	"pipeline-reviewer",
	"pipeline-arbiter",
	"pipeline-fixer",
	"pipeline-merger",
}

// legacyTemplateIDs are the pre-existing templates EX-02 must not touch.
var legacyTemplateIDs = []string{"coder", "coordinator", "default", "planner"}

func newPipelinePool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return context.Background(), pool
}

func mustLoadTemplate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) *Template {
	t.Helper()
	tpl, err := LoadTemplate(ctx, pool, id)
	if err != nil {
		t.Fatalf("load template %s: %v", id, err)
	}
	return tpl
}

// renderPipelineTemplate inserts an agents row, clones the template for it,
// loads the clone back and renders it uncapped — the realistic spawn path.
func renderPipelineTemplate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, templateID, agentID string) string {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ($1, 's', 'w')
	`, agentID); err != nil {
		t.Fatalf("seed agent %s: %v", agentID, err)
	}
	if err := CreateFromTemplate(ctx, pool, agentID, templateID, Overrides{}); err != nil {
		t.Fatalf("clone %s: %v", templateID, err)
	}
	s, err := Load(ctx, pool, agentID)
	if err != nil {
		t.Fatalf("load clone %s: %v", agentID, err)
	}
	return Render(*s, 0)
}

// --- S1: seed migration ---

func TestMigration035_Seed(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	for _, id := range pipelineTemplateIDs {
		tpl := mustLoadTemplate(t, ctx, pool, id)
		if tpl.IsDefault {
			t.Errorf("%s: is_default = true, want false", id)
		}
		if tpl.Role == "" || tpl.Goal == "" || tpl.CoreTruths == "" || tpl.Boundaries == "" {
			t.Errorf("%s: empty contract fields (role/goal/core_truths/boundaries)", id)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM soul_templates WHERE id LIKE 'pipeline-%'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(pipelineTemplateIDs) {
		t.Errorf("pipeline templates = %d, want %d", n, len(pipelineTemplateIDs))
	}
}

func TestMigration035_Idempotent(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	snapshot := func() ([]string, int) {
		rows, err := pool.Query(ctx, `
			SELECT id || '|' || name || '|' || role || '|' || goal
			FROM soul_templates WHERE id = ANY($1) ORDER BY id
		`, legacyTemplateIDs)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		var total int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM soul_templates`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		return got, total
	}

	before, totalBefore := snapshot()

	sql, err := os.ReadFile("../db/migrations/035_seed_pipeline_souls.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	after, totalAfter := snapshot()
	if totalAfter != totalBefore {
		t.Errorf("row count after re-apply = %d, want %d", totalAfter, totalBefore)
	}
	if len(before) != len(after) {
		t.Fatalf("legacy snapshot size changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("legacy template %d changed on re-apply:\nbefore %q\nafter  %q", i, before[i], after[i])
		}
	}
}

func TestPipelineTemplateClone(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	tpl := mustLoadTemplate(t, ctx, pool, "pipeline-worker")

	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ('pw-clone', 's', 'w')
	`); err != nil {
		t.Fatal(err)
	}
	if err := CreateFromTemplate(ctx, pool, "pw-clone", "pipeline-worker", Overrides{}); err != nil {
		t.Fatalf("clone: %v", err)
	}

	got, err := Load(ctx, pool, "pw-clone")
	if err != nil {
		t.Fatal(err)
	}
	if got.TemplateID != "pipeline-worker" {
		t.Errorf("template_id = %q, want pipeline-worker", got.TemplateID)
	}
	if got.Role != tpl.Role || got.Goal != tpl.Goal ||
		got.CoreTruths != tpl.CoreTruths || got.Boundaries != tpl.Boundaries {
		t.Error("clone does not carry template contract fields verbatim")
	}
	if got.Extras["default_runner"] != tpl.Extras["default_runner"] ||
		got.Extras["reasoning_class"] != tpl.Extras["reasoning_class"] {
		t.Errorf("extras not cloned: got %v, want %v", got.Extras, tpl.Extras)
	}
}

// --- S2: worker contract ---

func TestPipelineWorkerContract(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	tpl := mustLoadTemplate(t, ctx, pool, "pipeline-worker")
	out := renderPipelineTemplate(t, ctx, pool, "pipeline-worker", "pw-rend")

	// AC 4: spec-first rule with the validators and the spec path.
	for _, want := range []string{
		"validate_plan.py",
		"validate_checks.py",
		".specs/features/",
		"before any implementation code",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("worker prompt missing spec-first element %q", want)
		}
	}

	// AC 5: task tools + completion binding.
	for _, want := range []string{
		"maquinista-done",
		"maquinista-observe",
		"maquinista-handoff",
		"check_commit.py",
		"runs green at HEAD",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("worker prompt missing completion element %q", want)
		}
	}

	// AC 6: boundaries — no out-of-task scope, no checks.md gaming.
	if !strings.Contains(tpl.Boundaries, "scope") {
		t.Error("worker boundaries missing scope restriction")
	}
	if !strings.Contains(tpl.Boundaries, "checks.md") {
		t.Error("worker boundaries missing checks.md no-gaming rule")
	}
}

// --- S3: reviewer + arbiter contract ---

func TestPipelineReviewerContract(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	tpl := mustLoadTemplate(t, ctx, pool, "pipeline-reviewer")
	out := renderPipelineTemplate(t, ctx, pool, "pipeline-reviewer", "pr-rend")

	// AC 7: review materials + no summary-trusting.
	for _, want := range []string{
		"plan.md",
		"checks.md",
		"verification.md",
		"full diff",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reviewer prompt missing review material %q", want)
		}
	}
	if !strings.Contains(tpl.Boundaries, "builder summary") {
		t.Error("reviewer boundaries missing builder-summary prohibition")
	}

	// AC 8: verdict vocabulary.
	for _, want := range []string{
		"VERDICT: approve",
		"VERDICT: request_changes",
		"VERDICT: needs_human",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reviewer prompt missing verdict line %q", want)
		}
	}
}

func TestPipelineArbiterContract(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	tpl := mustLoadTemplate(t, ctx, pool, "pipeline-arbiter")
	out := renderPipelineTemplate(t, ctx, pool, "pipeline-arbiter", "pa-rend")

	// AC 9: adjudication of contested/repeated request_changes, file:line evidence.
	if !strings.Contains(tpl.Goal, "request_changes") {
		t.Error("arbiter goal missing request_changes adjudication")
	}
	if !strings.Contains(tpl.Goal, "file:line") {
		t.Error("arbiter goal missing file:line evidence rule")
	}
	if !strings.Contains(out, "contested") {
		t.Error("arbiter prompt missing contested-review scope")
	}

	// AC 8: same verdict vocabulary.
	for _, want := range []string{
		"VERDICT: approve",
		"VERDICT: request_changes",
		"VERDICT: needs_human",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("arbiter prompt missing verdict line %q", want)
		}
	}
}

// --- S4: fixer + merger contract ---

func TestPipelineFixerContract(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	tpl := mustLoadTemplate(t, ctx, pool, "pipeline-fixer")
	out := renderPipelineTemplate(t, ctx, pool, "pipeline-fixer", "pf-rend")

	// AC 10: same worktree/PR, numbered findings, no scope creep,
	// no spec-obligation edits.
	if !strings.Contains(tpl.Goal, "worktree") || !strings.Contains(tpl.Goal, "PR") {
		t.Error("fixer goal missing same-worktree/PR scoping")
	}
	if !strings.Contains(out, "findings") {
		t.Error("fixer prompt missing findings list discipline")
	}
	if !strings.Contains(tpl.Boundaries, "scope") {
		t.Error("fixer boundaries missing no-scope-expansion rule")
	}
	if !strings.Contains(tpl.Boundaries, "checks.md") {
		t.Error("fixer boundaries missing spec-obligation no-edit rule")
	}
}

func TestPipelineMergerContract(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	tpl := mustLoadTemplate(t, ctx, pool, "pipeline-merger")
	out := renderPipelineTemplate(t, ctx, pool, "pipeline-merger", "pm-rend")

	// AC 11: rebase, gate, conflict handling.
	for _, want := range []string{
		"origin/main",
		"gh pr checks",
		"conflict file list",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("merger prompt missing merge element %q", want)
		}
	}

	// AC 12: proposal-then-wait is the stated default.
	if !strings.Contains(out, "merge proposal") {
		t.Error("merger prompt missing merge proposal behavior")
	}
	if !strings.Contains(tpl.Goal, "maquinista approve") {
		t.Error("merger goal missing maquinista approve wait")
	}
}

// --- S5: dispatch hints ---

func TestPipelineDispatchExtras(t *testing.T) {
	ctx, pool := newPipelinePool(t)

	wantReasoning := map[string]string{
		"pipeline-worker":   "standard",
		"pipeline-fixer":    "standard",
		"pipeline-merger":   "standard",
		"pipeline-reviewer": "high",
		"pipeline-arbiter":  "high",
	}

	for _, id := range pipelineTemplateIDs {
		tpl := mustLoadTemplate(t, ctx, pool, id)
		if got := tpl.Extras["default_runner"]; got != "pi" {
			t.Errorf("%s: default_runner = %q, want pi", id, got)
		}
		if got := tpl.Extras["reasoning_class"]; got != wantReasoning[id] {
			t.Errorf("%s: reasoning_class = %q, want %q", id, got, wantReasoning[id])
		}
	}
}
