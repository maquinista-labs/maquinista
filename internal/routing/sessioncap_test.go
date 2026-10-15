package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckSessionCap(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Three agents bound to user "capuser". live-1 starts live; the rest
	// are stopped. Subtests flip statuses as they go — order matters.
	exec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, status) VALUES
			('live-1','s','w1','running'),
			('live-2','s','w2','stopped'),
			('stopped-1','s','w3','stopped'),
			('dead-1','s','w4','dead')
	`)
	exec(t, pool, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id) VALUES
			(1, 'live-1',    'owner', 'capuser', '1', -1),
			(2, 'live-2',    'owner', 'capuser', '2', -2),
			(3, 'stopped-1', 'owner', 'capuser', '3', -3),
			(4, 'dead-1',    'owner', 'capuser', '4', -4)
	`)

	t.Run("under_cap_allows_spawn", func(t *testing.T) {
		// One live other + two stopped: a new pane stays under the cap.
		if err := CheckSessionCap(ctx, pool, "capuser", "incoming-1", 2); err != nil {
			t.Errorf("cap should allow spawn with 1 live other: %v", err)
		}
	})

	t.Run("at_cap_rejects_with_readable_error", func(t *testing.T) {
		// live-2 flips live → 2 live others ≥ cap 2: a new spawn rejects.
		exec(t, pool, `UPDATE agents SET status='idle' WHERE id='live-2'`)
		err := CheckSessionCap(ctx, pool, "capuser", "incoming-1", 2)
		var capErr *SessionCapError
		if !errors.As(err, &capErr) {
			t.Fatalf("err=%v, want *SessionCapError", err)
		}
		if capErr.Max != 2 || capErr.Count != 2 {
			t.Errorf("capErr=%+v, want Max=2 Count=2", capErr)
		}
		// Readability contract: mentions the count, the max, and a way out.
		msg := capErr.Error()
		for _, want := range []string{"2 live agents", "max 2", "/agent_kill"} {
			if !contains(msg, want) {
				t.Errorf("message %q missing %q", msg, want)
			}
		}
	})

	t.Run("respawn_excludes_own_row", func(t *testing.T) {
		// Re-adopting a live topic whose binding was lost must not count
		// itself: excluding live-1 leaves 1 live other < cap 2.
		err := CheckSessionCap(ctx, pool, "capuser", "live-1", 2)
		if err != nil {
			t.Errorf("respawn of own live row should be allowed: %v", err)
		}
	})

	t.Run("observer_bindings_do_not_count", func(t *testing.T) {
		exec(t, pool, `
			INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
			VALUES (5, 'live-1', 'observer', 'otheruser', '5', -5)
		`)
		n, err := CountLiveAgentsForUser(ctx, pool, "otheruser", "")
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("observer binding counted as ownership: n=%d", n)
		}
	})

	t.Run("disabled_cap_allows_everything", func(t *testing.T) {
		if err := CheckSessionCap(ctx, pool, "capuser", "", 0); err != nil {
			t.Errorf("max<=0 must disable the cap: %v", err)
		}
		if err := CheckSessionCap(ctx, pool, "capuser", "", -1); err != nil {
			t.Errorf("negative max must disable the cap: %v", err)
		}
	})

	t.Run("spawning_counts_as_live", func(t *testing.T) {
		exec(t, pool, `UPDATE agents SET status='spawning' WHERE id='stopped-1'`)
		// live-1 + live-2 + spawning stopped-1, incoming ≠ any → 3 ≥ 2 reject.
		err := CheckSessionCap(ctx, pool, "capuser", "incoming-x", 2)
		if !errors.As(err, new(*SessionCapError)) {
			t.Errorf("spawning rows must count toward the cap, err=%v", err)
		}
	})

	t.Run("unbound_user_has_zero_rows", func(t *testing.T) {
		n, err := CountLiveAgentsForUser(ctx, pool, "nobody", "")
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("n=%d, want 0", n)
		}
	})
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
