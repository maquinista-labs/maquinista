// Turn-end detection for live pipeline agents — ADR-0008 Fase 1, shadow
// mode (MAQ-43).
//
// The r8 incident (08/10, MAQ-37): an implementor finished real work,
// wrote its summary to the outbox, and ended its turn without ever running
// `maquinista-done`. Nothing distinguished that clean ending from a freeze
// — both are silence — so the watchdog retired it 30m later and the spent
// respawn cap parked finished work needs-human. ADR-0008 makes turn end a
// first-class signal; this file is its shadow phase: DETECT + LOG + COUNT
// only. No outbox rows, no agent-row writes, no scheduler/dispatch
// behavior — the freeze predicate reads agent_outbox and
// agents.last_transcript_at, and anything this fase wrote there would move
// watchdog decisions (the acceptance forbids exactly that: zero pipeline
// state changes attributable to F1). The journal is the measurement
// surface: one structured line per detection, `total=` carrying the
// boot-cumulative incidence; after 24h the counts decide F2's order.
//
// Turn end = the transcript tail shows an assistant message closing the
// turn with no pending tool call. pi first (every pipeline soul pins
// default_runner: pi): the closing shape is a `message` entry with role
// assistant whose content carries no `toolCall` block — r8's real close,
// verified on barceloneta: `stopReason:"stop"` with [thinking, text]
// blocks at EOF, vs 22 mid-turn messages with `stopReason:"toolUse"`. The
// structural check (no toolCall block) is primary; stopReason only
// corroborates it in fixtures, never gates the logic — pi may omit it.
//
// Detection rides the poll loop: whenever a bound transcript grows, scan
// its tail. The scan walks backwards over metadata lines (model_change,
// compaction, ...) to the last complete message entry; an unterminated
// final line (pi mid-append) is invisible to it, so a close is only ever
// seen complete. Firings dedup per window on the closing entry's id — the
// tail stays a turn end until the next turn begins, and polls happen every
// interval. A freshness gate (close within a few poll intervals) keeps
// bind-time backlog replays (same-epoch seed reads from 0) from
// inflating the counts: only live closings are counted.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"time"
)

// TurnEndInspector is the optional per-runner capability for detecting
// turn end in a bound transcript (ADR-0008). pi implements it first; the
// claude/opencode parsers follow the same tail pattern. The monitor casts
// for it after every observed transcript growth.
type TurnEndInspector interface {
	// TailTurnEnd inspects the session's transcript tail and reports
	// whether it ends in a turn close. msgID identifies the closing
	// entry (dedup key), at is its wall-clock timestamp (freshness
	// gate); both are zero/"" when the tail is not a turn end.
	TailTurnEnd(session ActiveSession) (msgID string, at time.Time, ended bool)
}

// turnEndFreshFallback bounds the freshness gate when the monitor was
// built without a poll interval (struct-literal construction in tests).
const turnEndFreshFallback = 6 * time.Second

// pipelineLiveAgentSQL resolves whether a detected turn end belongs to a
// live pipeline agent (ADR-0008 scope): an agent row bound to a task with
// a live status — the same live set the freeze arms use (taskscheduler
// liveAgentStatusSQL). User topic agents (role='user', task_id NULL) and
// dead/retired rows are not pipeline signals. $1 is the agents.id
// resolved by resolveAgentFromWindow (live-preferring, MAQ-38).
const pipelineLiveAgentSQL = `
SELECT role, task_id FROM agents
WHERE id = $1
  AND task_id IS NOT NULL
  AND status IN ('running','idle','working','spawning')`

// piTailTurnEndResult is what the tail scan reports.
type piTailTurnEndResult struct {
	MsgID string    // closing entry id ("" when no turn end)
	At    time.Time // closing entry timestamp (zero when absent/unparseable)
	Ended bool
}

// piTailTurnEnd scans the tail of a pi session JSONL and reports whether
// it ends in a turn close: the last complete message entry is an
// assistant message with no pending tool call (no toolCall content
// block). Backwards walk so metadata entries appended after the close
// (model_change, compaction, ...) do not mask it. An unterminated final
// line — pi mid-append — is not a complete line and is skipped: the close
// is seen complete on a later poll, never half-written.
func piTailTurnEnd(path string) piTailTurnEndResult {
	f, err := os.Open(path)
	if err != nil {
		return piTailTurnEndResult{}
	}
	defer f.Close()

	const tailSize = 64 << 10 // 64KB is far more than any message entry needs
	fi, err := f.Stat()
	if err != nil {
		return piTailTurnEndResult{}
	}
	size := fi.Size()
	if size > tailSize {
		size = tailSize
	}
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, fi.Size()-int64(len(buf)))
	if err != nil && n == 0 {
		return piTailTurnEndResult{}
	}
	buf = buf[:n]

	// Drop the trailing partial line (no newline after it = pi is
	// mid-append), then walk backwards line by line.
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[:i]
	} else {
		return piTailTurnEndResult{} // no complete line in the tail
	}
	for len(buf) > 0 {
		start := bytes.LastIndexByte(buf, '\n') + 1
		line := buf[start:]
		buf = buf[:start]
		if start > 0 {
			buf = buf[:start-1] // drop the newline that ended this line
		}
		if len(line) == 0 {
			continue
		}
		var e piEntry
		if err := json.Unmarshal(line, &e); err != nil || e.Type != "message" || e.Message == nil {
			continue // metadata / garbage — keep walking back
		}
		return piTurnEndFromEntry(e)
	}
	return piTailTurnEndResult{}
}

// piTurnEndFromEntry classifies one message entry: an assistant message
// with no toolCall block closes the turn; anything else (user prompt,
// toolResult awaiting the next assistant hop, assistant toolCall) leaves
// the turn open.
func piTurnEndFromEntry(e piEntry) piTailTurnEndResult {
	m := e.Message
	if m.Role != "assistant" {
		return piTailTurnEndResult{}
	}
	for _, c := range m.Content {
		if c.Type == "toolCall" {
			return piTailTurnEndResult{} // tool call pending — turn continues
		}
	}
	id := e.ID
	if id == "" {
		id = e.Timestamp // degenerate ids still need a stable dedup key
	}
	at, _ := time.Parse(time.RFC3339, e.Timestamp)
	return piTailTurnEndResult{MsgID: id, At: at, Ended: true}
}

// TailTurnEnd implements TurnEndInspector for the pi source: resolve the
// bound transcript path for the session, scan its tail.
func (p *PiSource) TailTurnEnd(session ActiveSession) (string, time.Time, bool) {
	entry, ok := p.lastSessionMap[session.Key]
	if !ok || entry.SessionID == "" {
		return "", time.Time{}, false
	}
	path := p.findSessionFile(entry)
	if path == "" {
		return "", time.Time{}, false
	}
	r := piTailTurnEnd(path)
	return r.MsgID, r.At, r.Ended
}

// detectTurnEnd is the shadow-mode emission point, called once per session
// per poll when the transcript grew. Pipeline-filtered (live agents with a
// task), freshness-gated (backlog replays are not live closings), deduped
// per window on the closing message id, then counted and logged. No sink,
// no DB write — the journal line IS the event (ADR-0008 F1).
func (m *Monitor) detectTurnEnd(src TranscriptSource, sess ActiveSession) {
	ins, ok := src.(TurnEndInspector)
	if !ok {
		return
	}
	msgID, at, ended := ins.TailTurnEnd(sess)
	if !ended {
		return
	}
	fresh := 3 * m.pollInterval
	if fresh <= 0 {
		fresh = turnEndFreshFallback
	}
	if at.IsZero() || time.Since(at) > fresh {
		return // stale tail (backlog replay, resumed transcript) — not a live close
	}
	if v, ok := m.turnEnds.Load(sess.WindowID); ok && v == msgID {
		return // same closing already fired for this window
	}
	if m.pool == nil {
		return // no registry — cannot attribute, do not count
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agentID, err := resolveAgentFromWindow(ctx, m.pool, sess.WindowID)
	if err != nil || agentID == "" {
		return
	}
	var role, taskID string
	if err := m.pool.QueryRow(ctx, pipelineLiveAgentSQL, agentID).Scan(&role, &taskID); err != nil {
		return // not a live pipeline agent (user topic, dead row, or gone)
	}

	// Dedup Store only after a close actually fired: a non-pipeline or
	// unattributable close must not swallow a later qualifying fire.
	m.turnEnds.Store(sess.WindowID, msgID)
	total := m.turnEndCount.Add(1)
	log.Printf("monitor: turn-end detected (shadow) window=%s agent=%s task=%s role=%s msg=%s close_age=%s total=%d",
		sess.WindowID, agentID, taskID, role, msgID, time.Since(at).Round(time.Millisecond), total)
}
