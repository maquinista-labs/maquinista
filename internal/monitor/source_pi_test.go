package monitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/state"
)

const (
	fixSessionID = "01a0c457-b71c-77af-b6a8-c874297e3cbd"
	fixSlugDir   = "--tmp-proj--"
	fixFileName  = "2026-09-21T14-00-00-000Z_" + fixSessionID + ".jsonl"
)

func newFixtureSource(t *testing.T) *PiSource {
	t.Helper()
	p := NewPiSource(nil, nil, state.NewState(), state.NewMonitorState())
	p.sessionRoot = filepath.Join("testdata", "pi", "sessions")
	return p
}

// --- SlugifyCWD (C18) ---

// Expected values match pi 0.73.1 session-manager.js and directories
// observed live in ~/.pi/agent/sessions on 21/09.
func TestPiSource_SlugifyCWD(t *testing.T) {
	cases := []struct{ cwd, want string }{
		{"/tmp/bench-pi", "--tmp-bench-pi--"},
		{"/tmp/proj", "--tmp-proj--"},
		{"/home/otavio/code/maquinista", "--home-otavio-code-maquinista--"},
		{"/", "----"}, // leading slash stripped, nothing left
		// No leading-slash strip for drive letters; alg is faithful to pi's JS.
		{"C:\\Users\\dev", "--C--Users-dev--"},
		{"/tmp/bench:pi", "--tmp-bench-pi--"}, // ":" also replaced
	}
	for _, c := range cases {
		if got := SlugifyCWD(c.cwd); got != c.want {
			t.Errorf("SlugifyCWD(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

// --- Header extraction (C14) ---

func TestPiSource_Header(t *testing.T) {
	p := newFixtureSource(t)
	path, id, err := p.discoverSessionFile("/tmp/proj", time.Time{})
	if err != nil {
		t.Fatalf("discoverSessionFile: %v", err)
	}
	if id != fixSessionID {
		t.Errorf("header id = %q, want %q", id, fixSessionID)
	}
	if !strings.Contains(path, filepath.Join("sessions", fixSlugDir)) {
		t.Errorf("path = %q (want %s slug dir)", path, fixSlugDir)
	}
	// The header cwd anchors discovery — assert it parses too.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var h piSessionHeader
	if err := json.NewDecoder(f).Decode(&h); err != nil {
		t.Fatalf("header decode: %v", err)
	}
	if h.Type != "session" || h.Version != 3 || h.CWD != "/tmp/proj" {
		t.Errorf("header = %+v", h)
	}
}

// --- Role preservation (C15) ---

func TestPiSource_Roles(t *testing.T) {
	lines := map[string]string{
		"user":      `{"type":"message","id":"m1","parentId":"p","timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"list the files"}],"timestamp":1790000401000}}`,
		"assistant": `{"type":"message","id":"m2","parentId":"m1","timestamp":"t","message":{"role":"assistant","content":[{"type":"text","text":"there are 3 files"}],"api":"openai-responses","provider":"openai","model":"gemini","usage":{"input":10,"output":5}}}`,
		"toolResult": `{"type":"message","id":"m3","parentId":"m2","timestamp":"t","message":{"role":"toolResult","content":[{"type":"text","text":"file-a\nfile-b"}],"timestamp":1}}`,
	}
	for role, line := range lines {
		pe := parsePiLine([]byte(line))
		if pe == nil {
			t.Errorf("%s: want entry, got nil", role)
			continue
		}
		if pe.Role != role {
			t.Errorf("%s: role = %q", role, pe.Role)
		}
	}
}

// --- Unparseable lines never surface as errors (C16) ---

func TestPiSource_SkipsUnparseable(t *testing.T) {
	lines := []string{
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-09-21T14:00:00.000Z","cwd":"/tmp/proj"}`,
		`{"type":"branch_summary","id":"d3","parentId":"d2","timestamp":"t","summary":"x"}`,
		`{"type":"model_change","id":"d1","parentId":null,"timestamp":"t","provider":"openai","modelId":"gpt-5.4"}`,
		`{"type":"message","id":"m4","parentId":"m3","timestamp":"t","message":{"role":"assistant","content":[],"api":"openai-responses"}}`, // empty content
		`{broken json`,
		``,
	}
	for _, line := range lines {
		if got := parsePiLine([]byte(line)); got != nil {
			t.Errorf("parsePiLine(%q) = %+v, want nil", line, got)
		}
	}
}

// --- No duplicates on re-read (C17) ---

func TestPiSource_NoDupOnReread(t *testing.T) {
	p := newFixtureSource(t)
	p.lastSessionMap["sess:win"] = state.SessionMapEntry{SessionID: fixSessionID, CWD: "/tmp/proj"}
	sess := ActiveSession{Key: "sess:win", WindowID: "win"}

	first, offset, err := p.ReadNewEntries(sess, 0)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("got %d entries, want 3", len(first))
	}
	toolResults := 0
	for _, e := range first {
		if e.ContentType == "tool_result" {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Fatalf("got %d tool-result entries on first read, want 1", toolResults)
	}

	second, _, err := p.ReadNewEntries(sess, offset)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	for _, e := range second {
		if e.ContentType == "tool_result" {
			t.Errorf("duplicate tool-result on re-read: %+v", e)
		}
	}
	if len(second) != 0 {
		t.Errorf("re-read emitted %d entries, want 0", len(second))
	}
}

// --- Offset-incremental reads (C19) ---

func TestPiSource_OffsetIncremental(t *testing.T) {
	// Copy the fixture into a temp store so we can append to it.
	root := t.TempDir()
	dir := filepath.Join(root, fixSlugDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "pi", "sessions", fixSlugDir, fixFileName))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fixFileName)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewPiSource(nil, nil, state.NewState(), state.NewMonitorState())
	p.sessionRoot = root
	p.lastSessionMap["sess:win"] = state.SessionMapEntry{SessionID: fixSessionID, CWD: "/tmp/proj"}
	sess := ActiveSession{Key: "sess:win", WindowID: "win"}

	first, offset, err := p.ReadNewEntries(sess, 0)
	if err != nil || len(first) != 3 {
		t.Fatalf("first read: %d entries, err %v", len(first), err)
	}

	// Append one more user message — simulates pi writing new turns.
	next := `{"type":"message","id":"m9","parentId":"m3","timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"second question"}],"timestamp":2}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(next); err != nil {
		t.Fatal(err)
	}
	f.Close()

	inc, offset2, err := p.ReadNewEntries(sess, offset)
	if err != nil {
		t.Fatalf("incremental read: %v", err)
	}
	if len(inc) != 1 || inc[0].Role != "user" || inc[0].Text != "second question" {
		t.Errorf("incremental = %+v (want exactly the appended user entry)", inc)
	}
	if offset2 <= offset {
		t.Errorf("offset did not advance: %d → %d", offset, offset2)
	}
}

// --- PiProfile (C11, C12) ---

func TestPiProfile_Empty(t *testing.T) {
	prof := PiProfile()
	if prof.SeparatorRunes != nil {
		t.Errorf("SeparatorRunes = %v, want nil", prof.SeparatorRunes)
	}
	if prof.UIPatterns != nil {
		t.Errorf("UIPatterns = %v, want nil", prof.UIPatterns)
	}
}

func TestPiProfile_HelpersOnPiPane(t *testing.T) {
	pane, err := os.ReadFile(filepath.Join("testdata", "pi", "pane_idle.txt"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	prof := PiProfile()
	// All shared helpers must run without error over a real captured pane…
	_ = StripPaneChromeFor(string(pane), prof)
	if status, ok := ExtractStatusLineFor(string(pane), prof); ok {
		t.Errorf("ExtractStatusLineFor matched (%q) on empty profile; want no match", status)
	}
	// …and must not classify the pane as an interactive UI.
	if IsInteractiveUIFor(string(pane), prof) {
		t.Error("IsInteractiveUIFor true on empty profile; want false (panes flow through unparsed)")
	}
	p := newFixtureSource(t)
	if p.IsInteractiveUI(string(pane)) {
		t.Error("PiSource.IsInteractiveUI true; want false")
	}
}

// --- Session binding without a hook (C24, C25) ---

// fixtureStore builds a session store rooted at dir with the standard fixture.
func fixtureStore(t *testing.T, dir string) {
	t.Helper()
	slug := filepath.Join(dir, fixSlugDir)
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "pi", "sessions", fixSlugDir, fixFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slug, fixFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPiSource_DiscoverBackfill(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	// The spawn path for a hookless runner: RegisterAgent writes the row,
	// session_id stays NULL, and the cwd lands in agents.cwd.
	if err := db.RegisterAgent(pool, "agent-pi-1", "maq", "w1", strPtr("/tmp/proj"), nil, "pi", nil, "executor"); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE agents SET cwd='/tmp/proj' WHERE id='agent-pi-1'`); err != nil {
		t.Fatal(err)
	}

	st := state.NewState()
	st.SetWindowRunner("w1", "pi")
	p := NewPiSource(nil, pool, st, state.NewMonitorState())
	p.sessionRoot = t.TempDir()
	fixtureStore(t, p.sessionRoot)

	sessions := p.DiscoverSessions()
	if len(sessions) != 1 {
		t.Fatalf("DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	if sessions[0].Key != "maq:w1" || sessions[0].WindowID != "w1" {
		t.Errorf("session = %+v", sessions[0])
	}

	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT session_id FROM agents WHERE id='agent-pi-1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != fixSessionID {
		t.Errorf("agents.session_id = %q, want %q", got, fixSessionID)
	}
}

func TestPiSource_BindFromEcho(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if err := db.RegisterAgent(pool, "agent-pi-2", "maq", "w2", strPtr("/tmp/proj"), nil, "pi", nil, "executor"); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE agents SET cwd='/tmp/proj' WHERE id='agent-pi-2'`); err != nil {
		t.Fatal(err)
	}

	// Echo id differs from the header id: the echo wins (the agent may
	// have resumed a different session inside this pane).
	echoID := "0199aaaa-1111-7222-b333-444455556666"
	st := state.NewState()
	st.SetWindowRunner("w2", "pi")
	p := NewPiSource(nil, pool, st, state.NewMonitorState())
	p.sessionRoot = t.TempDir()
	fixtureStore(t, p.sessionRoot)

	echo := `{"type":"message","id":"me","parentId":"m3","timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"session file: /home/u/.pi/agent/sessions/` + fixSlugDir + `/2026-09-21T15-00-00-000Z_` + echoID + `.jsonl"}],"timestamp":3}}` + "\n"
	path := filepath.Join(p.sessionRoot, fixSlugDir, fixFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(echo); err != nil {
		t.Fatal(err)
	}
	f.Close()

	sessions := p.DiscoverSessions()
	if len(sessions) != 1 {
		t.Fatalf("DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT session_id FROM agents WHERE id='agent-pi-2'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != echoID {
		t.Errorf("session_id = %q, want echoed %q", got, echoID)
	}
}

// --- Pi relay loop (C1-C3, C6) ---

func piHeaderLine(id string) string {
	return `{"type":"session","version":3,"id":"` + id + `","cwd":"/tmp/proj"}` + "\n"
}

func piMsgLine(role, text string) string {
	return `{"type":"message","id":"m1","parentId":null,"timestamp":"t","message":{"role":"` +
		role + `","content":[{"type":"text","text":"` + text + `"}],"timestamp":1}}` + "\n"
}

// writePiSession writes a session file and returns its full path.
func writePiSession(t *testing.T, dir, id string, lines []string, finalNewline bool) string {
	t.Helper()
	slug := filepath.Join(dir, fixSlugDir)
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "2026-09-21T14-00-00-000Z_" + id + ".jsonl"
	var body strings.Builder
	for _, l := range lines {
		body.WriteString(l)
	}
	if finalNewline && !strings.HasSuffix(body.String(), "\n") {
		body.WriteString("\n")
	}
	path := filepath.Join(slug, name)
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPiSource_SelfTracksOffset: the monitor-shaped poll loop (offset read
// from MonitorState, passed back in) must see each entry exactly once — the
// source persists its own read offset, like the claude/openclaude sources.
func TestPiSource_SelfTracksOffset(t *testing.T) {
	p := newFixtureSource(t)
	p.sessionRoot = t.TempDir()
	id := "01a0c457-aaaa-77af-b6a8-c874297e3cbd"
	path := writePiSession(t, p.sessionRoot, id, []string{
		piHeaderLine(id),
		piMsgLine("user", "first question"),
		piMsgLine("assistant", "first answer"),
	}, true)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	p.appState.SetWindowRunner("w1", "pi")
	p.lastSessionMap["maq:w1"] = state.SessionMapEntry{SessionID: id, CWD: "/tmp/proj"}
	sess := ActiveSession{Key: "maq:w1", WindowID: "w1"}

	poll := func() []ParsedEntry {
		var offset int64
		if tr, ok := p.monitorState.GetTracked("maq:w1"); ok {
			offset = tr.LastByteOffset
		}
		entries, _, err := p.ReadNewEntries(sess, offset)
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}

	first := poll()
	if len(first) != 2 {
		t.Fatalf("first poll = %d entries, want 2", len(first))
	}
	tr, ok := p.monitorState.GetTracked("maq:w1")
	if !ok || tr.LastByteOffset != fi.Size() {
		t.Fatalf("tracked offset = %d (ok=%v), want file size %d", tr.LastByteOffset, ok, fi.Size())
	}
	if second := poll(); len(second) != 0 {
		t.Fatalf("second poll re-emitted %d entries, want 0 (offset loop)", len(second))
	}
}

// TestPiSource_PartialLineNoDupNoLoss: an unterminated final line is held
// back, then delivered exactly once once completed.
func TestPiSource_PartialLineNoDupNoLoss(t *testing.T) {
	p := newFixtureSource(t)
	p.sessionRoot = t.TempDir()
	id := "01a0c457-bbbb-77af-b6a8-c874297e3cbd"
	complete := piHeaderLine(id) + piMsgLine("user", "q1") + piMsgLine("assistant", "a1")
	// A torn append: pi is mid-write on the next line, so the file ends
	// with a truncated JSON prefix (no newline yet).
	partial := `{"type":"message","id":"m1","parentId":null,"timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"slow que`
	rest := `stion"}],"timestamp":1}}` + "\n"
	path := writePiSession(t, p.sessionRoot, id, []string{complete, partial}, false)

	p.appState.SetWindowRunner("w1", "pi")
	p.lastSessionMap["maq:w1"] = state.SessionMapEntry{SessionID: id, CWD: "/tmp/proj"}
	sess := ActiveSession{Key: "maq:w1", WindowID: "w1"}

	poll := func() []ParsedEntry {
		var offset int64
		if tr, ok := p.monitorState.GetTracked("maq:w1"); ok {
			offset = tr.LastByteOffset
		}
		entries, _, err := p.ReadNewEntries(sess, offset)
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}

	first := poll()
	if len(first) != 2 {
		t.Fatalf("first poll = %d entries, want 2 (partial line held back)", len(first))
	}
	if tr, ok := p.monitorState.GetTracked("maq:w1"); !ok || tr.LastByteOffset != int64(len(complete)) {
		t.Fatalf("tracked offset = %d (ok=%v), want %d (start of partial line)",
			tr.LastByteOffset, ok, len(complete))
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(rest); err != nil {
		t.Fatal(err)
	}
	f.Close()

	second := poll()
	if len(second) != 1 || second[0].Role != "user" || second[0].Text != "slow question" {
		t.Fatalf("second poll = %+v, want exactly the completed line", second)
	}
}

// TestPiSource_RebindsToPaneFile: a binding persisted before the live
// transcript appeared is re-evaluated; the rebind seeds the offset at the
// new file's size (no backlog relay).
func TestPiSource_RebindsToPaneFile(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if err := db.RegisterAgent(pool, "agent-pi-3", "maq", "w3", strPtr("/tmp/proj"), nil, "pi", nil, "executor"); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE agents SET cwd='/tmp/proj' WHERE id='agent-pi-3'`); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	staleID := "01a0c457-cccc-77af-b6a8-c874297e3cbd"
	liveID := "01a0c457-dddd-77af-b6a8-c874297e3cbd"

	// Pane v1 started at now-60s; its transcript (bound in phase 1) was
	// last written at now-30s.
	if _, err := pool.Exec(context.Background(),
		`UPDATE agents SET started_at=$1 WHERE id='agent-pi-3'`, now.Add(-60*time.Second)); err != nil {
		t.Fatal(err)
	}

	st := state.NewState()
	st.SetWindowRunner("w3", "pi")
	p := NewPiSource(nil, pool, st, state.NewMonitorState())
	p.sessionRoot = t.TempDir()
	stalePath := writePiSession(t, p.sessionRoot, staleID, []string{
		piHeaderLine(staleID),
		piMsgLine("user", "stale smoke"),
		piMsgLine("assistant", "42"),
	}, true)
	if err := os.Chtimes(stalePath, now.Add(-30*time.Second), now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Phase 1: first bind — only the stale candidate exists.
	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("phase 1: DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	var bound string
	if err := pool.QueryRow(context.Background(),
		`SELECT session_id FROM agents WHERE id='agent-pi-3'`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != staleID {
		t.Fatalf("phase 1: session_id = %q, want stale %q", bound, staleID)
	}

	// Pane v2: recreated at now-10s; the live transcript appears with a
	// fresh mtime (pi creates the file lazily, after pane start).
	if _, err := pool.Exec(context.Background(),
		`UPDATE agents SET started_at=$1 WHERE id='agent-pi-3'`, now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	livePath := writePiSession(t, p.sessionRoot, liveID, []string{
		piHeaderLine(liveID),
		piMsgLine("user", "real conversation"),
		piMsgLine("assistant", "real reply"),
	}, true)
	liveFi, err := os.Stat(livePath)
	if err != nil {
		t.Fatal(err)
	}

	// Phase 2: re-evaluation must rebind to the live transcript and seed
	// the offset at its size.
	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("phase 2: DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT session_id FROM agents WHERE id='agent-pi-3'`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != liveID {
		t.Fatalf("phase 2: session_id = %q, want live %q (binding never re-evaluated)", bound, liveID)
	}
	if p.lastSessionMap["maq:w3"].SessionID != liveID {
		t.Fatalf("phase 2: session map = %q, want %q",
			p.lastSessionMap["maq:w3"].SessionID, liveID)
	}
	tr, ok := p.monitorState.GetTracked("maq:w3")
	if !ok || tr.LastByteOffset != liveFi.Size() {
		t.Fatalf("phase 2: tracked offset = %d (ok=%v), want live size %d",
			tr.LastByteOffset, ok, liveFi.Size())
	}
}

// --- Misc ---

func TestPiSource_DiscoverSessions_NilPool(t *testing.T) {
	p := newFixtureSource(t)
	if sessions := p.DiscoverSessions(); len(sessions) != 0 {
		t.Errorf("want 0 sessions with empty session map, got %d", len(sessions))
	}
}

func TestPiSource_ReadNewEntries_UnknownSession(t *testing.T) {
	p := newFixtureSource(t)
	entries, offset, err := p.ReadNewEntries(ActiveSession{Key: "nope"}, 0)
	if err != nil || entries != nil || offset != 0 {
		t.Errorf("unknown session: %v, %v, %v", entries, offset, err)
	}
}

func TestPiSource_ImplementsInterface(t *testing.T) {
	var _ TranscriptSource = (*PiSource)(nil)
}

func strPtr(s string) *string { return &s }
