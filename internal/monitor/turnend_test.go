package monitor

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// --- fixtures ---

// Turn-end fixture shapes. The close fixture is r8's real close (MAQ-37,
// verified live on barceloneta 08/10): the transcript ends in a bare
// assistant message — thinking + text blocks, stopReason "stop", no
// toolCall. Mid-turn shapes leave a tool call pending or await the next
// assistant hop.

const (
	teSessionID = "01a11a87-1801-771e-965d-151d77c351d9"
	teFileName  = "2026-10-08T08-00-23-554Z_" + teSessionID + ".jsonl"
)

func teHeader() string {
	return `{"type":"session","version":3,"id":"` + teSessionID + `","timestamp":"2026-10-08T08:00:23.554Z","cwd":"/tmp/proj"}`
}

func teUser(id, ts, text string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"` + ts + `","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}],"timestamp":1}}`
}

// teAssistantToolCall is a mid-turn assistant message: it ends with a
// pending tool call (r8's transcript carries 22 of these, stopReason
// "toolUse").
func teAssistantToolCall(id, ts string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"running the merge"},{"type":"toolCall","id":"call_1","name":"bash","arguments":{"cmd":"go test"}}],"stopReason":"toolUse","timestamp":1}}`
}

// teToolResult is the tool's answer — the turn is still open (the next
// assistant hop has not landed yet).
func teToolResult(id, ts string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"` + ts + `","message":{"role":"toolResult","toolCallId":"call_1","toolName":"bash","content":[{"type":"text","text":"ok"}],"timestamp":1}}`
}

// teAssistantClose is r8's real close shape: bare assistant message at
// EOF, no pending tool call, stopReason "stop".
func teAssistantClose(id, ts string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"thinking","thinking":"work is complete"},{"type":"text","text":"MAQ-37 fixer work is complete. PR #44 left clean."}],"api":"openai-responses","provider":"openai","model":"x","usage":{"input":10,"output":5},"stopReason":"stop","timestamp":1}}`
}

func teMetadata(id, ts string) string {
	return `{"type":"model_change","id":"` + id + `","parentId":"p","timestamp":"` + ts + `","provider":"openai","modelId":"gpt-5.4"}`
}

// writeTEFixture writes lines to a fresh pi session file and returns its path.
func writeTEFixture(t *testing.T, lines ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "--tmp-proj--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, teFileName)
	body := teHeader() + "\n"
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- tail scan ---

func TestPiTailTurnEnd(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		wantEnded bool
		wantID    string
		wantAt    string // RFC3339, "" = zero ok
	}{
		{
			name:      "r8 close shape — bare assistant message at EOF",
			lines:     []string{teUser("m1", "2026-10-08T08:00:38.509Z", "fix the gate"), teAssistantToolCall("m2", "2026-10-08T08:01:00.000Z"), teToolResult("m3", "2026-10-08T08:01:05.000Z"), teAssistantClose("m4", "2026-10-08T08:08:30.000Z")},
			wantEnded: true,
			wantID:    "m4",
			wantAt:    "2026-10-08T08:08:30.000Z",
		},
		{
			name:      "mid-turn — pending tool call at EOF",
			lines:     []string{teUser("m1", "2026-10-08T08:00:38.509Z", "fix the gate"), teAssistantToolCall("m2", "2026-10-08T08:01:00.000Z")},
			wantEnded: false,
		},
		{
			name:      "mid-turn — toolResult at EOF awaits next assistant hop",
			lines:     []string{teUser("m1", "2026-10-08T08:00:38.509Z", "fix the gate"), teAssistantToolCall("m2", "2026-10-08T08:01:00.000Z"), teToolResult("m3", "2026-10-08T08:01:05.000Z")},
			wantEnded: false,
		},
		{
			name:      "turn just started — user prompt at EOF",
			lines:     []string{teUser("m1", "2026-10-08T08:00:38.509Z", "fix the gate")},
			wantEnded: false,
		},
		{
			name:      "metadata after close does not mask it",
			lines:     []string{teAssistantClose("m1", "2026-10-08T08:08:30.000Z"), teMetadata("d1", "2026-10-08T08:08:31.000Z")},
			wantEnded: true,
			wantID:    "m1",
			wantAt:    "2026-10-08T08:08:30.000Z",
		},
		{
			name:      "partial final line (pi mid-append) is invisible",
			lines:     []string{teToolResult("m1", "2026-10-08T08:01:05.000Z")},
			wantEnded: false,
		},
		{
			name:      "close without stopReason still ends (structural rule is primary)",
			lines:     []string{teUser("m1", "2026-10-08T08:00:38.509Z", "go"), `{"type":"message","id":"m2","timestamp":"2026-10-08T08:01:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`},
			wantEnded: true,
			wantID:    "m2",
			wantAt:    "2026-10-08T08:01:00.000Z",
		},
		{
			name:      "entry without id falls back to timestamp as dedup key",
			lines:     []string{`{"type":"message","timestamp":"2026-10-08T08:01:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`},
			wantEnded: true,
			wantID:    "2026-10-08T08:01:00.000Z",
			wantAt:    "2026-10-08T08:01:00.000Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTEFixture(t, tc.lines...)
			if tc.name == "partial final line (pi mid-append) is invisible" {
				// Append the closing message WITHOUT its trailing newline:
				// pi is mid-append, the line is not complete, the previous
				// complete state (toolResult) decides.
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				f.WriteString(teAssistantClose("m2", "2026-10-08T08:08:30.000Z")) // no \n
				f.Close()
			}
			got := piTailTurnEnd(path)
			if got.Ended != tc.wantEnded {
				t.Fatalf("ended = %v, want %v (result: %+v)", got.Ended, tc.wantEnded, got)
			}
			if tc.wantEnded {
				if got.MsgID != tc.wantID {
					t.Errorf("msgID = %q, want %q", got.MsgID, tc.wantID)
				}
				wantAt, _ := time.Parse(time.RFC3339, tc.wantAt)
				if !got.At.Equal(wantAt) {
					t.Errorf("at = %v, want %v", got.At, wantAt)
				}
			}
		})
	}
}

func TestPiTailTurnEnd_MissingFile(t *testing.T) {
	got := piTailTurnEnd(filepath.Join(t.TempDir(), "nope.jsonl"))
	if got.Ended {
		t.Fatalf("ended = true for missing file, want false")
	}
}

func TestPiTailTurnEnd_NoCompleteLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.jsonl")
	// A single unterminated line: nothing complete to classify.
	partial := "{\"type\":\"message\",\"id\":\"m1\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"d"
	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := piTailTurnEnd(path); got.Ended {
		t.Fatalf("ended = true for unterminated-only file, want false")
	}
}

// --- PiSource.TailTurnEnd (the TurnEndInspector impl) ---

func TestPiSource_TailTurnEnd(t *testing.T) {
	path := writeTEFixture(t, teUser("m1", "2026-10-08T08:00:38.509Z", "go"), teAssistantClose("m2", "2026-10-08T08:08:30.000Z"))

	p := NewPiSource(nil, nil, state.NewState(), state.NewMonitorState())
	p.sessionRoot = filepath.Dir(filepath.Dir(path))
	p.lastSessionMap = map[string]state.SessionMapEntry{
		"sess:@1": {SessionID: teSessionID, CWD: "/tmp/proj"},
	}

	msgID, at, ended := p.TailTurnEnd(ActiveSession{Key: "sess:@1", WindowID: "@1"})
	if !ended || msgID != "m2" || at.IsZero() {
		t.Fatalf("TailTurnEnd = %q %v %v, want m2/fresh/true", msgID, at, ended)
	}

	// Unbound session: no entry in the session map — never reports a close.
	if _, _, ended := p.TailTurnEnd(ActiveSession{Key: "sess:@9", WindowID: "@9"}); ended {
		t.Fatalf("unbound session reported a turn end")
	}
}

// --- detectTurnEnd (shadow emission) ---

// stubTurnEndSource feeds detectTurnEnd without transcript files.
type stubTurnEndSource struct {
	TranscriptSource
	msgID string
	at    time.Time
	ended bool
}

func (s stubTurnEndSource) Name() string { return "stub" }
func (s stubTurnEndSource) TailTurnEnd(ActiveSession) (string, time.Time, bool) {
	return s.msgID, s.at, s.ended
}

// captureLog redirects the std logger into a buffer for the duration of f.
func captureLog(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	out := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	}()
	f()
	return buf.String()
}

// seedTurnEndTask + seedTurnEndAgent build the DB rows a pipeline agent needs.
func seedTurnEndTask(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tasks (id, title, status) VALUES ($1, 'turn-end fixture task', 'claimed') ON CONFLICT DO NOTHING`, id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func seedTurnEndAgent(t *testing.T, pool *pgxpool.Pool, id, window, role, taskID, status string) {
	t.Helper()
	// Empty taskID → NULL: user topic agents carry no task (FK forbids '').
	var taskArg any
	if taskID != "" {
		taskArg = taskID
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO agents (id, tmux_session, tmux_window, role, status, runner_type, task_id)
		VALUES ($1, 'sess', $2, $3, $4, 'pi', $5)
	`, id, window, role, status, taskArg); err != nil {
		t.Fatalf("seed agent %s: %v", id, err)
	}
}

// newTurnEndMonitor builds a Monitor suitable for direct detectTurnEnd calls.
func newTurnEndMonitor(pool *pgxpool.Pool) *Monitor {
	return &Monitor{pool: pool, pollInterval: 50 * time.Millisecond}
}

func turnEndLogs(t *testing.T, m *Monitor, src TranscriptSource, sess ActiveSession) string {
	t.Helper()
	var out string
	out = captureLog(t, func() { m.detectTurnEnd(src, sess) })
	return out
}

func TestDetectTurnEnd_ShadowFiresOncePerClose(t *testing.T) {
	pool := migratedPool(t)
	seedTurnEndTask(t, pool, "task-te-1")
	seedTurnEndAgent(t, pool, "impl-te-1", "@77", "implementor", "task-te-1", "running")
	m := newTurnEndMonitor(pool)
	sess := ActiveSession{Key: "sess:@77", WindowID: "@77"}
	fresh := time.Now().Add(-10 * time.Millisecond)

	// First sight of the closing message: exactly one structured line.
	out := turnEndLogs(t, m, stubTurnEndSource{msgID: "m4", at: fresh, ended: true}, sess)
	for _, want := range []string{"turn-end detected (shadow)", "window=@77", "agent=impl-te-1", "task=task-te-1", "role=implementor", "msg=m4", "total=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got: %s", want, out)
		}
	}

	// Same tail on the next poll: deduped — no second line.
	out = turnEndLogs(t, m, stubTurnEndSource{msgID: "m4", at: fresh, ended: true}, sess)
	if strings.Contains(out, "turn-end detected") {
		t.Errorf("dedup failed, second fire logged: %s", out)
	}

	// A NEW closing message fires again and bumps the counter.
	out = turnEndLogs(t, m, stubTurnEndSource{msgID: "m9", at: time.Now().Add(-5 * time.Millisecond), ended: true}, sess)
	if !strings.Contains(out, "msg=m9") || !strings.Contains(out, "total=2") {
		t.Errorf("second close not counted; got: %s", out)
	}
}

func TestDetectTurnEnd_NonPipelineAgentsDoNotFire(t *testing.T) {
	pool := migratedPool(t)
	seedTurnEndTask(t, pool, "task-te-2")
	// User topic agent: role=user, no task.
	seedTurnEndAgent(t, pool, "user-te", "@78", "user", "", "running")
	m := newTurnEndMonitor(pool)
	sess := ActiveSession{Key: "sess:@78", WindowID: "@78"}
	src := stubTurnEndSource{msgID: "m1", at: time.Now().Add(-10 * time.Millisecond), ended: true}

	if out := turnEndLogs(t, m, src, sess); strings.Contains(out, "turn-end detected") {
		t.Errorf("user topic agent fired: %s", out)
	}

	// Same window, dead pipeline row: not a live signal either.
	seedTurnEndAgent(t, pool, "impl-te-dead", "@79", "implementor", "task-te-2", "dead")
	if out := turnEndLogs(t, m, stubTurnEndSource{msgID: "m1", at: time.Now().Add(-10 * time.Millisecond), ended: true},
		ActiveSession{Key: "sess:@79", WindowID: "@79"}); strings.Contains(out, "turn-end detected") {
		t.Errorf("dead agent fired: %s", out)
	}

	// Unknown window (no agent row at all): silent.
	if out := turnEndLogs(t, m, stubTurnEndSource{msgID: "m1", at: time.Now().Add(-10 * time.Millisecond), ended: true},
		ActiveSession{Key: "sess:@80", WindowID: "@80"}); strings.Contains(out, "turn-end detected") {
		t.Errorf("unknown window fired: %s", out)
	}
}

func TestDetectTurnEnd_StaleCloseNotCounted(t *testing.T) {
	pool := migratedPool(t)
	seedTurnEndTask(t, pool, "task-te-3")
	seedTurnEndAgent(t, pool, "impl-te-3", "@81", "implementor", "task-te-3", "running")
	m := newTurnEndMonitor(pool)
	sess := ActiveSession{Key: "sess:@81", WindowID: "@81"}

	// Bind-time backlog replay: the whole transcript was re-read from 0,
	// the tail close is an hour old — not a live detection.
	old := time.Now().Add(-time.Hour)
	if out := turnEndLogs(t, m, stubTurnEndSource{msgID: "m1", at: old, ended: true}, sess); strings.Contains(out, "turn-end detected") {
		t.Errorf("stale close fired: %s", out)
	}

	// And the stale sight must not poison the dedup map: a fresh close
	// with the same id still fires.
	if out := turnEndLogs(t, m, stubTurnEndSource{msgID: "m1", at: time.Now().Add(-5 * time.Millisecond), ended: true}, sess); !strings.Contains(out, "total=1") {
		t.Errorf("fresh close after stale sight did not fire: %s", out)
	}
}

func TestDetectTurnEnd_NonInspectorSourceIsNoop(t *testing.T) {
	m := newTurnEndMonitor(nil)
	// A source that does not implement TurnEndInspector: no panic, no log.
	src := stubPlainSource{}
	if out := turnEndLogs(t, m, src, ActiveSession{Key: "s:@1", WindowID: "@1"}); strings.Contains(out, "turn-end detected") {
		t.Errorf("non-inspector source fired: %s", out)
	}
	// Nil pool with an inspector: cannot attribute — silent, no panic.
	if out := turnEndLogs(t, m, stubTurnEndSource{msgID: "m1", at: time.Now(), ended: true}, ActiveSession{Key: "s:@1", WindowID: "@1"}); strings.Contains(out, "turn-end detected") {
		t.Errorf("nil pool fired: %s", out)
	}
}

// stubPlainSource implements only TranscriptSource.
type stubPlainSource struct {
	TranscriptSource
}

func (stubPlainSource) Name() string { return "plain" }

// --- end-to-end: poll loop wiring ---

// piStamp formats a pi FILENAME timestamp (UTC, millis, ':'→'-') —
// e.g. 2026-10-08T08-00-23-554Z_….jsonl. Entry timestamps inside the
// JSONL are RFC3339 (colons); use teRFC for those.
func piStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15-04-05.000Z")
}

// teRFC formats an RFC3339 entry timestamp as pi writes it into JSONL
// lines ("2026-10-08T08:08:30.000Z") — parseable by time.RFC3339.
func teRFC(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func TestMonitorPoll_TurnEndShadowEndToEnd(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedTurnEndTask(t, pool, "task-te-e2e")
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, status, runner_type, task_id, session_id, cwd)
		VALUES ('impl-te-e2e', 'sess', '@88', 'implementor', 'running', 'pi', 'task-te-e2e', $1, '/tmp/proj')
	`, teSessionID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// A live transcript whose creation predates the pane by seconds (the
	// same-epoch case pi's binding rules accept): prompt → tool round →
	// fresh close, r8's shape.
	now := time.Now().UTC()
	dir := filepath.Join(t.TempDir(), "--tmp-proj--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, piStamp(now.Add(-300*time.Millisecond))+"_"+teSessionID+".jsonl")
	body := strings.Join([]string{
		teHeader(),
		teUser("m1", teRFC(now.Add(-250*time.Millisecond)), "fix the gate"),
		teAssistantToolCall("m2", teRFC(now.Add(-200*time.Millisecond))),
		teToolResult("m3", teRFC(now.Add(-150*time.Millisecond))),
		teAssistantClose("m4", teRFC(now.Add(-100*time.Millisecond))),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.NewState()
	st.SetWindowRunner("@88", "pi")
	ms := state.NewMonitorState()
	p := NewPiSource(&config.Config{MaquinistaDir: t.TempDir()}, pool, st, ms)
	p.sessionRoot = filepath.Dir(dir)

	m := &Monitor{
		config:       &config.Config{MaquinistaDir: t.TempDir()},
		state:        st,
		monitorState: ms,
		pool:         pool,
		pollInterval: 500 * time.Millisecond,
	}
	m.AddSource(p)

	// First poll: discovery binds the same-epoch transcript, reads it from
	// 0, and the fresh close fires exactly once.
	out := captureLog(t, func() { m.poll() })
	if n := strings.Count(out, "turn-end detected (shadow)"); n != 1 {
		t.Fatalf("first poll turn-end lines = %d, want 1; log:\n%s", n, out)
	}
	for _, want := range []string{"window=@88", "agent=impl-te-e2e", "task=task-te-e2e", "msg=m4", "total=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got:\n%s", want, out)
		}
	}

	// Second poll with no growth: deduped, no new line.
	out = captureLog(t, func() { m.poll() })
	if strings.Contains(out, "turn-end detected") {
		t.Errorf("idle poll re-fired:\n%s", out)
	}

	// A new turn closes (append user + close, both fresh): fires again,
	// total=2 — the "within one poll interval of transcript close" AC.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	appendBody := strings.Join([]string{
		teUser("m5", teRFC(now.Add(-40*time.Millisecond)), "one more thing"),
		teAssistantClose("m6", teRFC(now.Add(-20*time.Millisecond))),
	}, "\n") + "\n"
	f.WriteString(appendBody)
	f.Close()
	out = captureLog(t, func() { m.poll() })
	if !strings.Contains(out, "msg=m6") || !strings.Contains(out, "total=2") {
		t.Errorf("second close not detected; log:\n%s", out)
	}
}
