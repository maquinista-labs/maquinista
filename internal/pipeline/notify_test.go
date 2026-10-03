package pipeline

// EX-06 Telegram plumbing tests: every emission point must land exactly one
// agent_outbox row for the synthetic pipeline agent, riding the stock
// relay → binding → channel_deliveries path (ADR-0005).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- notify core ----

func TestNotify_WritesPipelineOutbox(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := Notify(ctx, pool, "hello pipeline"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 || texts[0] != "hello pipeline" {
		t.Fatalf("outbox texts = %q, want one \"hello pipeline\"", texts)
	}
}

// TestNotifyf_SwallowsDeadPool: notification failures are logged, never
// escalated — a dead Telegram path must not fail the pipeline pass.
func TestNotifyf_SwallowsDeadPool(t *testing.T) {
	pool := testPool(t)
	pool.Close() // subsequent ops error

	notifyf(context.Background(), pool, "this must not panic: %d", 42)
}

// TestNotify_MissingPipelineAgentFails: without migration 036's seed the
// FK fails — Notify surfaces the error (notifyf logs it).
func TestNotify_MissingPipelineAgentFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	execOK(t, pool, `DELETE FROM agents WHERE id = $1`, NotifyAgentID)

	if err := Notify(ctx, pool, "nope"); err == nil {
		t.Fatal("Notify without the pipeline agent row must fail (FK), got nil")
	}
}

// ---- verdict summaries ----

func TestVerdict_NotifyPerOutcome(t *testing.T) {
	cases := []struct {
		name, verdict string
		bumpRounds    bool
		wantSubstr    []string
	}{
		{"approve", VerdictApprove, false, []string{"✅", "approved (review round 0)", "ready_to_merge", "reply `approve tv-appro`", "comment `approve` on the ticket issue"}},
		{"request-changes", VerdictRequestChanges, false, []string{"🔁", "request_changes (review round 0)", "fixer spawning"}},
		{"needs-human", VerdictNeedsHuman, false, []string{"🆘", "needs-human", "maquinista approve tv", "maquinista reject tv"}},
		{"round-cap", VerdictRequestChanges, true, []string{"🆘", "review round cap 3 reached", "parked needs-human"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			taskID := "tv-" + c.name
			seedReviewTask(t, pool, taskID, "uuid-"+c.name, "/tmp/wt")
			if c.bumpRounds {
				execOK(t, pool, `UPDATE tasks SET review_rounds = 3 WHERE id = $1`, taskID)
			}
			seedReviewer(t, pool, "reviewer-"+taskID, taskID)
			execOK(t, pool, `
				INSERT INTO agent_outbox (agent_id, content)
				VALUES ('reviewer-`+taskID+`', $1::jsonb)
			`, `{"text":"findings...\nVERDICT: `+c.verdict+`\n"}`)

			if err := verdictPass(ctx, pool, 3, "sess", nil); err != nil {
				t.Fatalf("verdictPass: %v", err)
			}

			texts := pipelineNotifyTextsPool(t, pool)
			if len(texts) != 1 {
				t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
			}
			text := texts[0]
			for _, want := range c.wantSubstr {
				if !strings.Contains(text, want) {
					t.Errorf("summary %q missing %q", text, want)
				}
			}
			// No pr_url seeded → no link fragment may appear (AC: graceful
			// degrade, no null/empty links).
			if strings.Contains(text, "🔗 PR") {
				t.Errorf("summary %q contains a PR link but the task has no pr_url", text)
			}
		})
	}
}

// TestVerdict_PRLinkInSummary (MAQ-10): every verdict branch for a task with
// a pr_url carries the link.
func TestVerdict_PRLinkInSummary(t *testing.T) {
	cases := []struct {
		name, verdict string
		bumpRounds    bool
	}{
		{"approve", VerdictApprove, false},
		{"request-changes", VerdictRequestChanges, false},
		{"needs-human", VerdictNeedsHuman, false},
		{"round-cap", VerdictRequestChanges, true},
	}
	const prURL = "https://github.com/maquinista-labs/maquinista/pull/42"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			taskID := "tvpr-" + c.name
			seedReviewTask(t, pool, taskID, "uuid-"+c.name, "/tmp/wt")
			execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`, taskID, prURL)
			if c.bumpRounds {
				execOK(t, pool, `UPDATE tasks SET review_rounds = 3 WHERE id = $1`, taskID)
			}
			seedReviewer(t, pool, "reviewer-"+taskID, taskID)
			execOK(t, pool, `
				INSERT INTO agent_outbox (agent_id, content)
				VALUES ('reviewer-`+taskID+`', $1::jsonb)
			`, `{"text":"findings...\nVERDICT: `+c.verdict+`\n"}`)

			if err := verdictPass(ctx, pool, 3, "sess", nil); err != nil {
				t.Fatalf("verdictPass: %v", err)
			}
			texts := pipelineNotifyTextsPool(t, pool)
			if len(texts) != 1 {
				t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
			}
			if !strings.Contains(texts[0], "\n🔗 PR: "+prURL) {
				t.Errorf("summary %q missing the PR link %q", texts[0], prURL)
			}
		})
	}
}

// TestVerdict_MalformedNoNotify: a malformed verdict must not notify —
// the emission sits inside the guarded transition.
func TestVerdict_MalformedNoNotify(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tm", "uuid-m", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tm", "tm")
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('reviewer-tm', '{"text":"VERDICT: approved-ish"}'::jsonb)
	`)

	if err := verdictPass(ctx, pool, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if texts := pipelineNotifyTextsPool(t, pool); len(texts) != 0 {
		t.Fatalf("malformed verdict notified: %q", texts)
	}
}

// ---- watchdog questions ----

func TestWatchdog_NotifyOnStall(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tw", "uuid-w", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tw", "tw")
	// Backdate past the stall bound (young-agent guard exempts fresh agents).
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='reviewer-tw'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
	}
	for _, want := range []string{"🆘", "watchdog: review stalled past 30m0s", "needs human"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("summary %q missing %q", texts[0], want)
		}
	}
}

// TestWatchdog_ActiveNoNotify: inside the timeout nothing happens at all.
func TestWatchdog_ActiveNoNotify(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "ta", "uuid-a", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-ta", "ta")
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content, created_at)
		VALUES ('reviewer-ta', '{"text":"thinking"}'::jsonb, NOW())
	`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	if texts := pipelineNotifyTextsPool(t, pool); len(texts) != 0 {
		t.Fatalf("active reviewer notified: %q", texts)
	}
}

// TestWatchdog_PRLink: a parked task with a PR gets the link in the
// needs-human question (MAQ-10).
func TestWatchdog_PRLink(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "twpr", "uuid-wpr", "/tmp/wt")
	execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`, "twpr",
		"https://github.com/maquinista-labs/maquinista/pull/7")
	seedReviewer(t, pool, "reviewer-twpr", "twpr")
	// Backdate past the stall bound: the young-agent guard exempts agents
	// younger than the timeout even with zero outbox activity (same shape
	// as TestWatchdog_StallTimeout).
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '31 minutes' WHERE id='reviewer-twpr'`)

	if err := watchdogPass(ctx, pool, 30*time.Minute, "sess", nil); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
	}
	if !strings.Contains(texts[0], "pull/7") {
		t.Errorf("watchdog summary %q missing the PR link", texts[0])
	}
}

// ---- helper ----

// pipelineNotifyTextsPool reads the pipeline agent's outbox texts
// (oldest first for deterministic indexing).
func pipelineNotifyTextsPool(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT content->>'text'
		FROM agent_outbox
		WHERE agent_id = $1
		ORDER BY created_at, id
	`, NotifyAgentID)
	if err != nil {
		t.Fatalf("query pipeline outbox: %v", err)
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		texts = append(texts, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return texts
}
