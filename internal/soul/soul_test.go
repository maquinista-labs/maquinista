package soul

import (
	"context"
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func TestMigration016_DefaultTemplateSeeded(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ctx := context.Background()
	tpl, err := LoadDefaultTemplate(ctx, pool)
	if err != nil {
		t.Fatalf("default template not seeded: %v", err)
	}
	if tpl.ID != "default" || !tpl.IsDefault {
		t.Errorf("got %+v, want id=default is_default=true", tpl)
	}
	if tpl.Name == "" || tpl.Role == "" || tpl.Goal == "" {
		t.Error("default template missing required fields")
	}
}

func TestCreateFromTemplate_ClonesDefault(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	// Need an agents row first — foreign key.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ('a1', 's', 'w1')
	`); err != nil {
		t.Fatal(err)
	}

	if err := CreateFromTemplate(ctx, pool, "a1", "", Overrides{}); err != nil {
		t.Fatalf("create from default: %v", err)
	}

	got, err := Load(ctx, pool, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TemplateID != "default" {
		t.Errorf("template_id = %q, want default", got.TemplateID)
	}
	if got.Name == "" || got.Role == "" {
		t.Error("identity fields empty after clone")
	}
	if got.MaxIter == 0 {
		t.Error("max_iter not cloned from template default")
	}
}

func TestCreateFromTemplate_AppliesOverrides(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ('a2', 's', 'w2')
	`); err != nil {
		t.Fatal(err)
	}

	name := "Alice"
	vibe := "Upbeat and fast."
	if err := CreateFromTemplate(ctx, pool, "a2", "", Overrides{
		Name: &name,
		Vibe: &vibe,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(ctx, pool, "a2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name {
		t.Errorf("name = %q, want %q", got.Name, name)
	}
	if got.Vibe != vibe {
		t.Errorf("vibe = %q, want %q", got.Vibe, vibe)
	}
	// Overrides didn't touch role/goal — those stay from template.
	if got.Role == "" || got.Goal == "" {
		t.Error("role/goal should come from template when not overridden")
	}
}

func TestCreateFromTemplate_NoDefault_FallsBackToAgentID(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	// Operator deleted the default template.
	if _, err := pool.Exec(ctx, `DELETE FROM soul_templates`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ('a3', 's', 'w3')
	`); err != nil {
		t.Fatal(err)
	}

	if err := CreateFromTemplate(ctx, pool, "a3", "", Overrides{}); err != nil {
		t.Fatalf("should fall through to empty soul, got err: %v", err)
	}

	got, err := Load(ctx, pool, "a3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a3" {
		t.Errorf("fallback name = %q, want agent id", got.Name)
	}
	if got.TemplateID != "" {
		t.Errorf("template_id should be empty when no template used, got %q", got.TemplateID)
	}
}

func TestUpsert_IncrementsVersion(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ('a4', 's', 'w4')
	`); err != nil {
		t.Fatal(err)
	}

	if err := CreateFromTemplate(ctx, pool, "a4", "", Overrides{}); err != nil {
		t.Fatal(err)
	}
	s1, _ := Load(ctx, pool, "a4")

	// Second upsert with a different vibe → version bumps.
	s2 := *s1
	s2.Vibe = "changed"
	if err := Upsert(ctx, pool, s2); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := Load(ctx, pool, "a4")
	if reloaded.Version <= s1.Version {
		t.Errorf("version = %d, want > %d", reloaded.Version, s1.Version)
	}
	if reloaded.Vibe != "changed" {
		t.Errorf("vibe = %q, want changed", reloaded.Vibe)
	}
}

func TestRender_ContainsRoleGoalAndSections(t *testing.T) {
	s := Soul{
		Name:       "Alice",
		Role:       "Reviewer",
		Goal:       "Keep the diff mergeable.",
		Tagline:    "Guardian of main.",
		CoreTruths: "- Be specific.",
		Boundaries: "- No force pushes.",
		Vibe:       "Terse.",
	}
	rendered := Render(s, 0)

	for _, want := range []string{"Alice", "Reviewer", "Keep the diff mergeable", "## Core truths", "## Boundaries", "## Vibe"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered output missing %q:\n%s", want, rendered)
		}
	}
}

func TestRender_EmptySectionsSkipped(t *testing.T) {
	s := Soul{
		Name: "Minimal",
		Role: "Agent",
		Goal: "Do the thing.",
	}
	rendered := Render(s, 0)

	for _, shouldNotAppear := range []string{"## Core truths", "## Boundaries", "## Vibe", "## Continuity"} {
		if strings.Contains(rendered, shouldNotAppear) {
			t.Errorf("empty section %q should be skipped:\n%s", shouldNotAppear, rendered)
		}
	}
}

func TestRender_TruncatesWithHeadTail(t *testing.T) {
	big := strings.Repeat("x", 200)
	s := Soul{
		Name:       "Long",
		Role:       "Dumper",
		Goal:       "Emit a lot.",
		CoreTruths: big,
	}
	rendered := Render(s, 120)
	if len(rendered) > 140 {
		// 120 target + marker slack.
		t.Errorf("truncated output longer than expected: %d bytes", len(rendered))
	}
	if !strings.Contains(rendered, "truncated") {
		t.Errorf("missing truncation marker in:\n%s", rendered)
	}
}
