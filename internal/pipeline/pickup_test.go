package pipeline

// MAQ-25: the PR pickup markers. Dispatch posts a one-line comment on the
// task's open PR at spawn time — reviewer rounds (🔁), fixer episodes (🔧,
// with the one-line reason distilled from the reviewer's findings) and the
// merge leg (🚀). Contract: exactly one comment per spawn (needle-scoped
// dedup), every GitHub failure only logs (never blocks the spawn), and
// tasks without an open PR skip the surface silently. GitHub is faked at
// the fakeGh seam (merge_test.go); DB-backed tests pair it with a
// disposable Postgres like the dispatch tests.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- pure units ---------------------------------------------------------

func TestPickupBodies(t *testing.T) {
	if got := reviewPickupBody("MAQ-25", 2); got != "🔁 [MAQ-25] review round 2 started" {
		t.Errorf("review pickup body = %q", got)
	}
	if got := fixerPickupBody("MAQ-25", 2, "1) tests missing"); got != "🔧 [MAQ-25] fixer round 2 picked this up - 1) tests missing" {
		t.Errorf("fixer pickup body = %q", got)
	}
	if got := mergePickupBody("MAQ-25"); got != "🚀 [MAQ-25] merge gate running" {
		t.Errorf("merge pickup body = %q", got)
	}
	// Keyless tasks (no ticket_issue_map row, no title tag) ship without
	// the [MAQ-n] tag — the marker must still read as a timeline entry.
	if got := reviewPickupBody("", 1); got != "🔁 review round 1 started" {
		t.Errorf("keyless review body = %q", got)
	}
	if got := fixerPickupBody("", 3, "addressing review findings"); got != "🔧 fixer round 3 picked this up - addressing review findings" {
		t.Errorf("keyless fixer body = %q", got)
	}
	if got := mergePickupBody(""); got != "🚀 merge gate running" {
		t.Errorf("keyless merge body = %q", got)
	}
}

func TestPickupNeedles_NoCrossRoundCollision(t *testing.T) {
	// A round-N needle must never match a round-M comment (N≠M) — the
	// dedup would otherwise swallow a whole round's marker.
	if strings.Contains("🔁 [MAQ-1] review round 12 started", reviewPickupNeedle(2)) ||
		strings.Contains("🔁 [MAQ-1] review round 2 started", reviewPickupNeedle(12)) {
		t.Errorf("review pickup needles collide across rounds")
	}
	if strings.Contains("🔧 fixer round 12 picked this up", fixerPickupNeedle(2)) ||
		strings.Contains("🔧 fixer round 2 picked this up", fixerPickupNeedle(12)) {
		t.Errorf("fixer pickup needles collide across rounds")
	}
	// The needles must not match the MAQ-16 verdict marker (or vice versa)
	// — the two dedup scans would suppress each other's comments.
	if strings.Contains("[review round 2] VERDICT: request_changes", reviewPickupNeedle(2)) {
		t.Errorf("review pickup needle matches the verdict marker")
	}
	if strings.Contains(reviewPickupBody("MAQ-1", 2), reviewCommentMarker(2)) {
		t.Errorf("review pickup body matches the verdict marker")
	}
}

func TestIsOwnPRComment(t *testing.T) {
	own := []string{
		"[review round 1] VERDICT: approve",                          // MAQ-16 verdict
		"🔁 [MAQ-25] review round 2 started",                         // reviewer pickup
		"🔁 review round 2 started",                                  // keyless
		"🔧 [MAQ-25] fixer round 2 picked this up - 1) tests missing", // fixer pickup
		"🚀 [MAQ-25] merge gate running",                             // merge leg
		"🚀 merge gate running",                                      // keyless
	}
	for _, b := range own {
		if !isOwnPRComment(b) {
			t.Errorf("isOwnPRComment(%q) = false, want true", b)
		}
	}
	human := []string{
		"please add tests",
		"the review round yesterday looked fine to me",
		"LGTM except the 🔧 tooling nit",
		"[review roundly] nice work",
	}
	for _, b := range human {
		if isOwnPRComment(b) {
			t.Errorf("isOwnPRComment(%q) = true, want false", b)
		}
	}
}

func TestRenderHumanComments_FiltersPickups(t *testing.T) {
	comments := []PRComment{
		{Author: "alice", Body: "please add tests"},
		{Author: "deploy-bot", IsBot: true, Body: "🚀 merge gate running"},
		{Author: "gh-human", Body: "🔧 [MAQ-25] fixer round 1 picked this up - 1) tests missing"},
		{Author: "gh-human", Body: "🔁 [MAQ-25] review round 2 started"},
	}
	out := renderHumanComments(comments, nil)
	if !strings.Contains(out, "alice") {
		t.Errorf("human comment dropped: %q", out)
	}
	for _, own := range []string{"picked this up", "review round 2 started", "merge gate running"} {
		if strings.Contains(out, own) {
			t.Errorf("our own pickup leaked into the human feed: %q", out)
		}
	}
}

// ---- DB-backed: the one-line reason --------------------------------------

func TestFixPickupReason(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seed := func(agentID, text string) {
		t.Helper()
		execOK(t, pool, `
			INSERT INTO agents (id, tmux_session, tmux_window, status, started_at, last_seen, stop_requested)
			VALUES ($1, 'sess', $1, 'dead', NOW(), NOW(), FALSE)
		`, agentID)
		body, err := json.Marshal(text)
		if err != nil {
			t.Fatalf("marshal findings: %v", err)
		}
		execOK(t, pool, `
			INSERT INTO agent_outbox (agent_id, content) VALUES ($1, $2::jsonb)
		`, agentID, `{"text":`+string(body)+`}`)
	}

	seed("r1", "1) tests missing\n2) checks.md claim unproven\nVERDICT: request_changes\n")
	if got := fixPickupReason(ctx, pool, "r1"); got != "1) tests missing" {
		t.Errorf("reason = %q, want the first finding line", got)
	}

	seed("r2", "\n\nVERDICT: request_changes\nthe diff drops the migration\n")
	if got := fixPickupReason(ctx, pool, "r2"); got != "the diff drops the migration" {
		t.Errorf("reason = %q, want first non-VERDICT line (blanks skipped)", got)
	}

	seed("r3", strings.Repeat("x", 200))
	got := fixPickupReason(ctx, pool, "r3")
	if len([]rune(got)) != maxPickupReasonChars+2 || !strings.HasSuffix(got, "…") {
		t.Errorf("reason len = %d, want capped at %d + separator + ellipsis", len([]rune(got)), maxPickupReasonChars)
	}

	seed("r4", "VERDICT: request_changes\n")
	if got := fixPickupReason(ctx, pool, "r4"); got != "" {
		t.Errorf("reason = %q, want empty (nothing but the verdict line)", got)
	}
	if got := fixPickupReason(ctx, pool, "r-missing"); got != "" {
		t.Errorf("reason for missing agent = %q, want empty", got)
	}
}

// ---- DB-backed: reviewer round pickup ------------------------------------

// seedPickupReview: a review task with a PR URL and a [MAQ-n] tag in
// ticket_issue_map — the exact state dispatchPass consumes at spawn.
func seedPickupReview(t *testing.T, pool *pgxpool.Pool, taskID string) {
	t.Helper()
	seedReviewTask(t, pool, taskID, "uuid-"+taskID, "/tmp/wt-"+taskID)
	execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`,
		taskID, "https://github.com/acme/repo/pull/7")
	seedIssueKey(t, pool, taskID, "MAQ-25")
}

// seedIssueKey maps the task to a Linear issue key (the [MAQ-n] tag source).
func seedIssueKey(t *testing.T, pool *pgxpool.Pool, taskID, key string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO ticket_issue_map (issue_id, issue_key, team_id, task_id, pending_state)
		VALUES ('issue-' || $1, $2, 'team-1', $1, 'In Progress')
	`, taskID, key)
}

func TestDispatchPass_PostsReviewPickupOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPickupReview(t, pool, "pk1")

	fg := &fakeGh{}
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
	if len(fg.postedBodies) != 1 {
		t.Fatalf("posted = %d, want 1: %v", len(fg.postedBodies), fg.postedBodies)
	}
	if got := fg.postedBodies[0]; got != "🔁 [MAQ-25] review round 1 started" {
		t.Fatalf("pickup body = %q", got)
	}
	if got := taskCol(t, pool, "pk1", "review_rounds::text"); got != "1" {
		t.Fatalf("review_rounds = %q, want 1 (marker must not disturb round bookkeeping)", got)
	}

	// The live reviewer blocks a re-pick; nothing else posts.
	before := len(fg.postedBodies)
	if err := dispatchPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass 2: %v", err)
	}
	if len(fg.postedBodies) != before {
		t.Fatalf("posted after 2nd pass = %d, want %d", len(fg.postedBodies), before)
	}
}

func TestDispatchPass_PickupDedupExistingComment(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPickupReview(t, pool, "pk2")

	// The marker already sits on the PR (crash after post, before the
	// round bookkeeping was visible): the needle scan suppresses the repost.
	fg := &fakeGh{comments: []PRComment{
		{Author: "acme-ci", IsBot: true, Body: "🔁 [MAQ-25] review round 1 started"},
	}}
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(fg.postedBodies) != 0 {
		t.Fatalf("posted = %v, want none (needle dedup)", fg.postedBodies)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1 (dedup is comment-side only)", len(sp.spawns))
	}
}

// ---- DB-backed: fixer episode pickup --------------------------------------

func seedPickupFixEpisode(t *testing.T, pool *pgxpool.Pool, taskID string, rounds int) {
	t.Helper()
	seedFixEpisode(t, pool, taskID, "uuid-"+taskID, "/tmp/wt-"+taskID, rounds)
	execOK(t, pool, `UPDATE tasks SET pr_url = $2 WHERE id = $1`,
		taskID, "https://github.com/acme/repo/pull/9")
	seedIssueKey(t, pool, taskID, "MAQ-25")
}

func TestFixerPass_PostsFixerPickup(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPickupFixEpisode(t, pool, "pk3", 1)

	fg := &fakeGh{}
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := fixerPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
	if len(fg.postedBodies) != 1 {
		t.Fatalf("posted = %d, want 1: %v", len(fg.postedBodies), fg.postedBodies)
	}
	// The reason is the first finding line from the reviewer's findings
	// (seedFixEpisode's outbox row), not the VERDICT line.
	want := "🔧 [MAQ-25] fixer round 1 picked this up - 1) tests missing"
	if got := fg.postedBodies[0]; got != want {
		t.Fatalf("pickup body = %q, want %q", got, want)
	}

	// The fix row ends the episode: a second pass spawns and posts nothing.
	before := len(fg.postedBodies)
	if err := fixerPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("fixerPass 2: %v", err)
	}
	if len(fg.postedBodies) != before {
		t.Fatalf("posted after 2nd pass = %d, want %d", len(fg.postedBodies), before)
	}
}

// ---- AC 4 + AC 5: GitHub is optional and never fatal -----------------------

func TestPickup_NoPR_SkipsSilently(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPickupReview(t, pool, "pk4")
	execOK(t, pool, `UPDATE tasks SET pr_url = NULL WHERE id = 'pk4'`)

	fg := &fakeGh{}
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, fg, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 || len(fg.postedBodies) != 0 {
		t.Fatalf("spawns = %d posted = %d, want 1/0 (no PR → no comment, no error)",
			len(sp.spawns), len(fg.postedBodies))
	}
	// ...and the round bookkeeping is untouched.
	if got := taskCol(t, pool, "pk4", "review_rounds::text"); got != "1" {
		t.Fatalf("review_rounds = %q, want 1", got)
	}
}

func TestPickup_GhOutage_SpawnUnaffected(t *testing.T) {
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
			id := "pk5-" + strings.ReplaceAll(c.name, " ", "-")
			seedPickupReview(t, pool, id)

			sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
			if err := dispatchPass(ctx, pool, c.fg, sp, DefaultImplementorIdleAfter); err != nil {
				t.Fatalf("dispatchPass: %v", err)
			}
			if len(sp.spawns) != 1 {
				t.Fatalf("spawns = %d, want 1 (gh outage must not block the spawn)", len(sp.spawns))
			}
			if got := taskCol(t, pool, id, "review_rounds::text"); got != "1" {
				t.Fatalf("review_rounds = %q, want 1", got)
			}

			// Same for the fixer leg.
			fid := id + "-fix"
			seedPickupFixEpisode(t, pool, fid, 1)
			fsp := &fakeSpawner{t: t, pool: pool, insertRow: true}
			if err := fixerPass(ctx, pool, c.fg, fsp, DefaultImplementorIdleAfter); err != nil {
				t.Fatalf("fixerPass: %v", err)
			}
			if len(fsp.spawns) != 1 {
				t.Fatalf("fixer spawns = %d, want 1 (gh outage must not block the spawn)", len(fsp.spawns))
			}
		})
	}
}

func TestPickup_NilGh_NoOp(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedPickupReview(t, pool, "pk6")

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := dispatchPass(ctx, pool, nil, sp, DefaultImplementorIdleAfter); err != nil {
		t.Fatalf("dispatchPass: %v", err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
}

// ---- DB-backed: merge leg pickup -------------------------------------------

// TestProcessMergeGH_PostsMergeGatePickupOnce: the gate's first real pass
// (human gate, status guard and merger-episode guard all passed) posts the
// 🚀 marker; the marker must not disturb the merge outcome.
func TestProcessMergeGH_PostsMergeGatePickupOnce(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "pickupgate")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID

	pushGoBase(t, admin)
	gitCommitFile(t, worktree, "extra.go", gateClean)
	gitRun(t, worktree, "push", "origin", entry.Branch)
	seedIssueKey(t, pool, taskID, "MAQ-25")

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "done" {
		t.Fatalf("task status = %q, want done (marker must not disturb the gate)", status)
	}
	if len(gh.postedBodies) != 1 {
		t.Fatalf("posted = %d, want 1: %v", len(gh.postedBodies), gh.postedBodies)
	}
	if got := gh.postedBodies[0]; got != "🚀 [MAQ-25] merge gate running" {
		t.Fatalf("pickup body = %q", got)
	}
}
