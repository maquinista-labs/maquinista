package pipeline

// Tests for the GitHub comment-command surface (MAQ-12). Parser, dispatch
// table, auth, id-less target resolution and exactly-once claiming are
// unit-tested at the CommentSource seam (fakes); the approve verb runs the
// real merge flow end-to-end on the same disposable Postgres + local git
// trio the EX-05 merge tests use — GitHub faked at the runner seam.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- fakes ----

type fakeComments struct {
	mu            sync.Mutex
	comments      map[int][]PRComment
	failPRs       map[int]error
	collaborators map[string]bool
	collabErr     error
	reactions     []int64
	headBranch    map[int]string
}

func (f *fakeComments) PRComments(_ context.Context, pr int, _ time.Time) ([]PRComment, error) {
	if err := f.failPRs[pr]; err != nil {
		return nil, err
	}
	return f.comments[pr], nil
}

func (f *fakeComments) IsCollaborator(_ context.Context, login string) (bool, error) {
	if f.collabErr != nil {
		return false, f.collabErr
	}
	return f.collaborators[login], nil
}

func (f *fakeComments) ReactToComment(_ context.Context, commentID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, commentID)
	return nil
}

func (f *fakeComments) PRHeadBranch(_ context.Context, pr int) (string, error) {
	return f.headBranch[pr], nil
}

// registerTestVerb registers a probe verb that records its invocation and
// unregisters itself on cleanup. The point: a second verb needs nothing but
// this registration (MAQ-12 AC 5).
func registerTestVerb(t *testing.T) func() []CommentContext {
	t.Helper()
	var mu sync.Mutex
	var got []CommentContext
	RegisterCommentVerb("probe", func(_ context.Context, hc CommentContext) error {
		mu.Lock()
		got = append(got, hc)
		mu.Unlock()
		return nil
	})
	t.Cleanup(func() { unregisterCommentVerb("probe") })
	return func() []CommentContext {
		mu.Lock()
		defer mu.Unlock()
		return append([]CommentContext(nil), got...)
	}
}

// ---- parser ----

func TestParseCommentCommand(t *testing.T) {
	cases := []struct {
		name string
		body string
		verb string
		args []string
		ok   bool
	}{
		{"plain", "maquinista approve", "approve", nil, true},
		{"slash", "/maquinista approve", "approve", nil, true},
		{"leading ws", "   \tmaquinista approve", "approve", nil, true},
		{"case-insensitive", "Maquinista APPROVE", "approve", nil, true},
		{"args", "/maquinista approve --force now", "approve", []string{"--force", "now"}, true},
		{"prose after first line", "/maquinista approve\n\nlooks great, thanks", "approve", nil, true},
		{"blank lines first", "\n \nmaquinista approve", "approve", nil, true},
		{"future verb parses", "maquinista park t-123", "park", []string{"t-123"}, true},
		{"prose prefix not a command", "hey maquinista approve", "", nil, false},
		{"hyphenated word", "maquinista-foo approve", "", nil, false},
		{"verb required", "maquinista", "", nil, false},
		{"embedded mid-line", "please maquinista approve", "", nil, false},
		{"empty", "", "", nil, false},
		{"whitespace only", "  \n  ", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verb, args, ok := ParseCommentCommand(tc.body)
			if ok != tc.ok || verb != tc.verb {
				t.Fatalf("ParseCommentCommand(%q) = %q,%v; want %q,%v", tc.body, verb, ok, tc.verb, tc.ok)
			}
			if len(args) != len(tc.args) {
				t.Fatalf("args = %v, want %v", args, tc.args)
			}
			for i := range args {
				if args[i] != tc.args[i] {
					t.Fatalf("args = %v, want %v", args, tc.args)
				}
			}
		})
	}
}

// ---- config ----

func TestGhCommandsConfigFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := GhCommandsConfigFromEnv()
		if cfg.Interval != DefaultGhCommentsPoll {
			t.Errorf("Interval = %s, want %s", cfg.Interval, DefaultGhCommentsPoll)
		}
		if len(cfg.AllowedLogins) != 0 {
			t.Errorf("AllowedLogins = %v, want empty (collaborator fallback)", cfg.AllowedLogins)
		}
	})
	t.Run("floor at 30s (rate-limit safety, AC 6)", func(t *testing.T) {
		t.Setenv("PIPELINE_GH_COMMENTS_POLL", "5s")
		if cfg := GhCommandsConfigFromEnv(); cfg.Interval != MinGhCommentsPoll {
			t.Errorf("Interval = %s, want floor %s", cfg.Interval, MinGhCommentsPoll)
		}
	})
	t.Run("allowlist parsing", func(t *testing.T) {
		t.Setenv("PIPELINE_GH_ALLOWED_LOGINS", "alice, bob;;carol  dave")
		cfg := GhCommandsConfigFromEnv()
		want := []string{"alice", "bob", "carol", "dave"}
		if len(cfg.AllowedLogins) != len(want) {
			t.Fatalf("AllowedLogins = %v, want %v", cfg.AllowedLogins, want)
		}
		for i := range want {
			if cfg.AllowedLogins[i] != want[i] {
				t.Fatalf("AllowedLogins = %v, want %v", cfg.AllowedLogins, want)
			}
		}
	})
}

// ---- dispatch: extensibility, auth, resolution, idempotency ----

func commentDepsForTest(pool *pgxpool.Pool, src CommentSource, logins ...string) CommentDeps {
	return CommentDeps{
		Pool:   pool,
		Source: src,
		Auth:   GhCommandsConfig{AllowedLogins: logins, Interval: time.Minute},
		Merge:  MergeConfig{Mode: MergeModeLocal},
	}
}

func TestDispatch_ExtensibleVerbs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	src := &fakeComments{}
	d := commentDepsForTest(pool, src, "alice")

	// Resolution is shared surface: every command resolves its target
	// before the verb handler runs, so give PR #7 a task.
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ($1, 'probe target', 'ready_to_merge', 'https://github.com/o/r/pull/7')
	`, taskID)

	calls := registerTestVerb(t)
	c := PRComment{ID: 1, Author: "alice", Body: "maquinista probe t-1 extra", CreatedAt: time.Now()}

	disp, err := DispatchCommentCommand(ctx, d, nil, 7, c)
	if err != nil || disp != DispOK {
		t.Fatalf("disp = %q err = %v, want ok", disp, err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("handler called %d times, want 1", len(got))
	}
	hc := got[0]
	if hc.TaskID != taskID || hc.PR != 7 || hc.Actor != "alice" {
		t.Errorf("context = %+v, want task %s", hc, taskID)
	}
	if len(hc.Args) != 2 || hc.Args[0] != "t-1" || hc.Args[1] != "extra" {
		t.Errorf("Args = %v, want [t-1 extra]", hc.Args)
	}
	// The comment is claimed — audit row exists with verb + actor.
	var verb, actor, dispRow string
	if err := pool.QueryRow(ctx,
		`SELECT verb, actor, disposition FROM gh_comment_commands WHERE comment_id = 1`).
		Scan(&verb, &actor, &dispRow); err != nil {
		t.Fatalf("claim row: %v", err)
	}
	if verb != "probe" || actor != "alice" || dispRow != DispOK {
		t.Errorf("row = %s/%s/%s", verb, actor, dispRow)
	}
}

func TestDispatch_Auth(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var fired int
	RegisterCommentVerb("probe", func(context.Context, CommentContext) error { fired++; return nil })
	t.Cleanup(func() { unregisterCommentVerb("probe") })

	// A resolvable target: commands that pass auth resolve to this task.
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ($1, 'auth target', 'ready_to_merge', 'https://github.com/o/r/pull/7')
	`, taskID)
	_ = taskID

	t.Run("allowlist member runs", func(t *testing.T) {
		d := commentDepsForTest(pool, &fakeComments{}, "alice")
		disp, err := DispatchCommentCommand(ctx, d, nil, 7,
			PRComment{ID: 101, Author: "alice", Body: "maquinista probe"})
		if err != nil || disp != DispOK || fired != 1 {
			t.Fatalf("disp=%q err=%v fired=%d", disp, err, fired)
		}
	})
	t.Run("non-member ignored silently but remembered", func(t *testing.T) {
		d := commentDepsForTest(pool, &fakeComments{}, "alice")
		disp, err := DispatchCommentCommand(ctx, d, nil, 7,
			PRComment{ID: 102, Author: "eve", Body: "maquinista probe"})
		if err != nil || disp != DispUnauthorized || fired != 1 {
			t.Fatalf("disp=%q err=%v fired=%d", disp, err, fired)
		}
		var row string
		if err := pool.QueryRow(ctx,
			`SELECT disposition FROM gh_comment_commands WHERE comment_id = 102`).Scan(&row); err != nil || row != DispUnauthorized {
			t.Fatalf("row=%q err=%v", row, err)
		}
	})
	t.Run("collaborator fallback", func(t *testing.T) {
		// No allowlist: gh collaborator check decides.
		d := commentDepsForTest(pool, &fakeComments{collaborators: map[string]bool{"carol": true}})
		disp, err := DispatchCommentCommand(ctx, d, nil, 7,
			PRComment{ID: 103, Author: "carol", Body: "maquinista probe"})
		if err != nil || disp != DispOK || fired != 2 {
			t.Fatalf("carol: disp=%q err=%v fired=%d", disp, err, fired)
		}
		disp, err = DispatchCommentCommand(ctx, d, nil, 7,
			PRComment{ID: 104, Author: "mallory", Body: "maquinista probe"})
		if err != nil || disp != DispUnauthorized || fired != 2 {
			t.Fatalf("mallory: disp=%q err=%v fired=%d", disp, err, fired)
		}
	})
	t.Run("transient auth error retries, claims nothing", func(t *testing.T) {
		d := commentDepsForTest(pool, &fakeComments{collabErr: errors.New("gh down")})
		disp, err := DispatchCommentCommand(ctx, d, nil, 7,
			PRComment{ID: 105, Author: "dave", Body: "maquinista probe"})
		if err == nil || disp != "" {
			t.Fatalf("disp=%q err=%v, want transient error", disp, err)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM gh_comment_commands WHERE comment_id = 105`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("claimed rows = %d err=%v, want 0", n, err)
		}
	})
}

func TestDispatch_NonCommandIgnored(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	d := commentDepsForTest(pool, &fakeComments{}, "alice")

	disp, err := DispatchCommentCommand(ctx, d, nil, 7,
		PRComment{ID: 201, Author: "alice", Body: "LGTM, shipping this 🚀"})
	if err != nil || disp != "" {
		t.Fatalf("disp=%q err=%v, want ignored entirely", disp, err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM gh_comment_commands WHERE comment_id = 201`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("non-command claimed: %d rows", n)
	}
}

func TestDispatch_TargetResolution(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	src := &fakeComments{headBranch: map[int]string{8: "t-task-br/feature"}}
	d := commentDepsForTest(pool, src, "alice")

	var fired []string
	RegisterCommentVerb("probe", func(_ context.Context, hc CommentContext) error {
		fired = append(fired, hc.TaskID)
		return nil
	})
	t.Cleanup(func() { unregisterCommentVerb("probe") })

	// Task with the canonical pr_url.
	rtm := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ($1, 'pr task', 'ready_to_merge', 'https://github.com/o/r/pull/7')
	`, rtm)
	// Task resolvable only through the branch fallback.
	branchTask := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ($1, 'branch task', 'ready_to_merge', 'https://github.com/o/r/pull/99')
	`, branchTask)
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, commit_sha)
		VALUES ($1, 'merger', 't-task-br/feature', '/tmp/x', 'main', '')
	`, branchTask)

	disp, err := DispatchCommentCommand(ctx, d, nil, 7,
		PRComment{ID: 301, Author: "alice", Body: "maquinista probe"})
	if err != nil || disp != DispOK {
		t.Fatalf("pr_url hit: disp=%q err=%v", disp, err)
	}
	if len(fired) != 1 || fired[0] != rtm {
		t.Fatalf("resolved %v, want [%s]", fired, rtm)
	}

	// Branch fallback.
	disp, err = DispatchCommentCommand(ctx, d, nil, 8,
		PRComment{ID: 302, Author: "alice", Body: "maquinista probe"})
	if err != nil || disp != DispOK {
		t.Fatalf("branch fallback: disp=%q err=%v", disp, err)
	}
	if len(fired) != 2 || fired[1] != branchTask {
		t.Fatalf("resolved %v, want [%s %s]", fired, rtm, branchTask)
	}

	// Unknown PR → one clean no-op, claimed.
	disp, err = DispatchCommentCommand(ctx, d, nil, 42,
		PRComment{ID: 303, Author: "alice", Body: "maquinista probe"})
	if err != nil || disp != DispNoOp {
		t.Fatalf("unknown PR: disp=%q err=%v", disp, err)
	}
	if len(fired) != 2 {
		t.Fatalf("handler ran on unknown PR")
	}
	var detail string
	if err := pool.QueryRow(ctx,
		`SELECT detail FROM gh_comment_commands WHERE comment_id = 303`).Scan(&detail); err != nil || detail == "" {
		t.Fatalf("no-op detail=%q err=%v", detail, err)
	}
}

func TestDispatch_Idempotency(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	d := commentDepsForTest(pool, &fakeComments{}, "alice")

	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ('t-idem', 'idem target', 'ready_to_merge', 'https://github.com/o/r/pull/7')
	`)

	var fired int
	RegisterCommentVerb("probe", func(context.Context, CommentContext) error { fired++; return nil })
	t.Cleanup(func() { unregisterCommentVerb("probe") })

	c := PRComment{ID: 401, Author: "alice", Body: "maquinista probe", CreatedAt: time.Now()}
	if disp, err := DispatchCommentCommand(ctx, d, nil, 7, c); err != nil || disp != DispOK {
		t.Fatalf("first: disp=%q err=%v", disp, err)
	}
	// The exact same comment re-delivered (restart catch-up, poll overlap).
	disp, err := DispatchCommentCommand(ctx, d, nil, 7, c)
	if err != nil || disp != "duplicate" {
		t.Fatalf("second: disp=%q err=%v, want duplicate", disp, err)
	}
	if fired != 1 {
		t.Fatalf("handler fired %d times, want exactly 1", fired)
	}
}

// ---- approve verb, end-to-end through the real merge flow ----

func TestApproveComment_MergesWithoutIds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	admin, worktree := initRemoteTrio(t, "approve-comment")
	fGh := &fakeGh{checks: ChecksGreen}
	src := &fakeComments{}
	d := CommentDeps{
		Pool:   pool,
		Source: src,
		Auth:   GhCommandsConfig{AllowedLogins: []string{"alice"}},
		Merge:  MergeConfig{Mode: MergeModeGH, Gh: fGh},
	}

	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, pr_state, metadata)
		VALUES ($1, 'id-less merge', 'ready_to_merge', $2,
		        'https://github.com/maquinista-labs/maquinista/pull/99', 'open',
		        '{"ticket_issue_id":"issue-1"}')
	`, taskID, worktree)

	c := PRComment{ID: 501, Author: "alice", Body: "/maquinista approve", CreatedAt: time.Now()}
	disp, err := DispatchCommentCommand(ctx, d, nil, 99, c)
	if err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v", disp, err)
	}
	if fGh.mergeCalls != 1 {
		t.Fatalf("mergeCalls = %d, want 1 (AC 1: no task id anywhere in the command)", fGh.mergeCalls)
	}
	var status, prState string
	if err := pool.QueryRow(ctx,
		`SELECT status, pr_state FROM tasks WHERE id = $1`, taskID).Scan(&status, &prState); err != nil {
		t.Fatal(err)
	}
	if status != "done" || prState != "merged" {
		t.Errorf("task = %s/%s, want done/merged", status, prState)
	}
	if len(src.reactions) != 1 || src.reactions[0] != 501 {
		t.Errorf("reactions = %v, want ack on comment 501", src.reactions)
	}
	_ = admin

	// Duplicate command comment → no double-merge (AC 3).
	disp, err = DispatchCommentCommand(ctx, d, nil, 99, c)
	if err != nil || disp != "duplicate" {
		t.Fatalf("duplicate: disp=%q err=%v", disp, err)
	}
	if fGh.mergeCalls != 1 {
		t.Fatalf("mergeCalls = %d after duplicate, want 1", fGh.mergeCalls)
	}
}

func TestApproveComment_NoOpWhenNotReadyToMerge(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	fGh := &fakeGh{checks: ChecksGreen}
	d := CommentDeps{
		Pool:   pool,
		Source: &fakeComments{},
		Auth:   GhCommandsConfig{AllowedLogins: []string{"alice"}},
		Merge:  MergeConfig{Mode: MergeModeGH, Gh: fGh},
	}

	// Same shape as approve, but the task sits in review — the verb must be
	// a clean single no-op with zero state damage (AC 4).
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ($1, 'still in review', 'review', 'https://github.com/o/r/pull/7')
	`, taskID)

	disp, err := DispatchCommentCommand(ctx, d, nil, 7,
		PRComment{ID: 601, Author: "alice", Body: "maquinista approve"})
	if err != nil || disp != DispNoOp {
		t.Fatalf("disp=%q err=%v, want clean no-op", disp, err)
	}
	if fGh.mergeCalls != 0 {
		t.Fatalf("mergeCalls = %d, want 0", fGh.mergeCalls)
	}
	var status, detail string
	if err := pool.QueryRow(ctx,
		`SELECT status, (SELECT detail FROM gh_comment_commands WHERE comment_id = 601)
		 FROM tasks WHERE id = $1`, taskID).Scan(&status, &detail); err != nil {
		t.Fatal(err)
	}
	if status != "review" || detail == "" {
		t.Errorf("status=%q detail=%q, want review + no-op detail", status, detail)
	}
}

// ---- poller ----

func TestPollPRCommands_CursorAndExactlyOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ('t-cursor', 'watched', 'ready_to_merge', 'https://github.com/o/r/pull/5')
	`)

	base := time.Now().UTC().Add(-time.Hour)
	cmdAt := base.Add(1 * time.Minute)
	noiseAt := base.Add(2 * time.Minute)
	src := &fakeComments{comments: map[int][]PRComment{
		5: {
			{ID: 701, Author: "mallory", Body: "unrelated chatter", CreatedAt: noiseAt},
			{ID: 702, Author: "alice", Body: "maquinista probe", CreatedAt: cmdAt},
		},
	}}
	var fired int
	RegisterCommentVerb("probe", func(context.Context, CommentContext) error { fired++; return nil })
	t.Cleanup(func() { unregisterCommentVerb("probe") })

	d := commentDepsForTest(pool, src, "alice")
	authz := newCommentAuthorizer(d.Auth)

	cursor, err := PollPRCommands(ctx, d, authz, base)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d after pass 1, want 1", fired)
	}
	if !cursor.Equal(noiseAt) {
		t.Fatalf("cursor = %s, want %s (advances past non-command comments too)", cursor, noiseAt)
	}

	// Pass 2 from the advanced cursor: nothing new, nothing re-fetched.
	cursor2, err := PollPRCommands(ctx, d, authz, cursor)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired = %d after pass 2, want still 1 (exactly-once)", fired)
	}
	if !cursor2.Equal(cursor) {
		t.Fatalf("cursor moved on empty pass: %s → %s", cursor, cursor2)
	}
}

func TestPollPRCommands_FetchErrorHoldsCursor(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ('t-cursor2', 'watched', 'ready_to_merge', 'https://github.com/o/r/pull/6')
	`)

	base := time.Now().UTC().Add(-time.Hour)
	boom := errors.New("gh rate limited")
	src := &fakeComments{
		comments: map[int][]PRComment{
			6: {{ID: 801, Author: "alice", Body: "maquinista probe", CreatedAt: base.Add(time.Minute)}},
		},
		failPRs: map[int]error{6: boom},
	}
	d := commentDepsForTest(pool, src, "alice")
	authz := newCommentAuthorizer(d.Auth)

	// A failing fetch holds the cursor AND dispatches nothing: the same
	// window is re-read next pass, the claim keeps it exactly-once.
	cursor, err := PollPRCommands(ctx, d, authz, base)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
	if !cursor.Equal(base) {
		t.Fatalf("cursor advanced on failed pass: %s", cursor)
	}

	// Next pass, GitHub healthy again: the comment lands exactly once.
	src.mu.Lock()
	src.failPRs = nil
	src.mu.Unlock()
	cursor, err = PollPRCommands(ctx, d, authz, cursor)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if !cursor.After(base) {
		t.Fatalf("cursor did not advance on clean pass: %s", cursor)
	}
}

// ---- resolve verb ----

// fakeMergerSpawner records ReviewSpawnParams; err short-circuits the spawn.
// insertRow materializes the agents row the real SpawnFresh pre-registers —
// the resolve prompt enqueues to agent_inbox, whose FK needs the row (EX-04
// pitfall (i)).
type fakeMergerSpawner struct {
	t         *testing.T
	pool      *pgxpool.Pool
	insertRow bool
	calls     []ReviewSpawnParams
	err       error
}

func (f *fakeMergerSpawner) SpawnReviewer(ctx context.Context, p ReviewSpawnParams) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, p)
	if f.insertRow {
		execOK(f.t, f.pool, `
			INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
			                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
			VALUES ($1, 'sess', $1, $5, $2, 'running', $3, $4, $1, NOW(), NOW(), FALSE)
		`, p.AgentID, p.TaskID, p.RunnerType, p.WorktreePath, p.Role)
	}
	return nil
}

// seedResolveTask inserts a parked task with a worktree and a conflicted
// merge_queue entry (the EX-05 park shape).
func seedResolveTask(t *testing.T, pool *pgxpool.Pool, status string) (taskID, worktree string) {
	t.Helper()
	taskID = fmt.Sprintf("t-%d", nextTaskNum())
	worktree = t.TempDir()
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url)
		VALUES ($1, 'parked pr', $2, $3, 'https://github.com/o/r/pull/9')
	`, taskID, status, worktree)
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, status, conflict_files)
		VALUES ($1, 'w-1', 'feat/parked', $2, 'main', 'conflict', '{internal/comments.go,cmd/main.go}')
	`, taskID, worktree)
	return taskID, worktree
}

func TestResolveComment_SpawnsMergerSession(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	taskID, worktree := seedResolveTask(t, pool, "pending_approval")
	src := &fakeComments{}
	sp := &fakeMergerSpawner{t: t, pool: pool, insertRow: true}
	d := CommentDeps{
		Pool:   pool,
		Source: src,
		Auth:   GhCommandsConfig{AllowedLogins: []string{"alice"}},
		Merge:  MergeConfig{Mode: MergeModeGH},
		Spawn:  sp,
	}

	disp, err := DispatchCommentCommand(ctx, d, nil, 9,
		PRComment{ID: 701, Author: "alice", Body: "maquinista resolve"})
	if err != nil || disp != DispOK {
		t.Fatalf("disp=%q err=%v", disp, err)
	}
	// Session spawned with the merger role + soul, in the task worktree.
	if len(sp.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(sp.calls))
	}
	call := sp.calls[0]
	if call.Role != "merger" || call.SoulTemplateID != MergerSoulTemplate {
		t.Errorf("role/template = %s/%s, want merger/%s", call.Role, call.SoulTemplateID, MergerSoulTemplate)
	}
	if call.WorktreePath != worktree {
		t.Errorf("worktree = %q, want %q", call.WorktreePath, worktree)
	}
	if call.TaskID != taskID || !strings.HasPrefix(call.AgentID, "merger-"+taskID) {
		t.Errorf("agent = %s task = %s, want merger-<task> prefix", call.AgentID, taskID)
	}
	// The resolve prompt is enqueued (dedup id carries the comment) and
	// names the parked branch + conflict files.
	var prompt string
	if err := pool.QueryRow(ctx,
		`SELECT content->>'prompt' FROM agent_inbox
		 WHERE agent_id = $1 AND external_msg_id = $2`,
		call.AgentID, fmt.Sprintf("resolve:%s:701", taskID)).Scan(&prompt); err != nil {
		t.Fatalf("resolve prompt row: %v", err)
	}
	for _, want := range []string{"feat/parked", "internal/comments.go", "--force-with-lease", "do not merge"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The 'merge' audit marker exists.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_context WHERE task_id = $1 AND kind = 'merge'`, taskID).Scan(&n); err != nil || n != 1 {
		t.Errorf("merge markers = %d err=%v, want 1", n, err)
	}
	// Ack + task untouched (still parked; the session does the moving).
	if len(src.reactions) != 1 || src.reactions[0] != 701 {
		t.Errorf("reactions = %v, want ack on comment 701", src.reactions)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status); err != nil || status != "pending_approval" {
		t.Errorf("status = %q err=%v, want pending_approval", status, err)
	}
}

func TestResolveComment_NoOpWrongState(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seedResolveTask(t, pool, "review") // parked shape, wrong state
	sp := &fakeMergerSpawner{}
	d := CommentDeps{
		Pool:   pool,
		Source: &fakeComments{},
		Auth:   GhCommandsConfig{AllowedLogins: []string{"alice"}},
		Merge:  MergeConfig{Mode: MergeModeGH},
		Spawn:  sp,
	}
	disp, err := DispatchCommentCommand(ctx, d, nil, 9,
		PRComment{ID: 702, Author: "alice", Body: "maquinista resolve"})
	if err != nil || disp != DispNoOp {
		t.Fatalf("disp=%q err=%v, want clean no-op", disp, err)
	}
	if len(sp.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0", len(sp.calls))
	}
}

func TestResolveComment_NoWorktreeNoOp(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, pr_url)
		VALUES ($1, 'no worktree', 'pending_approval', 'https://github.com/o/r/pull/11')
	`, fmt.Sprintf("t-%d", nextTaskNum()))
	sp := &fakeMergerSpawner{}
	d := CommentDeps{
		Pool:   pool,
		Source: &fakeComments{},
		Auth:   GhCommandsConfig{AllowedLogins: []string{"alice"}},
		Merge:  MergeConfig{Mode: MergeModeGH},
		Spawn:  sp,
	}
	disp, err := DispatchCommentCommand(ctx, d, nil, 11,
		PRComment{ID: 703, Author: "alice", Body: "maquinista resolve"})
	if err != nil || disp != DispNoOp {
		t.Fatalf("disp=%q err=%v, want clean no-op", disp, err)
	}
	if len(sp.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0", len(sp.calls))
	}
}

func TestResolveComment_SpawnErrorNoEpisode(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	taskID, _ := seedResolveTask(t, pool, "pending_approval")
	sp := &fakeMergerSpawner{err: fmt.Errorf("uq_agents_task_live")}
	d := CommentDeps{
		Pool:   pool,
		Source: &fakeComments{},
		Auth:   GhCommandsConfig{AllowedLogins: []string{"alice"}},
		Merge:  MergeConfig{Mode: MergeModeGH},
		Spawn:  sp,
	}
	disp, err := DispatchCommentCommand(ctx, d, nil, 9,
		PRComment{ID: 704, Author: "alice", Body: "maquinista resolve"})
	if err == nil || disp != DispError {
		t.Fatalf("disp=%q err=%v, want error disposition", disp, err)
	}
	// Spawn-before-record ordering: a failed spawn leaves no episode rows.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_context WHERE task_id = $1 AND kind = 'merge'`, taskID).Scan(&n); err != nil || n != 0 {
		t.Errorf("merge markers = %d err=%v, want 0", n, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_inbox WHERE external_msg_id = $1`,
		fmt.Sprintf("resolve:%s:704", taskID)).Scan(&n); err != nil || n != 0 {
		t.Errorf("inbox rows = %d err=%v, want 0", n, err)
	}
}
