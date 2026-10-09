package routing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window) VALUES
			('alpha','s','wa'),
			('beta','s','wb'),
			('gamma','s','wg')
	`)
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func TestParseMention(t *testing.T) {
	cases := []struct {
		in       string
		wantID   string
		wantRest string
		wantOK   bool
	}{
		{"@alpha hello", "alpha", "hello", true},
		{"  @beta-42 with rest", "beta-42", "with rest", true},
		{"no mention here", "", "no mention here", false},
		{"@ alpha missing", "", "@ alpha missing", false},
		{"mid-text @alpha", "", "mid-text @alpha", false},
		{"@a-b_c", "a-b_c", "", true},
	}
	for _, c := range cases {
		id, rest, ok := ParseMention(c.in)
		if id != c.wantID || rest != c.wantRest || ok != c.wantOK {
			t.Errorf("ParseMention(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, id, rest, ok, c.wantID, c.wantRest, c.wantOK)
		}
	}
}

// countingSpawner returns a SpawnFunc that records how many times it's
// called and hands back a fixed id. Used to assert tier-3 behavior.
func countingSpawner(id string, counter *int64) SpawnFunc {
	return func(ctx context.Context, userID, threadID string, chatID *int64) (string, error) {
		atomic.AddInt64(counter, 1)
		return id, nil
	}
}

func TestResolve_AllTiers(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	t.Run("tier1_mention_resolves_by_id", func(t *testing.T) {
		res, err := Resolve(ctx, pool, nil, nil, "u1", "100", ptrInt64(-1001), "@beta hello")
		if err != nil {
			t.Fatal(err)
		}
		if res.Tier != TierMention || res.AgentID != "beta" || res.Text != "hello" {
			t.Errorf("got %+v", res)
		}
		// Mention must NOT write a binding.
		var count int
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM topic_agent_bindings WHERE user_id='u1' AND thread_id='100' AND binding_type='owner'`).Scan(&count)
		if count != 0 {
			t.Errorf("mention wrote %d owner rows, want 0", count)
		}
	})

	t.Run("tier1_mention_resolves_by_handle", func(t *testing.T) {
		// Give alpha a handle, then mention it via the handle.
		exec(t, pool, `UPDATE agents SET handle='researcher' WHERE id='alpha'`)
		res, err := Resolve(ctx, pool, nil, nil, "u1h", "101", ptrInt64(-1001), "@researcher hey")
		if err != nil {
			t.Fatal(err)
		}
		if res.Tier != TierMention || res.AgentID != "alpha" || res.Text != "hey" {
			t.Errorf("got %+v (want canonical id 'alpha')", res)
		}
	})

	t.Run("tier1_mention_unknown_token_passes_through", func(t *testing.T) {
		res, err := Resolve(ctx, pool, nil, nil, "u1x", "102", ptrInt64(-1001), "@nobody-here ?")
		if err != nil {
			t.Fatal(err)
		}
		if res.AgentID != "nobody-here" {
			t.Errorf("unknown mention should pass through raw token, got %q", res.AgentID)
		}
	})

	t.Run("tier2_owner_binding", func(t *testing.T) {
		exec(t, pool, `INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id) VALUES (200, 'alpha', 'owner', 'u2', '200', -2001)`)
		res, err := Resolve(ctx, pool, nil, nil, "u2", "200", ptrInt64(-2001), "plain text")
		if err != nil {
			t.Fatal(err)
		}
		if res.Tier != TierOwnerBinding || res.AgentID != "alpha" || res.Text != "plain text" {
			t.Errorf("got %+v", res)
		}
		if res.BindingSet {
			t.Error("should not re-write existing owner binding")
		}
	})

	t.Run("tier3_spawn_writes_owner", func(t *testing.T) {
		var calls int64
		spawn := countingSpawner("gamma", &calls)
		res, err := Resolve(ctx, pool, spawn, nil, "u3", "300", ptrInt64(-3001), "hi")
		if err != nil {
			t.Fatal(err)
		}
		if res.Tier != TierSpawn || res.AgentID != "gamma" {
			t.Errorf("got %+v", res)
		}
		if !res.BindingSet {
			t.Error("tier3 should write owner binding on first use")
		}
		if got := atomic.LoadInt64(&calls); got != 1 {
			t.Errorf("SpawnFunc called %d times, want 1", got)
		}
		// Second call now hits tier-2 and must NOT call SpawnFunc again.
		res2, err := Resolve(ctx, pool, spawn, nil, "u3", "300", ptrInt64(-3001), "again")
		if err != nil {
			t.Fatal(err)
		}
		if res2.Tier != TierOwnerBinding || res2.AgentID != "gamma" {
			t.Errorf("second call tier=%s agent=%s", res2.Tier, res2.AgentID)
		}
		if got := atomic.LoadInt64(&calls); got != 1 {
			t.Errorf("SpawnFunc re-called on second message: calls=%d", got)
		}
	})

	t.Run("tier4_picker_required_when_no_spawner", func(t *testing.T) {
		// Nil SpawnFunc → no tier matches → caller renders picker.
		_, err := Resolve(ctx, pool, nil, nil, "u4", "400", ptrInt64(-4001), "no binding")
		if !errors.Is(err, ErrRequirePicker) {
			t.Errorf("err=%v, want ErrRequirePicker", err)
		}
	})
}

func TestConfirmPickerChoice_WritesBinding(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	res, err := ConfirmPickerChoice(ctx, pool, nil, "u5", "500", ptrInt64(-5001), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != TierPicker || res.AgentID != "beta" || !res.BindingSet {
		t.Errorf("got %+v", res)
	}

	// Subsequent Resolve should go through tier-2.
	r2, err := Resolve(ctx, pool, nil, nil, "u5", "500", nil, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Tier != TierOwnerBinding || r2.AgentID != "beta" {
		t.Errorf("follow-up routing: %+v", r2)
	}
}

func TestSetUserDefault(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Attach to existing agent: succeeds.
	res, err := SetUserDefault(ctx, pool, nil, "u6", "600", ptrInt64(-6001), "alpha")
	if err != nil {
		t.Fatalf("set alpha: %v", err)
	}
	if res.AgentID != "alpha" || !res.BindingSet {
		t.Errorf("got %+v", res)
	}

	// Re-attach to a different existing agent: overwrites.
	if _, err := SetUserDefault(ctx, pool, nil, "u6", "600", ptrInt64(-6001), "beta"); err != nil {
		t.Fatalf("set beta: %v", err)
	}
	var agentID string
	pool.QueryRow(ctx, `SELECT agent_id FROM topic_agent_bindings WHERE binding_type='owner' AND user_id='u6' AND thread_id='600'`).Scan(&agentID)
	if agentID != "beta" {
		t.Errorf("agent=%q, want beta", agentID)
	}

	// Attach to an unknown token: returns ErrUnknownAgent; no binding written.
	_, err = SetUserDefault(ctx, pool, nil, "u7", "700", ptrInt64(-7001), "nobody")
	if !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("err=%v, want ErrUnknownAgent", err)
	}
	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM topic_agent_bindings WHERE user_id='u7' AND thread_id='700'`).Scan(&count)
	if count != 0 {
		t.Errorf("unknown attach wrote %d rows, want 0", count)
	}

	// Attach by handle.
	exec(t, pool, `UPDATE agents SET handle='pilot' WHERE id='alpha'`)
	res, err = SetUserDefault(ctx, pool, nil, "u8", "800", ptrInt64(-8001), "pilot")
	if err != nil {
		t.Fatalf("handle attach: %v", err)
	}
	if res.AgentID != "alpha" {
		t.Errorf("handle resolution got %q, want alpha", res.AgentID)
	}
}

func TestValidateHandle(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		label string
	}{
		{"researcher", true, "simple"},
		{"pilot-one", true, "dash"},
		{"pilot_one", true, "underscore"},
		{"Pilot", true, "mixed case (normalized to lowercase)"},
		{"p", false, "too short"},
		{"", false, "empty"},
		{"pilot!", false, "invalid char"},
		{"t-55", false, "reserved prefix"},
		{"T-Cap", false, "reserved prefix case-insensitive"},
		{"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", false, "too long"},
	}
	for _, c := range cases {
		err := ValidateHandle(c.in)
		got := err == nil
		if got != c.ok {
			t.Errorf("ValidateHandle(%q) ok=%v err=%v; want ok=%v (%s)",
				c.in, got, err, c.ok, c.label)
		}
	}
}

func TestSetHandle(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Happy path.
	if err := SetHandle(ctx, pool, "alpha", "Researcher"); err != nil {
		t.Fatalf("set handle: %v", err)
	}
	var handle string
	pool.QueryRow(ctx, `SELECT handle FROM agents WHERE id='alpha'`).Scan(&handle)
	if handle != "researcher" {
		t.Errorf("handle stored as %q, want lowercase 'researcher'", handle)
	}

	// Uniqueness: another agent can't take the same handle.
	err := SetHandle(ctx, pool, "beta", "researcher")
	if !errors.Is(err, ErrHandleTaken) {
		t.Errorf("err=%v, want ErrHandleTaken", err)
	}

	// Same agent re-setting its own handle is a no-op success.
	if err := SetHandle(ctx, pool, "alpha", "researcher"); err != nil {
		t.Errorf("self re-set failed: %v", err)
	}

	// Reserved prefix is rejected at validation.
	err = SetHandle(ctx, pool, "beta", "t-55")
	if err == nil {
		t.Error("reserved prefix should fail validation")
	}
}

// TestResolve_Tier3_RaceOneWinner: two goroutines resolve tier-3 for the
// same (user, thread). The partial unique index on topic_agent_bindings
// forces one writer, the other reads the committed row.
func TestResolve_Tier3_RaceOneWinner(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	var calls int64
	spawn := countingSpawner("gamma", &calls)

	var wg sync.WaitGroup
	results := make([]*Resolution, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := Resolve(ctx, pool, spawn, nil, "race", "777", ptrInt64(-7001), "go")
			if err != nil {
				t.Errorf("resolve %d: %v", i, err)
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	writers := 0
	for _, r := range results {
		if r != nil && r.BindingSet {
			writers++
		}
		if r != nil && r.AgentID != "gamma" {
			t.Errorf("agent=%q, want gamma", r.AgentID)
		}
	}
	if writers != 1 {
		t.Errorf("writers=%d, want exactly 1", writers)
	}

	// Exactly one owner row in the DB.
	var rows int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM topic_agent_bindings WHERE binding_type='owner' AND user_id='race' AND thread_id='777'`).Scan(&rows)
	if rows != 1 {
		t.Errorf("owner rows=%d, want 1", rows)
	}
}

func ptrInt64(v int64) *int64 { return &v }

// --- per-user repo policy (MAQ-45) -----------------------------------------

// forbidPolicy rejects every agent rooted outside allowRepo with an error
// wrapping ErrRepoForbidden — the shape production policies must return.
func forbidPolicy(allowRepo string) RepoPolicy {
	return func(ctx context.Context, userID, repoRoot string) error {
		if repoRoot == allowRepo {
			return nil
		}
		return fmt.Errorf("%w: user %s may not access %q", ErrRepoForbidden, userID, repoRoot)
	}
}

func TestResolve_RepoPolicy_Tier1Mention(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	exec(t, pool, `UPDATE agents SET workspace_repo_root='/srv/alpha' WHERE id='alpha'`)
	exec(t, pool, `UPDATE agents SET workspace_repo_root='/srv/beta'  WHERE id='beta'`)
	policy := forbidPolicy("/srv/alpha")

	// Allowed repo: normal resolution.
	res, err := Resolve(ctx, pool, nil, policy, "u9", "900", ptrInt64(-9001), "@alpha hi")
	if err != nil {
		t.Fatalf("allowed mention rejected: %v", err)
	}
	if res.AgentID != "alpha" {
		t.Errorf("agent=%q, want alpha", res.AgentID)
	}

	// Foreign repo: routing rejection, nothing delivered.
	_, err = Resolve(ctx, pool, nil, policy, "u9", "901", ptrInt64(-9001), "@beta hi")
	if !errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("err=%v, want ErrRepoForbidden", err)
	}

	// Unknown token still passes through untouched (no agent → no repo to
	// check; the caller renders "no such agent").
	res, err = Resolve(ctx, pool, nil, policy, "u9", "902", ptrInt64(-9001), "@nobody hi")
	if err != nil {
		t.Fatalf("unknown token should not hit policy: %v", err)
	}
	if res.AgentID != "nobody" {
		t.Errorf("agent=%q, want raw token nobody", res.AgentID)
	}
}

func TestResolve_RepoPolicy_Tier2Binding(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	exec(t, pool, `UPDATE agents SET workspace_repo_root='/srv/beta' WHERE id='beta'`)
	exec(t, pool, `INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id) VALUES (910, 'beta', 'owner', 'u10', '910', -9001)`)
	policy := forbidPolicy("/srv/alpha")

	_, err := Resolve(ctx, pool, nil, policy, "u10", "910", ptrInt64(-9001), "hi")
	if !errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("err=%v, want ErrRepoForbidden", err)
	}

	// Nil policy keeps the pre-MAQ-45 behavior: binding resolves.
	res, err := Resolve(ctx, pool, nil, nil, "u10", "910", ptrInt64(-9001), "hi")
	if err != nil || res.Tier != TierOwnerBinding || res.AgentID != "beta" {
		t.Errorf("nil policy: res=%+v err=%v", res, err)
	}
}

func TestResolve_RepoPolicy_Tier3SpawnNotBlocked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// Tier 3's repo is chosen by the SpawnFunc (which enforces the binding
	// itself), so the ladder must NOT reject a spawn just because a policy
	// is installed.
	var calls int64
	spawn := countingSpawner("gamma", &calls)
	res, err := Resolve(ctx, pool, spawn, forbidPolicy("/srv/alpha"), "u11", "911", ptrInt64(-9001), "hi")
	if err != nil {
		t.Fatalf("tier 3 blocked by ladder policy: %v", err)
	}
	if res.Tier != TierSpawn {
		t.Errorf("tier=%s, want spawn", res.Tier)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Errorf("spawner calls=%d, want 1", calls)
	}
}

func TestRepoPolicy_ExistingAgentsWithoutRepoRoot(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// setup() rows have no workspace_repo_root and no cwd: AgentRepoRoot
	// resolves to "". A real allowlist policy must fail closed — an agent
	// with an unresolvable repo is not automatically in-bounds.
	policy := func(ctx context.Context, userID, repoRoot string) error {
		if repoRoot == "/srv/real" {
			return nil
		}
		return fmt.Errorf("%w: root=%q", ErrRepoForbidden, repoRoot)
	}
	_, err := Resolve(ctx, pool, nil, policy, "u12", "913", ptrInt64(-9001), "@alpha hi")
	if !errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("agent without repo root must fail closed, got %v", err)
	}
}

func TestConfirmPickerChoice_RepoPolicy(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	exec(t, pool, `UPDATE agents SET workspace_repo_root='/srv/beta' WHERE id='beta'`)

	_, err := ConfirmPickerChoice(ctx, pool, forbidPolicy("/srv/alpha"), "u13", "913", ptrInt64(-9001), "beta")
	if !errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("err=%v, want ErrRepoForbidden", err)
	}
	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM topic_agent_bindings WHERE user_id='u13' AND thread_id='913'`).Scan(&count)
	if count != 0 {
		t.Errorf("forbidden pick wrote %d binding rows, want 0", count)
	}

	// Allowed repo binds normally.
	res, err := ConfirmPickerChoice(ctx, pool, forbidPolicy("/srv/beta"), "u13", "914", ptrInt64(-9001), "beta")
	if err != nil || !res.BindingSet {
		t.Errorf("allowed pick: res=%+v err=%v", res, err)
	}
}

func TestSetUserDefault_RepoPolicy(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	exec(t, pool, `UPDATE agents SET workspace_repo_root='/srv/alpha' WHERE id='alpha'`)

	_, err := SetUserDefault(ctx, pool, forbidPolicy("/srv/beta"), "u14", "914", ptrInt64(-9001), "alpha")
	if !errors.Is(err, ErrRepoForbidden) {
		t.Fatalf("err=%v, want ErrRepoForbidden", err)
	}
	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM topic_agent_bindings WHERE user_id='u14' AND thread_id='914'`).Scan(&count)
	if count != 0 {
		t.Errorf("forbidden attach wrote %d rows, want 0", count)
	}

	// Allowed attach still works under a policy.
	res, err := SetUserDefault(ctx, pool, forbidPolicy("/srv/alpha"), "u15", "915", ptrInt64(-9001), "alpha")
	if err != nil || res.AgentID != "alpha" {
		t.Errorf("allowed attach: res=%+v err=%v", res, err)
	}
}

func TestAgentRepoRoot(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	root, err := AgentRepoRoot(ctx, pool, "alpha")
	if err != nil || root != "" {
		t.Errorf("unset root = %q, %v; want empty", root, err)
	}

	exec(t, pool, `UPDATE agents SET cwd='/srv/via-cwd' WHERE id='beta'`)
	root, err = AgentRepoRoot(ctx, pool, "beta")
	if err != nil || root != "/srv/via-cwd" {
		t.Errorf("cwd fallback = %q, %v", root, err)
	}

	exec(t, pool, `UPDATE agents SET workspace_repo_root='/srv/explicit', cwd='/srv/via-cwd' WHERE id='gamma'`)
	root, err = AgentRepoRoot(ctx, pool, "gamma")
	if err != nil || root != "/srv/explicit" {
		t.Errorf("workspace_repo_root precedence = %q, %v", root, err)
	}

	root, err = AgentRepoRoot(ctx, pool, "no-such-agent")
	if err != nil || root != "" {
		t.Errorf("unknown agent = %q, %v; want empty, nil", root, err)
	}
}
