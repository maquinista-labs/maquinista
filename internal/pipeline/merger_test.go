package pipeline

// MAQ-15 merger-agent proofs (AC 1–5). DB-backed tests pair the package
// testcontainer harness (testPool) with the real git trio from merge_test.go
// (bare origin + admin clone + task worktree), so rebase/push/cleanup run
// the actual machinery. Agents are faked at the ReviewSpawner seam; the
// merger's outbox verdict is seeded exactly like the dispatch tests seed it.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
)

// ---- frozen verdict contract ----

func TestParseMergeVerdict(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
		ok   bool
	}{
		{"resolved both sides\nVERDICT: merged\n", VerdictMerged, true},
		{"semantic mess\n  VERDICT:  needs_human  \n", VerdictNeedsHuman, true},
		{"VERDICT: approve\n", "", false},        // reviewer vocabulary, not the merger's
		{"VERDICT: MERGED\n", "", false},         // case-sensitive
		{"no verdict at all\n", "", false},       // absent
		{"VERDICT: merged-totally\n", "", false}, // not the line-anchored vocabulary
		{"VERDICT: unknown\n", "", false},        // outside the vocabulary
	} {
		got, ok := ParseMergeVerdict(tc.text)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseMergeVerdict(%q) = %q,%v want %q,%v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMergeConfigFromEnv_MergeAgentFlag(t *testing.T) {
	t.Setenv("PIPELINE_MERGE_AGENT", "")
	if cfg := MergeConfigFromEnv(); cfg.MergeAgent {
		t.Error("MERGE_AGENT must default to off")
	}
	for _, v := range []string{"1", "true", "YES", "t"} {
		t.Setenv("PIPELINE_MERGE_AGENT", v)
		if cfg := MergeConfigFromEnv(); !cfg.MergeAgent {
			t.Errorf("MERGE_AGENT=%q not accepted", v)
		}
	}
	t.Setenv("PIPELINE_MERGE_AGENT", "0")
	if cfg := MergeConfigFromEnv(); cfg.MergeAgent {
		t.Error("MERGE_AGENT=0 must stay off")
	}
}

// ---- harness ----

// forceRebaseConflict advances origin/main with a change to path that
// collides with the branch's own change (both edit the same file), and
// pushes both — the exact "additive, mechanically resolvable" shape from
// the ticket.
func forceRebaseConflict(t *testing.T, admin, worktree, path, mainContent, branchContent string) {
	t.Helper()
	gitRun(t, admin, "checkout", "main")
	gitCommitFile(t, admin, path, mainContent)
	gitRun(t, admin, "push", "origin", "main")
	gitCommitFile(t, worktree, path, branchContent)
	branch := gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	gitRun(t, worktree, "push", "origin", branch)
}

// armMergerConflict seeds a ready_to_merge task on a worktree that will
// conflict with main, runs one ProcessMergeGH pass with MergeAgent on, and
// returns the (released) entry plus the task id and worktree.
func armMergerConflict(t *testing.T, pool *pgxpool.Pool, tag string) (entry *db.MergeQueueEntry, taskID, worktree string) {
	t.Helper()
	_, worktree = initRemoteTrio(t, tag)
	entry = seedReadyTask(t, pool, worktree)
	taskID = entry.TaskID
	forceRebaseConflict(t, gitRepoRoot(t, worktree), worktree, "feature.txt", "main wins\n", "branch wins\n")

	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MergeAgent: true, Gh: &fakeGh{checks: ChecksGreen}}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	return entry, taskID, worktree
}

// simulateMergerKeepBoth plays the merger agent: rebase onto origin/main,
// resolve the conflict with the keep-both concatenation, continue.
func simulateMergerKeepBoth(t *testing.T, worktree, path, resolved string) {
	t.Helper()
	gitRun(t, worktree, "fetch", "origin")
	out, _ := exec.Command("git", "-C", worktree, "rebase", "origin/main").CombinedOutput()
	if !strings.Contains(string(out), "CONFLICT") {
		t.Fatalf("expected a rebase conflict, got: %s", out)
	}
	if err := os.WriteFile(worktree+"/"+path, []byte(resolved), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, worktree, "add", path)
	cmd := exec.Command("git", "-C", worktree, "rebase", "--continue")
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git rebase --continue: %v: %s", err, out)
	}
}

// seedMerger inserts a live merger agent row and, when outboxText is
// non-empty, its final assistant message (the row latestMergeVerdict parses).
func seedMerger(t *testing.T, pool *pgxpool.Pool, agentID, taskID, outboxText string) {
	t.Helper()
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ($1, 'sess', $1, 'merger', $2, 'running', 'pi', '/tmp/wt', $1, NOW(), NOW(), FALSE)
	`, agentID, taskID)
	if outboxText != "" {
		b, _ := json.Marshal(map[string]string{"text": outboxText})
		execOK(t, pool, `
			INSERT INTO agent_outbox (agent_id, content) VALUES ($1, $2::jsonb)
		`, agentID, string(b))
	}
}

func seedMergeConflictMarker(t *testing.T, pool *pgxpool.Pool, taskID string, marker mergeConflictMarker, ageSQL string) {
	t.Helper()
	b, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if ageSQL == "" {
		ageSQL = "0 seconds"
	}
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content, created_at)
		VALUES ($1, 'merger', $2, $3, NOW() - ($4)::interval)
	`, taskID, mergeConflictKind, string(b), ageSQL)
}

func mergerAgentStatus(t *testing.T, pool *pgxpool.Pool, agentID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM agents WHERE id = $1`, agentID).Scan(&status); err != nil {
		t.Fatalf("loading merger %s: %v", agentID, err)
	}
	return status
}

func entryConflictFiles(t *testing.T, pool *pgxpool.Pool, id int64) []string {
	t.Helper()
	var files []string
	if err := pool.QueryRow(context.Background(),
		`SELECT conflict_files FROM merge_queue WHERE id = $1`, id).Scan(&files); err != nil {
		t.Fatalf("loading entry %d: %v", id, err)
	}
	return files
}

func hasMergeVerdictRow(t *testing.T, pool *pgxpool.Pool, taskID, content string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM task_context WHERE task_id=$1 AND kind=$2 AND content=$3)`,
		taskID, mergeVerdictKind, content).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func inboxPrompt(t *testing.T, pool *pgxpool.Pool, agentID string) (externalMsgID, prompt string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT external_msg_id, content->>'prompt' FROM agent_inbox
		WHERE agent_id = $1 ORDER BY enqueued_at DESC LIMIT 1
	`, agentID).Scan(&externalMsgID, &prompt); err != nil {
		t.Fatalf("loading inbox prompt for %s: %v", agentID, err)
	}
	return externalMsgID, prompt
}

// ---- AC 1: a mechanical conflict arms the episode, then merges ----

// The conflict arm: marker written, entry released, task still
// ready_to_merge, no park, no merge fired.
func TestProcessMergeGH_ConflictArmsMergerEpisode(t *testing.T) {
	pool := testPool(t)
	entry, taskID, _ := armMergerConflict(t, pool, "arm")

	if status, _ := taskRow(t, pool, taskID); status != "ready_to_merge" {
		t.Errorf("task = %s, want ready_to_merge (no park on an armed episode)", status)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Errorf("entry = %q, want released to pending", got)
	}
	// Marker: payload carries entry/attempt/base/branch/files.
	marker, ok, err := latestMergeConflict(context.Background(), pool, taskID)
	if err != nil || !ok {
		t.Fatalf("merge_conflict marker missing (ok=%v, err=%v)", ok, err)
	}
	if marker.EntryID != entry.ID || marker.Attempt != 1 || marker.Base != "main" ||
		marker.Branch != entry.Branch || len(marker.Files) != 1 || marker.Files[0] != "feature.txt" {
		t.Errorf("marker = %+v, want entry %d attempt 1 base main branch %s files [feature.txt]",
			marker, entry.ID, entry.Branch)
	}
	if hasMergeVerdictRow(t, pool, taskID, marker.episodeLabel()) {
		t.Error("fresh episode must not be consumed yet")
	}
	// Notify names the merger and the file.
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("notes = %d, want 1 (the arming note)", len(texts))
	}
	for _, want := range []string{"🔀", "merger agent", "feature.txt", "attempt 1/5"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("arming note %q missing %q", texts[0], want)
		}
	}
}

// AC 1 end-to-end: the merger resolves keep-both, verdicts merged, and the
// released entry completes the normal merge path with no human.
func TestMergerVerdict_MergedContinuesToMerge(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	entry, taskID, worktree := armMergerConflict(t, pool, "resolve")
	marker, _, _ := latestMergeConflict(ctx, pool, taskID)

	// The merger agent does its thing in the worktree...
	simulateMergerKeepBoth(t, worktree, "feature.txt", "main wins\nbranch wins\n")
	// ...and verdicts on its outbox.
	agentID := "merger-" + taskID
	seedMerger(t, pool, agentID, taskID, "kept both sides (prLinkSuffix + short-id verbs)\nVERDICT: merged\n")

	if err := mergerVerdictPass(ctx, pool, "sess", nil); err != nil {
		t.Fatal(err)
	}
	if got := mergerAgentStatus(t, pool, agentID); got != "dead" {
		t.Errorf("merger = %s, want retired", got)
	}
	if !hasMergeVerdictRow(t, pool, taskID, marker.episodeLabel()) {
		t.Error("episode must be consumed by the merged verdict")
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 2 || !strings.Contains(texts[1], "re-entering the merge queue") {
		t.Fatalf("notes = %q, want the re-entering note second", texts)
	}

	// The released entry re-claims (FIFO) and the normal path completes.
	claimed, err := db.ClaimMergeEntry(pool)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != entry.ID {
		t.Fatalf("re-claim got %+v, want entry %d", claimed, entry.ID)
	}
	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MergeAgent: true, Gh: gh}
	if err := ProcessMergeGH(ctx, pool, cfg, &fakeProvider{}, "team-1", claimed); err != nil {
		t.Fatal(err)
	}
	status, prState := taskRow(t, pool, taskID)
	if status != "done" || prState != "merged" {
		t.Errorf("task = %s/%s, want done/merged", status, prState)
	}
	if got := entryStatus(t, pool, entry.ID); got != "merged" {
		t.Errorf("entry = %q, want merged", got)
	}
	if gh.mergeCalls != 1 {
		t.Errorf("gh merge calls = %d, want 1", gh.mergeCalls)
	}
}

// ---- AC 2: semantic conflict → park needs-human, unchanged semantics ----

func TestMergerVerdict_NeedsHumanParksWithFiles(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	entry, taskID, _ := armMergerConflict(t, pool, "nh")

	agentID := "merger-" + taskID
	seedMerger(t, pool, agentID, taskID, "both sides rewrote the same loop — cannot resolve mechanically\nVERDICT: needs_human\n")

	if err := mergerVerdictPass(ctx, pool, "sess", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("task = %s, want pending_approval", status)
	}
	if got := entryStatus(t, pool, entry.ID); got != "conflict" {
		t.Errorf("entry = %q, want conflict", got)
	}
	files := entryConflictFiles(t, pool, entry.ID)
	if len(files) != 1 || files[0] != "feature.txt" {
		t.Errorf("conflict_files = %v, want [feature.txt]", files)
	}
	if got := mergerAgentStatus(t, pool, agentID); got != "dead" {
		t.Errorf("merger = %s, want retired", got)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 2 {
		t.Fatalf("notes = %d, want arming + park", len(texts))
	}
	for _, want := range []string{"🆘", "could not resolve", "feature.txt"} {
		if !strings.Contains(texts[1], want) {
			t.Errorf("park note %q missing %q", texts[1], want)
		}
	}
	if strings.Contains(texts[1], "merger agent resolving") {
		t.Errorf("park note %q must not be the arming note", texts[1])
	}
}

// AC 2 (fallback): MergeAgent off keeps the old park behavior.
// MAQ-26: with the agent off, the conflict leg merge-ups first — two failed
// attempts park needs-human exactly as the bare path did, and no merger
// marker is ever written (no episode exists without the agent).
func TestProcessMergeGH_ConflictNoAgentParks(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "noagent")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	forceRebaseConflict(t, gitRepoRoot(t, worktree), worktree, "feature.txt", "main wins\n", "branch wins\n")

	gh := &fakeGh{checks: ChecksGreen, updateErr: ErrMergeUpConflict} // GitHub 422: overlap is semantic
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MergeAgent: false, Gh: gh}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("attempt 1: entry = %q, want released to pending (merge-up budget)", got)
	}
	claimed, err := db.ClaimMergeEntryByID(pool, entry.ID)
	if err != nil || claimed == nil {
		t.Fatalf("re-claim: %v (%v)", err, claimed)
	}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", claimed); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("task = %s, want pending_approval after 2 failed merge-ups", status)
	}
	if got := entryStatus(t, pool, entry.ID); got != "conflict" {
		t.Errorf("entry = %q, want conflict", got)
	}
	if _, ok, _ := latestMergeConflict(context.Background(), pool, taskID); ok {
		t.Error("no marker must be written with the agent off")
	}
}

// Cap: each armed episode consumes one attempt; at the cap the conflict
// parks needs-human exactly as before (no new episode).
func TestProcessMergeGH_ConflictMergerCapParks(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "cap")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	forceRebaseConflict(t, gitRepoRoot(t, worktree), worktree, "feature.txt", "main wins\n", "branch wins\n")
	execOK(t, pool, `UPDATE merge_queue SET attempts = 4 WHERE id = $1`, entry.ID) // one attempt left

	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MergeAgent: true, MaxAttempts: 5, Gh: &fakeGh{checks: ChecksGreen}}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("task = %s, want parked pending_approval at cap", status)
	}
	if got := entryStatus(t, pool, entry.ID); got != "conflict" {
		t.Errorf("entry = %q, want conflict", got)
	}
	if _, ok, _ := latestMergeConflict(context.Background(), pool, taskID); ok {
		t.Error("cap exhausted must not arm another episode")
	}
}

// ---- in-flight guard: processors never touch a worktree mid-episode ----

func TestProcessMergeGH_MergerInFlightReleases(t *testing.T) {
	pool := testPool(t)
	entry, taskID, worktree := armMergerConflict(t, pool, "inflight")

	// The episode is armed (marker unconsumed). A processor re-claims and
	// must release without bumping attempts or touching the worktree.
	claimed, err := db.ClaimMergeEntry(pool)
	if err != nil || claimed == nil || claimed.ID != entry.ID {
		t.Fatalf("re-claim: %v (%+v)", err, claimed)
	}
	branchBefore := gitRun(t, worktree, "rev-parse", "HEAD")
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MergeAgent: true, Gh: &fakeGh{checks: ChecksGreen}}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", claimed); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Errorf("entry = %q, want released to pending", got)
	}
	var attempts int
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts FROM merge_queue WHERE id = $1`, entry.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want still 1 (guard must not consume budget)", attempts)
	}
	if gitRun(t, worktree, "rev-parse", "HEAD") != branchBefore {
		t.Error("worktree moved while the merger episode was in flight")
	}

	// Same guard when the marker is consumed but the pane is still live.
	marker, _, _ := latestMergeConflict(context.Background(), pool, taskID)
	execOK(t, pool, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, 'merger', $2, $3)
	`, taskID, mergeVerdictKind, marker.episodeLabel())
	seedMerger(t, pool, "merger-"+taskID, taskID, "")
	claimed2, err := db.ClaimMergeEntry(pool)
	if err != nil || claimed2 == nil {
		t.Fatalf("re-claim 2: %v (%+v)", err, claimed2)
	}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", claimed2); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Errorf("entry = %q, want released with a live merger", got)
	}
}

// ---- mergerPass: spawn + prompt + heal ----

func TestMergerPass_SpawnPromptHeal(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path)
		VALUES ($1, 'merge me', 'ready_to_merge', '/tmp/wt-mp')
	`, taskID)
	seedMergeConflictMarker(t, pool, taskID, mergeConflictMarker{
		EntryID: 42, Attempt: 2, Base: "main", Branch: "t-x/feature",
		Files: []string{"a.go", "internal/deep/b.go"},
	}, "0 seconds")

	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := mergerPass(ctx, pool, sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want 1", len(sp.spawns))
	}
	p := sp.spawns[0]
	if p.AgentID != "merger-"+taskID || p.Role != "merger" || p.SoulTemplateID != MergerSoulTemplate ||
		p.TaskID != taskID || p.WorktreePath != "/tmp/wt-mp" {
		t.Fatalf("spawn params = %+v", p)
	}
	if p.RunnerType != "pi" {
		t.Fatalf("runner = %q, want pi (pipeline-merger extras)", p.RunnerType)
	}
	externalMsgID, prompt := inboxPrompt(t, pool, p.AgentID)
	if externalMsgID != fmt.Sprintf("merger:%s:42:2", taskID) {
		t.Errorf("external_msg_id = %q, want the episode-dedup id", externalMsgID)
	}
	for _, want := range []string{"attempt 2", "origin/main", "t-x/feature", "a.go", "internal/deep/b.go",
		"VERDICT: merged", "VERDICT: needs_human", "git rebase --abort"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Second tick: the live merger blocks a duplicate spawn.
	if err := mergerPass(ctx, pool, sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns after second pass = %d, want 1", len(sp.spawns))
	}

	// Heal: a lost prompt (crash between spawn and enqueue) re-enqueues
	// exactly once, dedup'd by external_msg_id — no second spawn.
	execOK(t, pool, `DELETE FROM agent_inbox WHERE agent_id = $1`, p.AgentID)
	if err := mergerPass(ctx, pool, sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns after heal = %d, want 1", len(sp.spawns))
	}
	if _, prompt := inboxPrompt(t, pool, p.AgentID); prompt == "" {
		t.Error("heal did not re-enqueue the prompt")
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_inbox WHERE agent_id = $1`, p.AgentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("inbox rows after heal = %d, want 1 (dedup)", n)
	}
}

// Stale episode: armed but never dispatched (CLI-only deployment, no
// dispatch loop) parks needs-human instead of bouncing the entry forever.
func TestMergerPass_StaleMarkerParks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url)
		VALUES ($1, 'stale', 'ready_to_merge', '/tmp/wt-stale',
		        'https://github.com/maquinista-labs/maquinista/pull/42')
	`, taskID)
	seedMergeConflictMarker(t, pool, taskID, mergeConflictMarker{
		EntryID: 7, Attempt: 1, Base: "main", Branch: "t-x/feature", Files: []string{"x.go"},
	}, "3 hours")

	sp := &fakeSpawner{t: t, pool: pool, insertRow: false}
	if err := mergerPass(ctx, pool, sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.spawns) != 0 {
		t.Errorf("spawns = %d, want 0 (stale marker must park, not spawn)", len(sp.spawns))
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("task = %s, want parked pending_approval", status)
	}
	if !hasMergeVerdictRow(t, pool, taskID, "entry 7 attempt 1") {
		t.Error("stale episode must be consumed by the park")
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 || !strings.Contains(texts[0], "never dispatched") {
		t.Fatalf("notes = %q, want one never-dispatched note", texts)
	}

	// Fresh markers are untouched by the stale bound.
	task2 := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path)
		VALUES ($1, 'fresh', 'ready_to_merge', '/tmp/wt-fresh')
	`, task2)
	seedMergeConflictMarker(t, pool, task2, mergeConflictMarker{
		EntryID: 8, Attempt: 1, Base: "main", Branch: "t-y/feature", Files: []string{"y.go"},
	}, "0 seconds")
	if err := mergerPass(ctx, pool, sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.spawns) != 1 || sp.spawns[0].TaskID != task2 {
		t.Errorf("fresh-marker spawn = %+v, want exactly one for %s", sp.spawns, task2)
	}
}

// ---- watchdog ----

func TestMergerWatchdog_ParksStalledMerger(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url)
		VALUES ($1, 'stalled', 'ready_to_merge', '/tmp/wt-wd',
		        'https://github.com/maquinista-labs/maquinista/pull/43')
	`, taskID)
	seedMergeConflictMarker(t, pool, taskID, mergeConflictMarker{
		EntryID: 9, Attempt: 1, Base: "main", Branch: "t-z/feature", Files: []string{"z.go"},
	}, "0 seconds")
	execOK(t, pool, `
		INSERT INTO merge_queue (id, task_id, agent_id, branch, worktree_dir, base_branch, status)
		VALUES (9, $1, 'merger', 't-z/feature', '/tmp/wt-wd', 'main', 'pending')
	`, taskID)
	seedMerger(t, pool, "merger-"+taskID, taskID, "")
	execOK(t, pool, `UPDATE agents SET started_at = NOW() - INTERVAL '3 hours' WHERE id = $1`, "merger-"+taskID)

	if err := mergerWatchdogPass(ctx, pool, 2*time.Hour, "sess", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("task = %s, want parked pending_approval", status)
	}
	if got := mergerAgentStatus(t, pool, "merger-"+taskID); got != "dead" {
		t.Errorf("merger = %s, want retired", got)
	}
	files := entryConflictFiles(t, pool, 9)
	if len(files) != 1 || files[0] != "z.go" {
		t.Errorf("conflict_files = %v, want [z.go]", files)
	}
}

// ---- AC 4: FIFO survives the release/re-claim cycle ----

func TestMergerRelease_KeepsFIFOPosition(t *testing.T) {
	pool := testPool(t)
	entryA, _, _ := armMergerConflict(t, pool, "fifo") // armed → released to pending

	// A second task's entry enqueued AFTER A's original enqueue.
	taskB := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path)
		VALUES ($1, 'later', 'ready_to_merge', '/tmp/wt-b')
	`, taskB)
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, enqueued_at)
		VALUES ($1, 'merger', 'b/branch', '/tmp/wt-b', 'main', NOW() + INTERVAL '1 second')
	`, taskB)

	claimed, err := db.ClaimMergeEntry(pool)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v (%+v)", err, claimed)
	}
	if claimed.ID != entryA.ID {
		t.Errorf("claimed entry %d, want %d (FIFO: the released oldest entry first)", claimed.ID, entryA.ID)
	}
}

// ---- AC 5: a re-approved task runs the merger path exactly once per entry ----

func TestReapprove_FreshEntryRunsMergerExactlyOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	entry1, taskID, _ := armMergerConflict(t, pool, "reappr")

	// Episode 1 lands needs-human (parked task, conflicted entry).
	agentID := "merger-" + taskID
	seedMerger(t, pool, agentID, taskID, "hopeless\nVERDICT: needs_human\n")
	if err := mergerVerdictPass(ctx, pool, "sess", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Fatalf("task = %s, want parked after episode 1", status)
	}

	// Re-approval (whatever route the operator takes) re-creates a
	// ready_to_merge task with a FRESH entry — attempts reset. The same
	// conflict fires again: exactly one new episode must arm, identified
	// by the new entry, NOT swallowed by episode 1's consumed marker.
	execOK(t, pool, `UPDATE tasks SET status = 'ready_to_merge' WHERE id = $1`, taskID)
	branch := gitRun(t, worktreeOf(t, pool, taskID), "rev-parse", "--abbrev-ref", "HEAD")
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, commit_sha, enqueued_at)
		VALUES ($1, 'merger', $2, $3, 'main', '', NOW() + INTERVAL '2 seconds')
	`, taskID, branch, worktreeOf(t, pool, taskID))
	entry2, err := db.ClaimMergeEntry(pool)
	if err != nil || entry2 == nil {
		t.Fatalf("claiming fresh entry: %v (%+v)", err, entry2)
	}

	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MergeAgent: true, Gh: &fakeGh{checks: ChecksGreen}}
	if err := ProcessMergeGH(ctx, pool, cfg, &fakeProvider{}, "team-1", entry2); err != nil {
		t.Fatal(err)
	}
	marker2, ok, err := latestMergeConflict(ctx, pool, taskID)
	if err != nil || !ok {
		t.Fatalf("episode 2 marker missing: %v", err)
	}
	if marker2.EntryID != entry2.ID || marker2.Attempt != 1 {
		t.Fatalf("marker = %+v, want entry %d attempt 1 (fresh episode identity)", marker2, entry2.ID)
	}
	if hasMergeVerdictRow(t, pool, taskID, marker2.episodeLabel()) {
		t.Error("episode 2 must be unconsumed — entry-scoped identity, not task-scoped")
	}

	// The dispatch loop spawns exactly ONE merger for episode 2.
	sp := &fakeSpawner{t: t, pool: pool, insertRow: true}
	if err := mergerPass(ctx, pool, sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns = %d, want exactly 1 for the re-approved task", len(sp.spawns))
	}
	if err := mergerPass(ctx, pool, sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.spawns) != 1 {
		t.Fatalf("spawns after repeat tick = %d, want 1 (no double episode)", len(sp.spawns))
	}
	_ = entry1
}

func worktreeOf(t *testing.T, pool *pgxpool.Pool, taskID string) string {
	t.Helper()
	var wt string
	if err := pool.QueryRow(context.Background(),
		`SELECT worktree_path FROM tasks WHERE id = $1`, taskID).Scan(&wt); err != nil {
		t.Fatal(err)
	}
	return wt
}
