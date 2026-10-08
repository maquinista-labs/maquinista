package pipeline

// EX-06 Telegram plumbing tests: every emission point must land exactly one
// agent_outbox row for the synthetic pipeline agent, riding the stock
// relay → binding → channel_deliveries path (ADR-0005).

import (
	"context"
	"fmt"
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

	Notifyf(context.Background(), pool, "this must not panic: %d", 42)
}

// TestNotify_MissingPipelineAgentFails: without migration 036's seed the
// FK fails — Notify surfaces the error (Notifyf logs it).
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
		{"approve", VerdictApprove, false, []string{"✅", "review approved (round 0)", "queued for merge", "reply `approve` here", "comment `approve` on the ticket issue"}},
		{"request-changes", VerdictRequestChanges, false, []string{"🔁", "the reviewer requested changes (round 0)", "fixer is picking it up", "No action needed"}},
		{"needs-human", VerdictNeedsHuman, false, []string{"🆘", "parked for you", "reply `approve` or `reject`", "comment the same on the ticket issue"}},
		{"round-cap", VerdictRequestChanges, true, []string{"🆘", "round cap (3 rounds without an approval)", "parked for you", "reply `approve` or `reject`"}},
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

			if err := verdictPass(ctx, pool, nil, parkFanout{}, 3, "sess", nil); err != nil {
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

			if err := verdictPass(ctx, pool, nil, parkFanout{}, 3, "sess", nil); err != nil {
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

	if err := verdictPass(ctx, pool, nil, parkFanout{}, 3, "sess", nil); err != nil {
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

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
		t.Fatalf("watchdogPass: %v", err)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
	}
	// MAQ-37 split: the 🆘 carries only the human sentence; the machine
	// note states what the freeze filter actually measured — parallel-bounds
	// silence on BOTH channels, not the old sequential-sounding "no outbox
	// activity for X past the Y spawn grace" (MAQ-36) — and lives in the
	// task_context ledger, pinned by TestWatchdog_RetireNoteStatesRealTrigger.
	for _, want := range []string{"🆘", "the reviewer went silent", "~30m with no activity", "fresh reviewer for the same round", "No action needed"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("summary %q missing %q", texts[0], want)
		}
	}
	// MAQ-37 AC1: no machine vocabulary in the headline — the watchdog
	// ledger note (ids, timings) went to task_context, not the topic.
	for _, banned := range []string{"watchdog:", "frozen", "30m0s", "spawn grace", "respawns in-round"} {
		if strings.Contains(texts[0], banned) {
			t.Errorf("summary %q still carries machine vocabulary %q", texts[0], banned)
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

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
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

	if err := watchdogPass(ctx, pool, 30*time.Minute, 10*time.Minute, 3, "sess", nil, parkFanout{}); err != nil {
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

// ---- MAQ-22 lifecycle one-liners ----

// TestReviewRound_NotifyClaimed: the reviewer spawn's round bump announces
// the claim exactly once — "task claimed (role + round)" for the reviewer
// leg of the journey.
func TestReviewRound_NotifyClaimed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tc", "uuid-c", "/tmp/wt")
	seedReviewer(t, pool, "reviewer-tc", "tc")

	if _, err := recordReviewRound(ctx, pool, nil, "reviewer-tc", "tc"); err != nil {
		t.Fatalf("recordReviewRound: %v", err)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
	}
	for _, want := range []string{"👀", "code review round 1", "task tc"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("claim note %q missing %q", texts[0], want)
		}
	}
	if strings.Contains(texts[0], "claimed") {
		t.Errorf("claim note %q still carries the internal 'reviewer claimed' jargon", texts[0])
	}

	// A second bump (the next round's reviewer) announces the NEW round.
	if _, err := recordReviewRound(ctx, pool, nil, "reviewer-tc", "tc"); err != nil {
		t.Fatalf("recordReviewRound 2: %v", err)
	}
	texts = pipelineNotifyTextsPool(t, pool)
	if len(texts) != 2 || !strings.Contains(texts[1], "code review round 2") {
		t.Fatalf("second claim = %d texts %q, want round 2 note", len(texts), texts)
	}
}

// TestFixEpisode_NotifyRoundStarted: the fix marker row is the exactly-once
// guard; the round-started one-liner rides it.
func TestFixEpisode_NotifyRoundStarted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tf", "uuid-f", "/tmp/wt")
	execOK(t, pool, `UPDATE tasks SET status='changes_requested' WHERE id='tf'`)
	seedReviewer(t, pool, "reviewer-tf", "tf")
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ('reviewer-tf', $1::jsonb)
	`, `{"text":"1. fix the bug\nVERDICT: request_changes\n"}`)
	// Retire the reviewer BEFORE the live fixer insert — uq_agents_task_live
	// allows one live agent per task; the findings outbox row survives.
	execOK(t, pool, `UPDATE agents SET status='dead' WHERE id='reviewer-tf'`)
	execOK(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
					    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('fixer-tf', 'sess', 'fixer-tf', 'fixer', 'tf', 'running', 'pi', '/tmp/wt', 'fixer-tf', NOW(), NOW(), FALSE)`)

	if err := recordFixEpisode(ctx, pool, "fixer-tf", "tf", 1, "reviewer-tf"); err != nil {
		t.Fatalf("recordFixEpisode: %v", err)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
	}
	for _, want := range []string{"🔧", "fixer round 1 started", "task tf"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("fix note %q missing %q", texts[0], want)
		}
	}
}

// ---- MAQ-37 pinned before/after examples ----

// TestNotifyRendering_HumanReadablePins (MAQ-37 AC5): pinned before/after
// examples. BEFORE is the retired Pipeline-topic shape — raw task UUID in
// the headline, the internal agent id echoed twice, Go durations ("30m0s"),
// watchdog jargon, no links:
//
//	🆘 [MAQ-34] Human-review parks must also comment... (caa43bb1-2bab-4ccf-baae-64b4c4daa55a):
//	watchdog: implementor implementor-caa43bb1-...-r4 frozen — no outbox
//	activity for 30m0s past the 10m0s spawn grace; 3 respawns already spent,
//	parking needs-human
//
// AFTER is what every emission point must render now: "[MAQ-n] <title>"
// headline, role prose, ~30m, plain sentence, issue + PR links.
func TestNotifyRendering_HumanReadablePins(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const (
		taskID    = "caa43bb1-2bab-4ccf-baae-64b4c4daa55a"
		issueID   = "d4c75be0-4029-4805-9937-c4a2b569e656"
		issueURL  = "https://linear.app/brisaai/issue/MAQ-34/human-review-parks"
		prURL     = "https://github.com/maquinista-labs/maquinista/pull/34"
		agentID   = "implementor-" + taskID + "-r4"
	)
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url, metadata)
		VALUES ($1, $2, 'pending_approval', $3, $4::jsonb)
	`, taskID, "[MAQ-34] Human-review parks must also comment on the ticket", prURL,
		`{"ticket_issue_id":"`+issueID+`","ticket_url":"`+issueURL+`"}`)
	execOK(t, pool, `
		INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id)
		VALUES ($1, 'MAQ-34', 'brisaai', $2)
	`, issueID, taskID)

	// The AFTER shape, composed exactly the way the freeze arms do:
	// headline from TaskTitle, sentence from the human field, links ride
	// with NotifyTask's decoration.
	human := fmt.Sprintf("the implementor (round 4) hung 3 times (each ~%s with no activity) — the machine gave up and is waiting for you.", DurHuman(30*time.Minute))
	NotifyTaskf(ctx, pool, taskID, "🆘 %s: %s", TaskTitle(ctx, pool, taskID), human)

	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("outbox texts = %d rows, want exactly 1", len(texts))
	}
	text := texts[0]

	// AC1: the headline carries [MAQ-n] + title, never the raw uuid.
	if !strings.HasPrefix(text, "🆘 [MAQ-34] Human-review parks must also comment on the ticket: ") {
		t.Errorf("headline = %q, want a \"[MAQ-34] <title>\" prefix", text)
	}
	for _, want := range []string{"the implementor (round 4) hung 3 times", "~30m", "waiting for you"} {
		if !strings.Contains(text, want) {
			t.Errorf("note %q missing %q", text, want)
		}
	}
	// AC1/AC3: no full task uuid, no internal agent id, no machine timings
	// or watchdog jargon anywhere in the prose.
	for _, banned := range []string{taskID, agentID, "30m0s", "watchdog", "spawn grace", "parking needs-human", "frozen"} {
		if strings.Contains(text, banned) {
			t.Errorf("note %q carries machine vocabulary %q", text, banned)
		}
	}
	// AC2: links always — Linear issue + PR, one line each.
	for _, want := range []string{"🎫 Issue: " + issueURL, "🔗 PR: " + prURL} {
		if !strings.Contains(text, want) {
			t.Errorf("note %q missing link line %q", text, want)
		}
	}
	// MAQ-24 invariant intact: the machine task_id still rides the outbox
	// content so a Telegram reply lands as a PR comment.
	var outTaskID string
	if err := pool.QueryRow(ctx, `
		SELECT content->>'task_id' FROM agent_outbox WHERE agent_id = $1
	`, NotifyAgentID).Scan(&outTaskID); err != nil || outTaskID != taskID {
		t.Fatalf("outbox task_id = %q (err %v), want %q", outTaskID, err, taskID)
	}

	// Pure-render pins: durations read like prose, ids render as roles.
	for d, want := range map[time.Duration]string{
		30 * time.Minute: "30m",
		90 * time.Minute: "1h30m",
		2 * time.Hour:    "2h",
	} {
		if got := DurHuman(d); got != want {
			t.Errorf("DurHuman(%s) = %q, want %q", d, got, want)
		}
	}
	if got := roleHuman(agentID); got != "the implementor (round 4)" {
		t.Errorf("roleHuman = %q, want %q", got, "the implementor (round 4)")
	}
	if got := roleHuman("reviewer-tw"); got != "the reviewer" {
		t.Errorf("roleHuman = %q, want %q", got, "the reviewer")
	}
	if got := shortTaskID(taskID); got != "caa43bb1" {
		t.Errorf("shortTaskID = %q, want 8-char short form", got)
	}
	// Merged notes link the merge commit; no PR → bare sha; no sha → no line.
	if got, want := commitLinkSuffix(prURL, "abc1234"), "\n🔨 Merged as https://github.com/maquinista-labs/maquinista/commit/abc1234"; got != want {
		t.Errorf("commitLinkSuffix = %q, want %q", got, want)
	}
	if got, want := commitLinkSuffix("", "abc1234"), "\n🔨 Commit: abc1234"; got != want {
		t.Errorf("commitLinkSuffix = %q, want %q", got, want)
	}
	if got := commitLinkSuffix(prURL, ""); got != "" {
		t.Errorf("commitLinkSuffix = %q, want empty without a sha", got)
	}

	// Headline fallbacks: no issue mapping → the raw title (intake titles
	// already carry the [MAQ-n] prefix); no title at all → the short id.
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status) VALUES ('t-nomap', '[MAQ-99] No mapping', 'pending_approval')
	`)
	if got, want := TaskTitle(ctx, pool, "t-nomap"), "[MAQ-99] No mapping"; got != want {
		t.Errorf("TaskTitle(no mapping) = %q, want %q", got, want)
	}
	if got, want := TaskTitle(ctx, pool, taskID), "[MAQ-34] Human-review parks must also comment on the ticket"; got != want {
		t.Errorf("TaskTitle = %q, want the issue-key headline (no doubled brackets)", got)
	}
	execOK(t, pool, `INSERT INTO tasks (id, title, status) VALUES ('t-notitle', '', 'ready')`)
	if got := TaskTitle(ctx, pool, "t-notitle"); got != "t-notitl" {
		t.Errorf("TaskTitle(untitled) = %q, want the short-id fallback", got)
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
