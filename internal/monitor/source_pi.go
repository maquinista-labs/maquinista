package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// PiSource implements TranscriptSource for the pi coding agent
// (badlogic/pi-mono, session-store layout verified against v0.73.1).
//
// pi writes one JSONL file per session under
//
//	$PI_CODING_AGENT_DIR/sessions/<cwd-slug>/<timestamp>_<uuid>.jsonl
//
// (default root ~/.pi/agent). The directory slug wraps the cwd in "--...--"
// and replaces the leading "/" plus every "/", "\" and ":" with "-" — see
// SlugifyCWD. The first line of each file is a v3 session header
// {"type":"session","version":3,"id":<uuid>,"cwd":...}; the rest are typed
// entries of which only type:"message" carries conversation payload.
//
// Session discovery is hookless: pi has no SessionStart hook, so the bot
// writes a preliminary agents row and this source backfills agents
// .session_id by locating the session file for the row's cwd — preferring
// a real session id echoed into the transcript via $PI_SESSION_FILE when
// one is present.
type PiSource struct {
	config         *config.Config
	pool           *pgxpool.Pool
	appState       *state.State
	monitorState   *state.MonitorState
	sessionRoot    string
	lastSessionMap map[string]state.SessionMapEntry
}

// NewPiSource creates a new PiSource.
func NewPiSource(cfg *config.Config, pool *pgxpool.Pool, st *state.State, ms *state.MonitorState) *PiSource {
	return &PiSource{
		config:         cfg,
		pool:           pool,
		appState:       st,
		monitorState:   ms,
		sessionRoot:    piSessionsRoot(),
		lastSessionMap: make(map[string]state.SessionMapEntry),
	}
}

func (p *PiSource) Name() string { return "pi" }

// piSessionsRoot resolves the session-store root: PI_CODING_AGENT_DIR
// rebases the whole store (/<dir>/sessions); default ~/.pi/agent/sessions.
func piSessionsRoot() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// SlugifyCWD reproduces pi 0.73.1's session directory slug
// (session-manager.js): "--" + cwd minus its leading "/" with every "/",
// "\" and ":" replaced by "-", then "--". Examples from the live store
// (observed 21/09): /tmp/bench-pi → --tmp-bench-pi--,
// /home/otavio/code/maquinista → --home-otavio-code-maquinista--.
func SlugifyCWD(cwd string) string {
	s := strings.TrimPrefix(cwd, "/")
	s = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(s)
	return "--" + s + "--"
}

// piSessionHeader is the first line of a pi session JSONL file.
type piSessionHeader struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	ID      string `json:"id"`
	CWD     string `json:"cwd"`
}

// piEntry is one JSONL line of a pi session file. Only Type "message"
// carries conversation payload; everything else (session, model_change,
// thinking_level_change, branch_summary, compaction, ...) is metadata to
// skip.
type piEntry struct {
	Type    string     `json:"type"`
	Message *piMessage `json:"message"`
}

type piMessage struct {
	Role    string      `json:"role"`
	Content []piContent `json:"content"`
}

type piContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// sessionIDFromEcho extracts a session uuid from text that echoes a
// $PI_SESSION_FILE-style path like
// /home/u/.pi/agent/sessions/<slug>/<ts>_<uuid>.jsonl. Returns "" when no
// such path appears in the text.
func sessionIDFromEcho(text string) string {
	// Split on whitespace AND on JSON punctuation (`"` and `}`): echoes
	// arrive inside JSONL lines, so a path at the end of a string value
	// is glued to the closing quote/braces (…uuid.jsonl"}]}).
	isSep := func(r rune) bool {
		return r == ' ' || r == '\t' || r == '"' || r == '}'
	}
	for _, token := range strings.FieldsFunc(text, isSep) {
		token = strings.Trim(token, `'`)
		if !strings.Contains(token, "/sessions/") || !strings.HasSuffix(token, ".jsonl") {
			continue
		}
		base := filepath.Base(token)
		idx := strings.LastIndex(base, "_")
		if idx < 0 {
			continue
		}
		if id := strings.TrimSuffix(base[idx+1:], ".jsonl"); id != "" {
			return id
		}
	}
	return ""
}

// piEchoSessionID scans a session file for an echoed $PI_SESSION_FILE
// path and returns the session id parsed from it — the transcript's own
// claim about which session is live. "" when nothing is echoed.
func piEchoSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if id := sessionIDFromEcho(scanner.Text()); id != "" {
			return id
		}
	}
	return ""
}

// piHeaderID reads a session file's first line and returns the session
// uuid from the v3 header ("" when unparseable).
func piHeaderID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return ""
	}
	var h piSessionHeader
	if err := json.Unmarshal(scanner.Bytes(), &h); err != nil || h.Type != "session" {
		return ""
	}
	return h.ID
}

// piBindingSlack relaxes the pane-age filter for bindings: a transcript's
// embedded creation time should always postdate agents.started_at (the
// agents row is written before the pane spawns), but pi stamps filenames
// in UTC while the DB clock may drift slightly.
const piBindingSlack = 90 * time.Second

// piFile is one parseable session transcript found in a cwd's session dir.
type piFile struct {
	path    string
	id      string
	created time.Time // from the <ts>_ filename prefix; zero when unparseable
}

// piWindow is one live pi window (from the runner session map) competing
// for a transcript binding.
type piWindow struct {
	key       string
	entry     state.SessionMapEntry
	windowID  string
	notBefore time.Time // pane creation (agents.started_at); zero = unknown
	sticky    bool      // existing binding validated this cycle
}

// piFileCreatedTime parses pi's filename prefix — UTC
// "2006-01-02T15-04-05.000Z" with ':' rendered as '-' — e.g.
// 2026-09-30T22-34-44-810Z_01a0f474-….jsonl. Zero when it doesn't match.
// This is the file's CREATION time; ModTime moves on every append and is
// useless for deciding which pane a file belongs to.
func piFileCreatedTime(name string) time.Time {
	idx := strings.Index(name, "_")
	if idx <= 0 {
		return time.Time{}
	}
	stamp := name[:idx]
	// pi renders the millis separator as '-' (…T22-34-44-810Z); Go's
	// fractional-second layout uses '.'. Normalize the dash form to the
	// dot form — only when no fractional dot already follows the 'T' —
	// so both spellings parse.
	if ti := strings.Index(stamp, "T"); ti >= 0 && strings.LastIndex(stamp, ".") < ti {
		if i := strings.LastIndex(stamp, "-"); i > ti {
			stamp = stamp[:i] + "." + stamp[i+1:]
		}
	}
	t, err := time.Parse("2006-01-02T15-04-05.000Z", stamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

// readPiInventory lists a cwd's parseable session files sorted ascending by
// name — pi's <timestamp>_ prefix makes lexicographic order chronological
// by creation.
func readPiInventory(dir string) []piFile {
	names, err := listSessionFiles(dir)
	if err != nil {
		return nil
	}
	var inv []piFile
	for _, n := range names {
		id := piHeaderID(filepath.Join(dir, n))
		if id == "" {
			continue
		}
		inv = append(inv, piFile{
			path:    filepath.Join(dir, n),
			id:      id,
			created: piFileCreatedTime(n),
		})
	}
	return inv
}

// resolvePiBindings computes which transcript file each live pi window is
// bound to; windows with no file yet are absent from the result.
//
// Binding rules, per cwd group of live windows:
//  1. Sticky: an existing binding survives while its transcript exists and
//     was created at/after the pane's start (agents.started_at, carried as
//     WindowCreatedAt). A binding left over from a previous pane epoch
//     points at an older file and is invalidated — this self-corrects the
//     lazily-created transcript gap (pi writes its first byte ~26s after
//     spawn) without re-running a newest-file rule.
//  2. Exclusive claims: two live windows sharing a cwd must never hold the
//     same transcript. When they do (concurrent panes defeat any
//     newest-file rule), the whole group is re-paired deterministically:
//     newest pane ↔ newest file.
//  3. An unbound window takes the newest unclaimed file created at/after
//     the pane's start. Until its own transcript appears it stays unbound
//     instead of borrowing a sibling's file.
func resolvePiBindings(windows []*piWindow, inventory func(cwd string) []piFile) map[string]piFile {
	// Pass 1: validate sticky bindings.
	for _, w := range windows {
		if w.entry.SessionID == "" {
			continue
		}
		for _, f := range inventory(w.entry.CWD) {
			if f.id != w.entry.SessionID {
				continue
			}
			if !w.notBefore.IsZero() && f.created.Before(w.notBefore.Add(-piBindingSlack)) {
				break // transcript predates the pane: previous epoch
			}
			w.sticky = true
			break
		}
	}

	// Pass 2: a transcript claimed by two live windows is corrupt state;
	// re-pair the whole cwd group deterministically.
	groups := map[string][]*piWindow{}
	for _, w := range windows {
		groups[w.entry.CWD] = append(groups[w.entry.CWD], w)
	}
	for _, ws := range groups {
		counts := map[string]int{}
		for _, w := range ws {
			if w.sticky {
				counts[w.entry.SessionID]++
			}
		}
		dup := false
		for _, c := range counts {
			if c > 1 {
				dup = true
			}
		}
		if !dup {
			continue
		}
		for _, w := range ws {
			w.sticky = false
		}
	}

	// Pass 3: bind. Newest pane claims first, newest unclaimed file first,
	// so concurrent panes on one cwd each end up with their own file.
	bound := map[string]piFile{}
	claimed := map[string]bool{}
	var toBind []*piWindow
	for _, w := range windows {
		if !w.sticky {
			toBind = append(toBind, w)
			continue
		}
		for _, f := range inventory(w.entry.CWD) {
			if f.id == w.entry.SessionID {
				bound[w.key] = f
				claimed[f.id] = true
				break
			}
		}
	}
	sort.Slice(toBind, func(i, j int) bool { return toBind[i].notBefore.After(toBind[j].notBefore) })
	for _, w := range toBind {
		inv := inventory(w.entry.CWD)
		for i := len(inv) - 1; i >= 0; i-- { // newest first
			f := inv[i]
			if claimed[f.id] {
				continue
			}
			if !w.notBefore.IsZero() && f.created.Before(w.notBefore.Add(-piBindingSlack)) {
				continue
			}
			bound[w.key] = f
			claimed[f.id] = true
			break
		}
	}
	return bound
}

// listSessionFiles returns the .jsonl file names in dir sorted ascending
// (pi's <timestamp>_ prefix makes lexicographic order chronological).
func listSessionFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// discoverSessionFile finds the newest parseable session file for cwd and
// returns its path plus the session id from its header. When notBefore is
// non-zero, candidates must have an mtime at or after it: pi TUI creates
// its transcript lazily (observed ~26s after pane start), so a file older
// than the pane is a previous pane's transcript, never the live one.
func (p *PiSource) discoverSessionFile(cwd string, notBefore time.Time) (string, string, error) {
	dir := filepath.Join(p.sessionRoot, SlugifyCWD(cwd))
	names, err := listSessionFiles(dir)
	if err != nil {
		return "", "", err
	}
	for i := len(names) - 1; i >= 0; i-- {
		path := filepath.Join(dir, names[i])
		if !notBefore.IsZero() {
			info, serr := os.Stat(path)
			if serr != nil || info.ModTime().Before(notBefore) {
				continue
			}
		}
		if id := piHeaderID(path); id != "" {
			return path, id, nil
		}
	}
	return "", "", fmt.Errorf("no parseable session header under %s", dir)
}

func (p *PiSource) DiscoverSessions() []ActiveSession {
	var sm map[string]state.SessionMapEntry
	if p.pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		loaded, err := loadRunnerSessionMap(ctx, p.pool, "pi")
		if err != nil {
			log.Printf("pi source: session discovery: %v", err)
			return nil
		}
		sm = loaded
	} else {
		sm = map[string]state.SessionMapEntry{}
	}

	// Clean up stale sessions.
	for key := range p.lastSessionMap {
		if _, ok := sm[key]; !ok {
			p.monitorState.RemoveSession(key)
		}
	}

	// Collect live pi windows from the runner session map, then resolve
	// transcript bindings for the whole set at once: sticky per pane,
	// exclusively claimed, deterministically re-paired on conflict (see
	// resolvePiBindings). An echoed $PI_SESSION_FILE still wins over any
	// derived id.
	var windows []*piWindow
	for key, entry := range sm {
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
			continue
		}
		if p.appState.GetWindowRunner(windowID) != "pi" {
			continue
		}
		w := &piWindow{key: key, entry: entry, windowID: windowID}
		if entry.WindowCreatedAt > 0 {
			w.notBefore = time.UnixMilli(entry.WindowCreatedAt)
		}
		windows = append(windows, w)
	}

	inventories := map[string][]piFile{}
	inventory := func(cwd string) []piFile {
		if inv, ok := inventories[cwd]; ok {
			return inv
		}
		inv := readPiInventory(filepath.Join(p.sessionRoot, SlugifyCWD(cwd)))
		inventories[cwd] = inv
		return inv
	}

	bound := resolvePiBindings(windows, inventory)

	for _, w := range windows {
		f, ok := bound[w.key]
		if !ok {
			// pi creates the transcript lazily (~26s after spawn); the
			// window binds on a later cycle, when its own file exists.
			// It must never borrow a sibling pane's file in the meantime.
			continue
		}
		entry := sm[w.key]
		sessionID := f.id
		if echo := piEchoSessionID(f.path); echo != "" && echo != sessionID {
			// The transcript echoed its own $PI_SESSION_FILE — that is
			// the authoritative binding, not the file we happened to
			// find by timestamp (the agent may have resumed another
			// session inside this pane).
			log.Printf("Pi: binding via echoed $PI_SESSION_FILE %s (header said %s)", echo, sessionID)
			sessionID = echo
		}
		if entry.SessionID == sessionID {
			continue
		}
		prev := entry.SessionID

		entry.SessionID = sessionID
		sm[w.key] = entry

		// Persist so the bot and follow-up polls see it without
		// rediscovering.
		if p.pool != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if _, uerr := p.pool.Exec(ctx, `
				UPDATE agents SET session_id=$1, last_seen=NOW()
				WHERE tmux_session=$2 AND tmux_window=$3
			`, sessionID, strings.SplitN(w.key, ":", 2)[0], strings.SplitN(w.key, ":", 2)[1]); uerr != nil {
				log.Printf("Pi: persist session_id: %v", uerr)
			}
			cancel()
		}

		// Seed the tracked offset at the bound file's current size:
		// transcript content predating the binding (spawn prompt,
		// earlier pane) must not relay as backlog on the next poll.
		if fi, serr := os.Stat(f.path); serr == nil {
			p.monitorState.UpdateOffset(w.key, sessionID, f.path, fi.Size())
		}
		if prev != "" {
			log.Printf("Pi: rebound %s: %s -> %s", w.key, prev, sessionID)
		}
		log.Printf("Pi session discovered: %s -> %s", entry.CWD, sessionID)
	}

	var sessions []ActiveSession
	for key, entry := range sm {
		if entry.SessionID == "" {
			continue
		}
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
			continue
		}
		if p.appState.GetWindowRunner(windowID) != "pi" {
			continue
		}
		sessions = append(sessions, ActiveSession{
			Key:      key,
			WindowID: windowID,
		})
	}

	p.lastSessionMap = sm
	return sessions
}

// findSessionFile locates the JSONL file for a session id: pi names files
// <timestamp>_<uuid>.jsonl, so suffix match on "_" + id + ".jsonl" and
// take the newest.
func (p *PiSource) findSessionFile(entry state.SessionMapEntry) string {
	dir := filepath.Join(p.sessionRoot, SlugifyCWD(entry.CWD))
	names, err := listSessionFiles(dir)
	if err != nil {
		return ""
	}
	suffix := "_" + entry.SessionID + ".jsonl"
	newest := ""
	for _, name := range names {
		if strings.HasSuffix(name, suffix) && name > newest {
			newest = name
		}
	}
	if newest == "" {
		return ""
	}
	return filepath.Join(dir, newest)
}

func (p *PiSource) ReadNewEntries(session ActiveSession, lastOffset int64) ([]ParsedEntry, int64, error) {
	entry, ok := p.lastSessionMap[session.Key]
	if !ok || entry.SessionID == "" {
		return nil, lastOffset, nil
	}

	path := p.findSessionFile(entry)
	if path == "" {
		return nil, lastOffset, nil
	}

	// Detect truncation (session reset) the same way ClaudeSource does.
	info, err := os.Stat(path)
	if err != nil {
		return nil, lastOffset, nil
	}
	offset := lastOffset
	if offset > info.Size() {
		offset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, lastOffset, nil
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return nil, lastOffset, nil
		}
	}

	var entries []ParsedEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer
	var bytesRead int64
	var lastLineLen int

	for scanner.Scan() {
		line := scanner.Bytes()
		lastLineLen = len(line)
		bytesRead += int64(len(line)) + 1 // +1 for newline

		if pe := parsePiLine(line); pe != nil {
			entries = append(entries, *pe)
		}
		// Unparseable lines (metadata, unknown types, malformed JSON) are
		// skipped silently — pi adds new entry types between releases and
		// the monitor must not stall on them.
	}
	if err := scanner.Err(); err != nil {
		log.Printf("pi JSONL read error for %s at offset %d: %v (not advancing offset)", path, offset, err)
		return nil, lastOffset, nil
	}

	newOffset := offset + bytesRead
	// An unterminated final line means pi is mid-append: hold the offset
	// at the line start so the completed line is read (exactly once) on a
	// later pass, instead of resuming mid-line and silently losing it.
	if bytesRead > 0 && !endsWithNewline(f, newOffset) {
		if lastLineLen > 0 {
			newOffset -= int64(lastLineLen) + 1
		}
	}
	if newOffset == lastOffset {
		return nil, lastOffset, nil
	}
	// Self-track like the claude/openclaude sources: the monitor discards
	// the returned offset (monitor.go keeps `_ = newOffset`), so without
	// this write every poll re-read the transcript from the same offset
	// and re-emitted it.
	p.monitorState.UpdateOffset(session.Key, entry.SessionID, path, newOffset)
	return entries, newOffset, nil
}

// endsWithNewline reports whether the byte just before pos is a newline.
// An out-of-range pos means the count includes a phantom newline (the last
// scanned line had none), so there is definitively no newline there.
func endsWithNewline(f *os.File, pos int64) bool {
	if pos <= 0 {
		return true
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], pos-1); err != nil {
		return false // beyond EOF or unreadable: hold the offset back
	}
	return b[0] == '\n'
}

// parsePiLine converts one pi JSONL line into a ParsedEntry, or nil when
// the line is not a conversation message (metadata types, unknown roles,
// malformed JSON). Roles user/assistant/toolResult are preserved verbatim.
func parsePiLine(line []byte) *ParsedEntry {
	var e piEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return nil
	}
	if e.Type != "message" || e.Message == nil {
		return nil
	}
	m := e.Message
	switch m.Role {
	case "user", "assistant":
		text := piTextContent(m.Content)
		if text == "" {
			return nil
		}
		return &ParsedEntry{Role: m.Role, ContentType: "text", Text: text}
	case "toolResult":
		return &ParsedEntry{Role: "toolResult", ContentType: "tool_result", Text: piTextContent(m.Content)}
	default:
		return nil
	}
}

// piTextContent returns the first non-empty text block, or "".
func piTextContent(content []piContent) string {
	for _, c := range content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			return strings.TrimSpace(c.Text)
		}
	}
	return ""
}

func (p *PiSource) ExtractStatusLine(paneText string) (string, bool) {
	return ExtractStatusLineFor(paneText, PiProfile())
}

func (p *PiSource) IsInteractiveUI(paneText string) bool {
	return IsInteractiveUIFor(paneText, PiProfile())
}
