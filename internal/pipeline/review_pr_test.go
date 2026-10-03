package pipeline

// MAQ-16: the reviewer's verdict + findings surface on the PR as one
// `[review round N]` comment, and human PR comments newer than the previous
// round's reviewer feed the next round's prompt as verdict INPUT (never
// verbs). GitHub is faked at the fakeGh seam (merge_test.go); DB-backed
// tests pair it with a disposable Postgres like the dispatch tests.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func commentTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// ---- pure units ---------------------------------------------------------

func TestVerdictCommentBody(t *testing.T) {
	body := verdictCommentBody(2, VerdictRequestChanges, "1. missing tests\nVERDICT: request_changes")
	if !strings.HasPrefix(body, "[review round 2] VERDICT: request_changes\n") {
		t.Errorf("body missing marker+verdict header: %q", body)
	}
	if !strings.Contains(body, "1. missing tests") {
		t.Errorf("body missing findings: %q", body)
	}
	// Empty findings degrade to a placeholder — the verdict line still ships.
	degraded := verdictCommentBody(1, VerdictApprove, "  ")
	if !strings.Contains(degraded, "VERDICT: approve") || !strings.Contains(degraded, "(findings unavailable)") {
		t.Errorf("degraded body = %q", degraded)
	}
}

func TestReviewCommentMarker_NoCrossRoundCollision(t *testing.T) {
	if strings.Contains(reviewCommentMarker(12), reviewCommentMarker(1)) {
		t.Errorf("marker 1 collides with marker 12")
	}
	if strings.Contains(reviewCommentMarker(1), reviewCommentMarker(12)) {
		t.Errorf("marker 12 collides with marker 1")
	}
}

func TestRenderHumanComments_Filters(t *testing.T) {
	cutoff := commentTime("2026-10-03T10:00:00Z")
	comments := []PRComment{
		{Author: "old-human", Body: "stale feedback", CreatedAt: commentTime("2026-10-03T09:00:00Z")},                   // before cutoff
		{Author: "at-human", Body: "exactly at cutoff", CreatedAt: cutoff},                                              // not After(cutoff)
		{Author: "alice", Body: "please add tests", CreatedAt: commentTime("2026-10-03T10:30:00Z")},                     // in
		{Author: "ci-bot", IsBot: true, Body: "build failed", CreatedAt: commentTime("2026-10-03T10:31:00Z")},           // bot
		{Author: "reviewer", Body: "[review round 1] VERDICT: approve", CreatedAt: commentTime("2026-10-03T10:32:00Z")}, // ours
		{Author: "bob", Body: "  ", CreatedAt: commentTime("2026-10-03T10:33:00Z")},                                     // blank
	}
	out := renderHumanComments(comments, &cutoff)
	if !strings.Contains(out, "alice") || !strings.Contains(out, "please add tests") {
		t.Errorf("kept comment missing: %q", out)
	}
	if strings.Contains(out, "stale feedback") {
		t.Errorf("pre-cutoff comment leaked: %q", out)
	}
	if strings.Contains(out, "exactly at cutoff") {
		t.Errorf("cutoff boundary is inclusive, want exclusive: %q", out)
	}
	if strings.Contains(out, "build failed") {
		t.Errorf("bot comment leaked: %q", out)
	}
	if strings.Contains(out, "[review round 1]") {
		t.Errorf("our own round comment leaked: %q", out)
	}
	if !strings.Contains(out, "INPUT ONLY") {
		t.Errorf("missing input-not-verbs framing: %q", out)
	}
}

func TestRenderHumanComments_NoCutoff_KeepsAll(t *testing.T) {
	comments := []PRComment{
		{Author: "old-human", Body: "stale feedback", CreatedAt: commentTime("2026-10-03T09:00:00Z")},
		{Author: "alice", Body: "please add tests", CreatedAt: commentTime("2026-10-03T10:30:00Z")},
	}
	out := renderHumanComments(comments, nil)
	if !strings.Contains(out, "stale feedback") || !strings.Contains(out, "please add tests") {
		t.Errorf("round 1 must see every human comment: %q", out)
	}
}

func TestRenderHumanComments_Empty(t *testing.T) {
	if got := renderHumanComments(nil, nil); got != "" {
		t.Errorf("nil comments = %q, want empty", got)
	}
	if got := renderHumanComments([]PRComment{{Author: "bot", IsBot: true, Body: "x"}}, nil); got != "" {
		t.Errorf("bot-only = %q, want empty", got)
	}
}

func TestRenderHumanComments_OverflowKeepsNewest(t *testing.T) {
	// Five ~850-char comments overflow the 4000 section cap — the OLDEST
	// whole comment must be dropped, the newest kept.
	mk := func(name, created string) PRComment {
		return PRComment{Author: name, Body: strings.Repeat("x", 800), CreatedAt: commentTime(created)}
	}
	comments := []PRComment{
		mk("c1", "2026-10-03T10:00:00Z"),
		mk("c2", "2026-10-03T10:05:00Z"),
		mk("c3", "2026-10-03T10:10:00Z"),
		mk("c4", "2026-10-03T10:15:00Z"),
		mk("c5", "2026-10-03T10:20:00Z"),
	}
	out := renderHumanComments(comments, nil)
	if strings.Contains(out, "c1") {
		t.Errorf("overflow kept the oldest comment: %q", out)
	}
	if !strings.Contains(out, "c5") || !strings.Contains(out, "c4") {
		t.Errorf("overflow dropped newest comments: %q", out)
	}
}

func TestReviewPromptBody_IncludesHumanComments(t *testing.T) {
	base := reviewPromptBody("tp", 1, "")
	if strings.Contains(base, "Human comments") {
		t.Errorf("empty section leaked the header: %q", base)
	}
	with := reviewPromptBody("tp", 2, "Human comments on the PR ...\n\n- alice: fix it")
	if !strings.Contains(with, "fix it") || !strings.Contains(with, "VERDICT: approve") {
		t.Errorf("prompt lost comments or the verdict contract: %q", with)
	}
}

// ---- DB-backed: verdict → PR comment ------------------------------------

// seedPRReview: a review task with a PR URL, one review round recorded, a
// live reviewer whose newest outbox row carries findings + verdict — the
// exact state the verdict pass consumes.
func seedPRReview(t *testing.T, pool *pgxpool.Pool, taskID, verdict string) {
	t.Helper()
	seedReviewTask(t, pool, taskID, "uuid-"+taskID, "/tmp/wt-"+taskID)
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1,
		pr_url = $2 WHERE id = $1`, taskID, "https://github.com/acme/repo/pull/7")
	seedReviewer(t, pool, "reviewer-"+taskID, taskID)
	execOK(t, pool, `
		INSERT INTO agent_outbox (agent_id, content)
		VALUES ($1, $2::jsonb)
	`, "reviewer-"+taskID, `{"text":"1. missing tests\nVERDICT: `+verdict+`\n"}`)
}

// inboxPrompts returns the review prompts enqueued for the agent.
func inboxPrompts(t *testing.T, pool *pgxpool.Pool, agentID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT content->>'prompt' FROM agent_inbox WHERE agent_id = $1 AND content->>'type' = 'review'`, agentID)
	if err != nil {
		t.Fatalf("inbox prompts: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan prompt: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// TestVerdictPass_PostsRoundCommentOnce is AC 1 + AC 4: the applied verdict
// lands on the PR as exactly one `[review round N]` comment carrying the
// verdict line and the findings — and the transition is unaffected.
func TestVerdictPass_PostsRoundCommentOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPRReview(t, pool, "tpc", VerdictRequestChanges)
	fg := &fakeGh{}

	if err := verdictPass(ctx, pool, fg, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if got := taskCol(t, pool, "tpc", "status"); got != "changes_requested" {
		t.Fatalf("status = %q, want changes_requested (comment must not block the verdict)", got)
	}
	if len(fg.postedBodies) != 1 {
		t.Fatalf("posted = %d, want 1", len(fg.postedBodies))
	}
	body := fg.postedBodies[0]
	for _, want := range []string{"[review round 1]", "VERDICT: request_changes", "1. missing tests"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment body missing %q: %q", want, body)
		}
	}

	// A second pass is a strict no-op everywhere: the reviewer is retired,
	// no repost (round-marker dedup keeps crash retries at exactly one).
	if err := verdictPass(ctx, pool, fg, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass 2: %v", err)
	}
	if len(fg.postedBodies) != 1 {
		t.Fatalf("posted after 2nd pass = %d, want 1", len(fg.postedBodies))
	}
}

// TestVerdictPass_RoundNumberInComment: a round-2 verdict (after a fixer
// requeue bumps review_rounds) comments with the round-2 marker.
func TestVerdictPass_RoundNumberInComment(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPRReview(t, pool, "tpr2", VerdictApprove)
	execOK(t, pool, `UPDATE tasks SET review_rounds = 2 WHERE id = 'tpr2'`)
	fg := &fakeGh{}

	if err := verdictPass(ctx, pool, fg, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if len(fg.postedBodies) != 1 || !strings.HasPrefix(fg.postedBodies[0], "[review round 2] VERDICT: approve") {
		t.Fatalf("posted = %v, want one [review round 2] comment", fg.postedBodies)
	}
}

// TestVerdictPass_DedupsExistingRoundComment: a crash after a successful
// post but before the transition re-parses the same verdict next tick — the
// pre-existing `[review round N]` body suppresses the repost (AC 1 dedup)
// while the verdict still transitions.
func TestVerdictPass_DedupsExistingRoundComment(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPRReview(t, pool, "tpd", VerdictApprove)
	fg := &fakeGh{comments: []PRComment{
		{Author: "acme-ci", IsBot: true, Body: "[review round 1] VERDICT: approve\n\n(pre-existing)"},
	}}

	if err := verdictPass(ctx, pool, fg, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if len(fg.postedBodies) != 0 {
		t.Fatalf("posted = %v, want none (dedup by round marker)", fg.postedBodies)
	}
	if got := taskCol(t, pool, "tpd", "status"); got != "ready_to_merge" {
		t.Fatalf("status = %q, want ready_to_merge", got)
	}
}

// TestVerdictPass_NoPR_SkipsComment: tasks without a pr_url (or with a
// non-GitHub URL) skip the GitHub surface entirely.
func TestVerdictPass_NoPR_SkipsComment(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPRReview(t, pool, "tpn", VerdictApprove)
	execOK(t, pool, `UPDATE tasks SET pr_url = NULL WHERE id = 'tpn'`)
	fg := &fakeGh{}

	if err := verdictPass(ctx, pool, fg, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if len(fg.postedBodies) != 0 {
		t.Fatalf("posted = %v, want none (no PR)", fg.postedBodies)
	}
	if got := taskCol(t, pool, "tpn", "status"); got != "ready_to_merge" {
		t.Fatalf("status = %q, want ready_to_merge", got)
	}
}

// TestVerdictPass_GhOutage_StillTransitions is AC 3: every GitHub failure —
// comment list, post — logs and degrades to today's behavior; the verdict
// is still parsed and the task still transitions.
func TestVerdictPass_GhOutage_StillTransitions(t *testing.T) {
	cases := []struct {
		name string
		fg   *fakeGh
	}{
		{"post fails", &fakeGh{postErr: errors.New("gh: HTTP 502")}},
		{"comment list fails", &fakeGh{commentsErr: errors.New("gh: connection refused")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			seedPRReview(t, pool, "tpo-"+strings.ReplaceAll(c.name, " ", "-"), VerdictApprove)

			if err := verdictPass(ctx, pool, c.fg, 3, "sess", nil); err != nil {
				t.Fatalf("verdictPass: %v", err)
			}
			if got := taskCol(t, pool, "tpo-"+strings.ReplaceAll(c.name, " ", "-"), "status"); got != "ready_to_merge" {
				t.Fatalf("status = %q, want ready_to_merge (gh outage must not block)", got)
			}
		})
	}
}

// ---- DB-backed: human comments → round prompt ---------------------------

// TestDispatch_PromptCarriesHumanPRComments is AC 2: a human comment added
// between rounds (after round 1's reviewer started) appears in round 2's
// prompt; pre-cutoff comments, bots, and our own round comments don't.
func TestDispatch_PromptCarriesHumanPRComments(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tph", "uuid-ph", "/tmp/wt-tph")
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1,
		pr_url = 'https://github.com/acme/repo/pull/9' WHERE id = 'tph'`)
	// Round 1's reviewer: started 1h ago — the cutoff for round 2's prompt.
	// Comment times are relative to now so the test is clock-independent.
	seedReviewer(t, pool, "reviewer-tph", "tph")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - interval '1 hour' WHERE id = 'reviewer-tph'`)
	// Round 1's reviewer is retired; the spawn pass mints reviewer-tph-r2.
	execOK(t, pool, `UPDATE agents SET status = 'dead' WHERE id = 'reviewer-tph'`)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tph', 'impl-tph', 'result', 'done')
	`)

	pre := time.Now().Add(-90 * time.Minute)  // before the round-1 reviewer started
	post := time.Now().Add(-30 * time.Minute) // between the rounds
	fg := &fakeGh{comments: []PRComment{
		{Author: "early-bird", Body: "pre-round feedback", CreatedAt: pre},
		{Author: "alice", Body: "between-rounds: cover the empty case", CreatedAt: post},
		{Author: "ci-bot", IsBot: true, Body: "coverage dropped", CreatedAt: post},
		{Author: "reviewer", Body: "[review round 1] VERDICT: request_changes", CreatedAt: post},
	}}

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].AgentID != "reviewer-tph-r2" {
		t.Fatalf("spawns = %+v, want reviewer-tph-r2", sp.spawns)
	}
	prompts := inboxPrompts(t, pool, "reviewer-tph-r2")
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	prompt := prompts[0]
	if !strings.Contains(prompt, "between-rounds: cover the empty case") {
		t.Errorf("between-rounds human comment missing from the prompt: %q", prompt)
	}
	if strings.Contains(prompt, "pre-round feedback") {
		t.Errorf("pre-cutoff comment leaked into the prompt: %q", prompt)
	}
	if strings.Contains(prompt, "coverage dropped") || strings.Contains(prompt, "[review round 1]") {
		t.Errorf("bot/own comment leaked into the prompt: %q", prompt)
	}
}

// TestDispatch_PromptRound1_NoCutoff: with no prior reviewer row, every
// human comment on the PR (the PR-open baseline) feeds the round-1 prompt.
func TestDispatch_PromptRound1_NoCutoff(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tph1", "uuid-ph1", "/tmp/wt-tph1")
	execOK(t, pool, `UPDATE tasks SET pr_url = 'https://github.com/acme/repo/pull/9' WHERE id = 'tph1'`)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tph1', 'impl-tph1', 'result', 'done')
	`)
	fg := &fakeGh{comments: []PRComment{
		{Author: "early-bird", Body: "day-one feedback", CreatedAt: commentTime("2026-10-01T09:00:00Z")},
	}}

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	prompts := inboxPrompts(t, pool, "reviewer-tph1")
	if len(prompts) != 1 || !strings.Contains(prompts[0], "day-one feedback") {
		t.Fatalf("round-1 prompt must carry all human comments: %q", prompts)
	}
}

// TestPromptHeal_CarriesHumanComments: the heal path shares
// enqueueReviewPrompt, so a healed prompt carries the same human-comment
// section (no divergence between spawn-time and heal-time prompts).
func TestPromptHeal_CarriesHumanComments(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tphh", "uuid-phh", "/tmp/wt-tphh")
	execOK(t, pool, `UPDATE tasks SET review_rounds = 1,
		pr_url = 'https://github.com/acme/repo/pull/9' WHERE id = 'tphh'`)
	seedReviewer(t, pool, "reviewer-tphh", "tphh")
	fg := &fakeGh{comments: []PRComment{
		{Author: "alice", Body: "healed-prompt feedback", CreatedAt: commentTime("2026-10-03T12:00:00Z")},
	}}

	if err := promptPass(ctx, pool, fg); err != nil {
		t.Fatalf("promptPass: %v", err)
	}
	prompts := inboxPrompts(t, pool, "reviewer-tphh")
	if len(prompts) != 1 || !strings.Contains(prompts[0], "healed-prompt feedback") {
		t.Fatalf("healed prompt must carry human comments: %q", prompts)
	}
}

// TestPromptBuild_GhOutage_ShipsPlainPrompt is AC 3 on the prompt side: a
// failing PRComments call yields the standard prompt (no section, no error).
func TestPromptBuild_GhOutage_ShipsPlainPrompt(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedReviewTask(t, pool, "tpg", "uuid-pg", "/tmp/wt-tpg")
	execOK(t, pool, `UPDATE tasks SET pr_url = 'https://github.com/acme/repo/pull/9' WHERE id = 'tpg'`)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ('tpg', 'impl-tpg', 'result', 'done')
	`)
	fg := &fakeGh{commentsErr: errors.New("gh: rate limited")}

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	prompts := inboxPrompts(t, pool, "reviewer-tpg")
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	if strings.Contains(prompts[0], "Human comments") {
		t.Errorf("outage must ship the prompt without the section: %q", prompts[0])
	}
	// The plain briefing still carries the verdict contract.
	if !strings.Contains(prompts[0], "VERDICT: approve") {
		t.Errorf("plain prompt lost the verdict contract: %q", prompts[0])
	}
}

// TestNilGh_DisablesSurface: cfg.Gh nil (GitHub optional) skips both flows
// silently — the historical behavior.
func TestNilGh_DisablesSurface(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPRReview(t, pool, "tpz", VerdictApprove)

	if err := verdictPass(ctx, pool, nil, 3, "sess", nil); err != nil {
		t.Fatalf("verdictPass: %v", err)
	}
	if got := taskCol(t, pool, "tpz", "status"); got != "ready_to_merge" {
		t.Fatalf("status = %q, want ready_to_merge", got)
	}
}
