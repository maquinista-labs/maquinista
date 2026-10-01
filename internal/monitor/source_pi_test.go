package monitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// writePiSessionAt writes a session file whose NAME and MTIME both say
// `created` — pi names files at creation and the mtime moves on every
// append, so a time-coherent fixture needs both — and returns its path.
func writePiSessionAt(t *testing.T, dir, id string, created time.Time, lines []string, finalNewline bool) string {
	t.Helper()
	slug := filepath.Join(dir, fixSlugDir)
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	name := created.UTC().Format("2006-01-02T15-04-05-000Z") + "_" + id + ".jsonl"
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
	if err := os.Chtimes(path, created, created); err != nil {
		t.Fatal(err)
	}
	return path
}

// piAgentFixture is one registered agent row for piBindingFixture.
type piAgentFixture struct {
	agent     string
	windowID  string
	startedAt time.Time
	sessionID string // optional: pre-seeds a sticky binding
}

// piBindingFixture spins a DB-backed pi source with the given agents
// registered (cwd /tmp/proj, runner pi, window marked pi).
func piBindingFixture(t *testing.T, agents ...piAgentFixture) (*PiSource, *pgxpool.Pool) {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	st := state.NewState()
	for _, a := range agents {
		if err := db.RegisterAgent(pool, a.agent, "maq", a.windowID, strPtr("/tmp/proj"), nil, "pi", nil, "executor"); err != nil {
			t.Fatalf("RegisterAgent: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`UPDATE agents SET cwd='/tmp/proj', started_at=$1, session_id=NULLIF($2,'') WHERE id=$3`,
			a.startedAt, a.sessionID, a.agent); err != nil {
			t.Fatal(err)
		}
		st.SetWindowRunner(a.windowID, "pi")
	}
	p := NewPiSource(nil, pool, st, state.NewMonitorState())
	p.sessionRoot = t.TempDir()
	return p, pool
}

// piSessionIDOf reads back agents.session_id for agent.
func piSessionIDOf(t *testing.T, pool *pgxpool.Pool, agent string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(session_id,'') FROM agents WHERE id=$1`, agent).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPiSource_DiscoverBackfill(t *testing.T) {
	now := time.Now()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	// The spawn path for a hookless runner: RegisterAgent writes the row
	// (started_at defaults to now), session_id stays NULL, and the cwd
	// lands in agents.cwd.
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
	// The pane's transcript, created (name + mtime) seconds after the pane.
	writePiSessionAt(t, p.sessionRoot, fixSessionID, now.Add(-1*time.Second), []string{
		piHeaderLine(fixSessionID),
		piMsgLine("user", "first turn"),
		piMsgLine("assistant", "first reply"),
	}, true)

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
	now := time.Now()
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
	path := writePiSessionAt(t, p.sessionRoot, fixSessionID, now.Add(-1*time.Second), []string{
		piHeaderLine(fixSessionID),
		piMsgLine("user", "first turn"),
		piMsgLine("assistant", "first reply"),
	}, true)

	// The echo names the RESUMED session's file (echoID), which is not
	// the header id of the file on disk: the echo wins.
	echoPath := filepath.Join("/home/u/.pi/agent/sessions", fixSlugDir,
		now.Add(-1*time.Second).UTC().Format("2006-01-02T15-04-05-000Z")+"_"+echoID+".jsonl")
	echo := `{"type":"message","id":"me","parentId":"m3","timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"session file: ` + echoPath + `"}],"timestamp":3}}` + "\n"
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

// TestPiSource_RebindsToPaneFile: a sticky binding whose transcript the
// pane never wrote (mtime predates the pane's restart) and which has an
// unclaimed same-epoch candidate is re-evaluated; the rebind seeds the
// offset at 0 (a fresh-epoch transcript's first turn must relay).
func TestPiSource_RebindsToPaneFile(t *testing.T) {
	now := time.Now()
	p, pool := piBindingFixture(t, piAgentFixture{
		agent:     "agent-pi-3",
		windowID:  "w3",
		startedAt: now.Add(-60 * time.Second),
	})

	staleID := "01a0c457-cccc-77af-b6a8-c874297e3cbd"
	liveID := "01a0c457-dddd-77af-b6a8-c874297e3cbd"

	// Pane v1's transcript: created AND last written at now-30s (its own
	// pane's epoch, so the initial backfill binds it and seeds at 0).
	writePiSessionAt(t, p.sessionRoot, staleID, now.Add(-30*time.Second), []string{
		piHeaderLine(staleID),
		piMsgLine("user", "stale smoke"),
		piMsgLine("assistant", "42"),
	}, true)

	// Phase 1: first bind — only the stale candidate exists.
	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("phase 1: DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	if bound := piSessionIDOf(t, pool, "agent-pi-3"); bound != staleID {
		t.Fatalf("phase 1: session_id = %q, want stale %q", bound, staleID)
	}

	// Pane v2: recreated at now-10s (restart); the live transcript
	// appears created now-5s — after the pane, so unclaimed and fresh.
	if _, err := pool.Exec(context.Background(),
		`UPDATE agents SET started_at=$1 WHERE id='agent-pi-3'`, now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	writePiSessionAt(t, p.sessionRoot, liveID, now.Add(-5*time.Second), []string{
		piHeaderLine(liveID),
		piMsgLine("user", "real conversation"),
		piMsgLine("assistant", "real reply"),
	}, true)

	// Phase 2: the stale binding (mtime now-30s predates the pane's
	// restart at now-10s) is invalidated and re-paired to the live
	// transcript; the fresh-epoch seed is 0, not the file size.
	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("phase 2: DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	if bound := piSessionIDOf(t, pool, "agent-pi-3"); bound != liveID {
		t.Fatalf("phase 2: session_id = %q, want live %q (binding never re-evaluated)", bound, liveID)
	}
	if p.lastSessionMap["maq:w3"].SessionID != liveID {
		t.Fatalf("phase 2: session map = %q, want %q",
			p.lastSessionMap["maq:w3"].SessionID, liveID)
	}
	if tr, ok := p.monitorState.GetTracked("maq:w3"); !ok || tr.LastByteOffset != 0 {
		t.Fatalf("phase 2: tracked offset = %d (ok=%v), want 0 (fresh-epoch seed)",
			tr.LastByteOffset, ok)
	}
}

// --- Stale-binding repair + epoch seed (pi-binding-heal) ---

// TestPiSource_FirstTurnNotSkipped (C1): a first turn that completed
// between pane start and discovery must relay. The same-epoch transcript
// seeds at 0, so the monitor-shaped poll loop sees the assistant entry on
// the first poll and nothing on an immediate repeat.
func TestPiSource_FirstTurnNotSkipped(t *testing.T) {
	now := time.Now()
	c1ID := "01a0d0c1-0001-77af-b6a8-c874297e3cbd"
	p, _ := piBindingFixture(t, piAgentFixture{
		agent: "agent-pi-c1", windowID: "w1", startedAt: now.Add(-60 * time.Second),
	})
	writePiSessionAt(t, p.sessionRoot, c1ID, now.Add(-55*time.Second), []string{
		piHeaderLine(c1ID),
		piMsgLine("user", "olá"),
		piMsgLine("assistant", "primeira resposta"),
	}, true)

	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	// The seed claim: a fresh binding on a same-epoch transcript starts
	// at 0, not at the file size (which would skip the completed turn).
	if tr, ok := p.monitorState.GetTracked("maq:w1"); !ok || tr.LastByteOffset != 0 {
		t.Fatalf("tracked offset = %d (ok=%v), want 0 (same-epoch seed)", tr.LastByteOffset, ok)
	}

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
	var sawAssistant bool
	for _, e := range first {
		if e.Role == "assistant" && e.Text == "primeira resposta" {
			sawAssistant = true
		}
	}
	if !sawAssistant {
		t.Fatalf("first poll = %+v, want the completed assistant turn (user never reaches the outbox sink)", first)
	}
	if second := poll(); len(second) != 0 {
		t.Fatalf("second poll re-emitted %d entries, want 0", len(second))
	}
}

// TestPiSource_PreEpochSeedsAtSize (C2): a sticky binding on a transcript
// created long before the pane (resumed session) seeds at the file size —
// pre-binding content must not relay as backlog.
func TestPiSource_PreEpochSeedsAtSize(t *testing.T) {
	now := time.Now()
	oldID := "01a0d0c2-0002-77af-b6a8-c874297e3cbd"
	p, _ := piBindingFixture(t, piAgentFixture{
		agent: "agent-pi-c2", windowID: "w2",
		startedAt: now.Add(-60 * time.Second),
		sessionID: oldID, // resumed session, bound before this daemon epoch
	})
	path := writePiSessionAt(t, p.sessionRoot, oldID, now.Add(-10*time.Minute), []string{
		piHeaderLine(oldID),
		piMsgLine("user", "old turn"),
		piMsgLine("assistant", "old answer"),
	}, true)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	tr, ok := p.monitorState.GetTracked("maq:w2")
	if !ok || tr.LastByteOffset != fi.Size() {
		t.Fatalf("tracked offset = %d (ok=%v), want file size %d (pre-epoch seed)",
			tr.LastByteOffset, ok, fi.Size())
	}
	entries, _, err := p.ReadNewEntries(ActiveSession{Key: "maq:w2", WindowID: "w2"}, tr.LastByteOffset)
	if err != nil || len(entries) != 0 {
		t.Fatalf("poll at size = %d entries (err %v), want 0 (no backlog relay)", len(entries), err)
	}
}

// TestPiSource_StaleBindingsRepaired (C3): after a daemon restart, panes
// that booted fresh still hold pre-restart bindings whose transcripts they
// never wrote. With unclaimed same-epoch transcripts present, the repair
// re-pairs newest pane ↔ newest file.
func TestPiSource_StaleBindingsRepaired(t *testing.T) {
	now := time.Now()
	oldA := "01a0d0c3-000a-77af-b6a8-c874297e3cbd"
	oldB := "01a0d0c3-000b-77af-b6a8-c874297e3cbd"
	newA := "01a0d0c3-000c-77af-b6a8-c874297e3cbd"
	newB := "01a0d0c3-000d-77af-b6a8-c874297e3cbd"
	p, pool := piBindingFixture(t,
		piAgentFixture{agent: "agent-pi-c3a", windowID: "w3", startedAt: now.Add(-50 * time.Second), sessionID: oldA},
		piAgentFixture{agent: "agent-pi-c3b", windowID: "w4", startedAt: now.Add(-40 * time.Second), sessionID: oldB},
	)
	// Pre-restart transcripts: last written long before the panes.
	writePiSessionAt(t, p.sessionRoot, oldA, now.Add(-10*time.Minute), []string{piHeaderLine(oldA)}, true)
	writePiSessionAt(t, p.sessionRoot, oldB, now.Add(-9*time.Minute), []string{piHeaderLine(oldB)}, true)
	// Fresh transcripts created after the panes booted.
	writePiSessionAt(t, p.sessionRoot, newA, now.Add(-45*time.Second), []string{piHeaderLine(newA)}, true)
	writePiSessionAt(t, p.sessionRoot, newB, now.Add(-35*time.Second), []string{piHeaderLine(newB)}, true)

	if sessions := p.DiscoverSessions(); len(sessions) != 2 {
		t.Fatalf("DiscoverSessions = %d sessions, want 2", len(sessions))
	}
	if got := piSessionIDOf(t, pool, "agent-pi-c3a"); got != newA {
		t.Fatalf("w3 session_id = %q, want %q (newest-of-two ↔ older pane)", got, newA)
	}
	if got := piSessionIDOf(t, pool, "agent-pi-c3b"); got != newB {
		t.Fatalf("w4 session_id = %q, want %q (newest pane ↔ newest file)", got, newB)
	}
}

// TestPiSource_LiveBindingNotStolen (C4): a binding whose transcript the
// pane HAS written (mtime after pane start) is never re-paired, even when
// a newer unclaimed transcript exists.
func TestPiSource_LiveBindingNotStolen(t *testing.T) {
	now := time.Now()
	liveID := "01a0d0c4-0004-77af-b6a8-c874297e3cbd"
	otherID := "01a0d0c4-0005-77af-b6a8-c874297e3cbd"
	p, pool := piBindingFixture(t, piAgentFixture{
		agent: "agent-pi-c4", windowID: "w5", startedAt: now.Add(-60 * time.Second), sessionID: liveID,
	})
	writePiSessionAt(t, p.sessionRoot, liveID, now.Add(-5*time.Second), []string{piHeaderLine(liveID)}, true)
	writePiSessionAt(t, p.sessionRoot, otherID, now.Add(-2*time.Second), []string{piHeaderLine(otherID)}, true)

	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	if got := piSessionIDOf(t, pool, "agent-pi-c4"); got != liveID {
		t.Fatalf("session_id = %q, want live %q (live binding must not be stolen)", got, liveID)
	}
}

// TestPiSource_StaleBindingKeptWithoutCandidate (C5): an idle resumed pane
// holds a pre-epoch transcript with no same-epoch candidate — the binding
// stands (never unbind without a repair target).
func TestPiSource_StaleBindingKeptWithoutCandidate(t *testing.T) {
	now := time.Now()
	oldID := "01a0d0c5-0006-77af-b6a8-c874297e3cbd"
	p, pool := piBindingFixture(t, piAgentFixture{
		agent: "agent-pi-c5", windowID: "w6", startedAt: now.Add(-60 * time.Second), sessionID: oldID,
	})
	writePiSessionAt(t, p.sessionRoot, oldID, now.Add(-10*time.Minute), []string{piHeaderLine(oldID)}, true)

	if sessions := p.DiscoverSessions(); len(sessions) != 1 {
		t.Fatalf("DiscoverSessions = %d sessions, want 1", len(sessions))
	}
	if got := piSessionIDOf(t, pool, "agent-pi-c5"); got != oldID {
		t.Fatalf("session_id = %q, want %q (no candidate: binding must stand)", got, oldID)
	}
}

// TestPiSource_StaleRepairLoserKeepsOwn (C6): when two stale windows
// compete for one repair candidate, the newer pane wins it and the loser
// falls back to its own (stale) transcript instead of going unbound.
func TestPiSource_StaleRepairLoserKeepsOwn(t *testing.T) {
	now := time.Now()
	oldA := "01a0d0c6-000a-77af-b6a8-c874297e3cbd"
	oldB := "01a0d0c6-000b-77af-b6a8-c874297e3cbd"
	newA := "01a0d0c6-000c-77af-b6a8-c874297e3cbd"
	p, pool := piBindingFixture(t,
		piAgentFixture{agent: "agent-pi-c6a", windowID: "w7", startedAt: now.Add(-50 * time.Second), sessionID: oldA},
		piAgentFixture{agent: "agent-pi-c6b", windowID: "w8", startedAt: now.Add(-40 * time.Second), sessionID: oldB},
	)
	writePiSessionAt(t, p.sessionRoot, oldA, now.Add(-10*time.Minute), []string{piHeaderLine(oldA)}, true)
	writePiSessionAt(t, p.sessionRoot, oldB, now.Add(-9*time.Minute), []string{piHeaderLine(oldB)}, true)
	writePiSessionAt(t, p.sessionRoot, newA, now.Add(-30*time.Second), []string{piHeaderLine(newA)}, true)

	if sessions := p.DiscoverSessions(); len(sessions) != 2 {
		t.Fatalf("DiscoverSessions = %d sessions, want 2", len(sessions))
	}
	if got := piSessionIDOf(t, pool, "agent-pi-c6b"); got != newA {
		t.Fatalf("w8 session_id = %q, want %q (newer pane wins the candidate)", got, newA)
	}
	if got := piSessionIDOf(t, pool, "agent-pi-c6a"); got != oldA {
		t.Fatalf("w7 session_id = %q, want %q (loser keeps its own transcript)", got, oldA)
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
