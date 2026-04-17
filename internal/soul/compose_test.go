package soul

import (
	"context"
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/memory"
)

func setupAgentWithMemory(t *testing.T, id string) (ctx context.Context, pool poolIface) {
	t.Helper()
	p, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(p); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx = context.Background()
	if _, err := p.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ($1, 's', 'w')
	`, id); err != nil {
		t.Fatal(err)
	}
	if err := CreateFromTemplate(ctx, p, id, "", Overrides{}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SeedDefaultBlocks(ctx, p, id, "I am careful with git."); err != nil {
		t.Fatal(err)
	}
	// Pin a user-facing fact so FetchForInjection includes it.
	if _, err := memory.Remember(ctx, p, memory.Memory{
		AgentID: id, Dimension: "user", Tier: "daily", Category: "preference",
		Title: "Prefers pt-BR", Body: "Operator writes in Portuguese.", Source: "operator", Pinned: true,
	}); err != nil {
		t.Fatal(err)
	}
	// A long_term row (not pinned) should also show up.
	if _, err := memory.Remember(ctx, p, memory.Memory{
		AgentID: id, Dimension: "agent", Tier: "long_term", Category: "project",
		Title: "Uses Postgres 5434", Body: "Local DB is port 5434.", Source: "agent",
	}); err != nil {
		t.Fatal(err)
	}
	return ctx, p
}

// poolIface is a minimal interface covering what Compose tests need,
// satisfied by *pgxpool.Pool.
type poolIface interface {
	Querier
	memory.Querier
}

func TestCompose_LayersInOrder(t *testing.T) {
	ctx, pool := setupAgentWithMemory(t, "compose-a")

	out, err := ComposeForAgent(ctx, pool, pool, "compose-a", EnvHints{
		Platform: "telegram",
		CWD:      "/home/otavio/code/maquinista",
	}, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Order: soul heading → core memory → relevant memories → env.
	idxIdentity := strings.Index(out, "# You are")
	idxCore := strings.Index(out, "## Core memory")
	idxRelevant := strings.Index(out, "## Relevant memories")
	idxEnv := strings.Index(out, "## Environment")

	for name, idx := range map[string]int{
		"identity":  idxIdentity,
		"core":      idxCore,
		"relevant":  idxRelevant,
		"env":       idxEnv,
	} {
		if idx < 0 {
			t.Errorf("section %s missing from composed output:\n%s", name, out)
		}
	}
	if !(idxIdentity < idxCore && idxCore < idxRelevant && idxRelevant < idxEnv) {
		t.Errorf("layers out of order: identity=%d core=%d relevant=%d env=%d",
			idxIdentity, idxCore, idxRelevant, idxEnv)
	}
	if !strings.Contains(out, "Prefers pt-BR") {
		t.Error("pinned archival fact missing from composition")
	}
	if !strings.Contains(out, "Uses Postgres 5434") {
		t.Error("long-term archival fact missing from composition")
	}
	if !strings.Contains(out, "Platform: telegram") {
		t.Error("env Platform missing")
	}
}

func TestCompose_AppliesModelGuidance(t *testing.T) {
	ctx, pool := setupAgentWithMemory(t, "compose-b")
	out, err := ComposeForAgent(ctx, pool, pool, "compose-b", EnvHints{}, "Use absolute paths; verify before edit.", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "## Model guidance") {
		t.Error("Model guidance section missing")
	}
	if !strings.Contains(out, "Use absolute paths") {
		t.Error("Model guidance body missing")
	}
}

func TestCompose_NoSoul_ReturnsEmpty(t *testing.T) {
	p, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// No agents row at all.
	out, err := ComposeForAgent(ctx, p, p, "nobody", EnvHints{}, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("expected empty output for missing soul, got %d bytes:\n%s", len(out), out)
	}
}
