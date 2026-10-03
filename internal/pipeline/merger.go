// Merger agent (MAQ-15): the ready_to_merge conflict leg.
//
// A rebase conflict used to park the task needs-human unconditionally
// (parkMergeConflict) — even when the conflict was purely mechanical
// (both sides touched the same file additively). Under
// PIPELINE_MERGE_AGENT=1 the conflict instead arms a merger-agent episode:
//
//  1. ProcessMergeGH (merge.go) bumps the queue entry's attempts (the same
//     budget the CI cap uses), parks a merge_conflict marker row
//     (task_context) carrying attempt/base/branch/files, and releases the
//     entry back to 'pending' — in ONE tx, so no processor can claim a
//     released entry whose marker is not visible yet. Task stays
//     ready_to_merge.
//  2. mergerPass (this file, dispatch loop) spawns a fresh pipeline-merger
//     agent in the task worktree and enqueues the episode prompt (dedup
//     id `merger:<task>:<entry>:<attempt>`; a crash between spawn and
//     enqueue heals on the next tick).
//  3. The merger rebases, resolves conflicts PRESERVING both sides'
//     semantics, proves the resolution (go build + go test), and ends its
//     reply with exactly `VERDICT: merged` or `VERDICT: needs_human`.
//  4. mergerVerdictPass parses the verdict:
//     merged      → the entry re-enters the normal merge path unchanged
//                   (fetch → rebase → lease push → CI gate → squash);
//     needs_human → parkMergeConflict semantics: task → pending_approval,
//                   entry → conflict with the marker's files, notify.
//  5. Caps and backstops: each conflict bump consumes one attempt from the
//     entry's budget; past the cap the conflict parks needs-human exactly
//     as before. A stalled merger is watchdog-parked like reviewers/fixers;
//     a marker no dispatcher ever consumed goes stale-parked so CLI-only
//     deployments cannot wedge silently.
//
// Queue-entry states stay the source of truth: no new merge_queue status —
// the episode lives on a released 'pending' entry, and re-approving a
// parked task runs the merger path exactly once per fresh entry.

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// MergerSoulTemplate is the soul template dispatch clones per merger agent
// (migration 035 seeded it; 038 pivoted it to the conflict contract).
const MergerSoulTemplate = "pipeline-merger"

// mergerRole is the agents.role value for dispatched merger agents.
const mergerRole = "merger"

// VerdictMerged extends the frozen verdict vocabulary for the merger
// contract (reviewers keep approve/request_changes/needs_human).
const VerdictMerged = "merged"

// task_context kinds used by the merger episode.
const (
	// mergeConflictKind marks an armed episode: JSON
	// {"attempt":N,"base":...,"branch":...,"files":[...]} — the spawn
	// trigger AND the prompt/park payload.
	mergeConflictKind = "merge_conflict"
	// mergeVerdictKind consumes a marker: content `entry <id> attempt <n>`
	// — written when the episode's verdict is applied (merged, needs_human,
	// watchdog or stale park), so no episode ever spawns twice.
	mergeVerdictKind = "merge_verdict"
)

// mergerStaleAfter bounds an armed-but-never-consumed marker: past this age
// with no live merger, the dispatch pass parks needs-human instead of
// letting the entry bounce between claim and release forever (covers
// deployments that process merges via CLI without a dispatch loop).
const mergerStaleAfter = 2 * time.Hour

// mergeConflictMarker is the JSON payload of a merge_conflict row. The
// episode identity is (EntryID, Attempt) — attempts reset on a freshly
// enqueued entry (re-approve path), so the entry id must disambiguate
// episodes.
type mergeConflictMarker struct {
	EntryID int64    `json:"entry"`
	Attempt int      `json:"attempt"`
	Base    string   `json:"base"`
	Branch  string   `json:"branch"`
	Files   []string `json:"files"`
}

// episodeLabel is the consumed-marker content: the merge_verdict row that
// stops an episode from ever arming/spawning twice.
func (m mergeConflictMarker) episodeLabel() string {
	return fmt.Sprintf("entry %d attempt %d", m.EntryID, m.Attempt)
}

// mergeVerdictRe is the merger's line-anchored verdict vocabulary.
var mergeVerdictRe = regexp.MustCompile(`(?m)^[ \t]*VERDICT:[ \t]*(merged|needs_human)[ \t]*$`)

// ParseMergeVerdict extracts the merger verdict line (same contract shape
// as ParseVerdict, merger vocabulary).
func ParseMergeVerdict(text string) (string, bool) {
	m := mergeVerdictRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ---- conflict arm (called from ProcessMergeGH) ---------------------------

// armMergeConflictAgent is the MergeAgent conflict arm: consume one attempt
// from the entry's budget; below the cap, park the episode marker and
// release the entry for the merger path; at the cap, fall back to today's
// needs-human park. Everything the dispatch loop needs travels in the
// marker; the queue entry stays the source of truth (no new states).
func armMergeConflictAgent(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig, entry *db.MergeQueueEntry, base string, conflictErr *git.ConflictError) error {
	taskID := entry.TaskID
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMergeAttempts
	}
	attempts, err := db.BumpMergeAttempts(pool, entry.ID)
	if err != nil {
		return fmt.Errorf("pipeline: bumping attempts %d: %w", entry.ID, err)
	}
	if attempts >= maxAttempts {
		log.Printf("pipeline: merge %s conflict: merger budget exhausted (%d/%d) — parking needs-human", taskID, attempts, maxAttempts)
		return parkMergeConflict(ctx, pool, taskID, entry, conflictErr)
	}

	marker := mergeConflictMarker{
		EntryID: entry.ID,
		Attempt: attempts,
		Base:    base,
		Branch:  entry.Branch,
		Files:   conflictErr.Files,
	}
	payload, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("pipeline: encoding merge conflict marker %s: %w", taskID, err)
	}

	// ONE tx: marker + release. A processor claiming the released entry
	// must already see the marker (its in-flight guard reads it), so there
	// is no window where an episode exists without its payload.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, $3, $4)
	`, taskID, "merger", mergeConflictKind, string(payload)); err != nil {
		return fmt.Errorf("pipeline: writing merge conflict marker %s: %w", taskID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE merge_queue SET status = 'pending', started_at = NULL
		WHERE id = $1 AND status = 'merging'
	`, entry.ID); err != nil {
		return fmt.Errorf("pipeline: releasing %d for merger episode: %w", entry.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	notifyf(ctx, pool, "🔀 %s: rebase conflict on branch %s — merger agent resolving (attempt %d/%d). Conflicting files:\n%s%s",
		taskTitle(ctx, pool, taskID), entry.Branch, attempts, maxAttempts,
		strings.Join(conflictErr.Files, "\n"), prLinkSuffix(ctx, pool, taskID))
	log.Printf("pipeline: merge %s conflict: armed merger episode attempt %d/%d (%v)", taskID, attempts, maxAttempts, conflictErr)
	return nil
}

// mergerEpisodePending reports whether a merger episode is in flight for
// the task: a live merger agent, or an armed marker no verdict has consumed
// yet. ProcessMergeGH checks this BEFORE touching the worktree — a rebase
// against a worktree the merger is mid-resolution in would corrupt the
// episode.
func mergerEpisodePending(ctx context.Context, pool *pgxpool.Pool, taskID string) (bool, error) {
	live, err := liveMergerForTask(ctx, pool, taskID)
	if err != nil {
		return false, err
	}
	if live {
		return true, nil
	}
	return hasUnconsumedMergeConflict(ctx, pool, taskID)
}

func liveMergerForTask(ctx context.Context, pool *pgxpool.Pool, taskID string) (bool, error) {
	var live bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM agents
			WHERE task_id = $1 AND status <> 'dead' AND role = '` + mergerRole + `'
		)`, taskID).Scan(&live)
	return live, err
}

// hasUnconsumedMergeConflict: a merge_conflict marker exists AND its newest
// instance has no matching merge_verdict row. (The lateral join emits one
// row per marker; markerless tasks scan as ErrNoRows → nothing pending.)
func hasUnconsumedMergeConflict(ctx context.Context, pool *pgxpool.Pool, taskID string) (bool, error) {
	var consumed bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM task_context v
			WHERE v.task_id = t.id
			  AND v.kind = '` + mergeVerdictKind + `'
			  AND v.content = 'entry ' || (m.content::jsonb->>'entry')
			              || ' attempt ' || (m.content::jsonb->>'attempt')
		)
		FROM (SELECT id FROM tasks WHERE id = $1) t
		JOIN LATERAL (
			SELECT content FROM task_context
			WHERE task_id = t.id AND kind = '` + mergeConflictKind + `'
			ORDER BY created_at DESC LIMIT 1
		) m ON TRUE
	`, taskID).Scan(&consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // no marker — nothing pending
	}
	if err != nil {
		return false, err
	}
	return !consumed, nil
}

// latestMergeConflict returns the task's newest marker and whether any
// marker exists at all.
func latestMergeConflict(ctx context.Context, pool *pgxpool.Pool, taskID string) (mergeConflictMarker, bool, error) {
	var raw string
	err := pool.QueryRow(ctx, `
		SELECT content FROM task_context
		WHERE task_id = $1 AND kind = '` + mergeConflictKind + `'
		ORDER BY created_at DESC LIMIT 1
	`, taskID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return mergeConflictMarker{}, false, nil
	}
	if err != nil {
		return mergeConflictMarker{}, false, err
	}
	var marker mergeConflictMarker
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		return mergeConflictMarker{}, false, fmt.Errorf("parsing merge conflict marker %s: %w", taskID, err)
	}
	return marker, true, nil
}

// ---- dispatch-loop passes -------------------------------------------------

// mergerPass runs the merger spawn + prompt-heal arms (dispatch loop).
func mergerPass(ctx context.Context, pool *pgxpool.Pool, spawn ReviewSpawner) error {
	if err := mergerSpawnPass(ctx, pool, spawn); err != nil {
		return err
	}
	return mergerPromptPass(ctx, pool)
}

// mergerCandidatesSQL: pipeline tasks in ready_to_merge whose newest marker
// is unconsumed, with no live merger. The lateral carries the marker (the
// spawn payload) and its age (stale-park bound).
const mergerCandidatesSQL = `
SELECT t.id, t.worktree_path, m.content,
       EXTRACT(EPOCH FROM NOW() - m.created_at)
FROM tasks t
JOIN LATERAL (
    SELECT content, created_at FROM task_context
    WHERE task_id = t.id AND kind = '` + mergeConflictKind + `'
    ORDER BY created_at DESC LIMIT 1
) m ON TRUE
WHERE t.status = 'ready_to_merge'
  AND t.worktree_path IS NOT NULL AND t.worktree_path <> ''
  AND NOT EXISTS (
        SELECT 1 FROM agents a
        WHERE a.task_id = t.id AND a.status <> 'dead' AND a.role = '` + mergerRole + `')
  AND NOT EXISTS (
        SELECT 1 FROM task_context v
        WHERE v.task_id = t.id
          AND v.kind = '` + mergeVerdictKind + `'
          AND v.content = 'entry ' || (m.content::jsonb->>'entry')
                      || ' attempt ' || (m.content::jsonb->>'attempt'))`

func mergerSpawnPass(ctx context.Context, pool *pgxpool.Pool, spawn ReviewSpawner) error {
	rows, err := pool.Query(ctx, mergerCandidatesSQL)
	if err != nil {
		return err
	}
	type cand struct {
		taskID, worktree string
		marker           mergeConflictMarker
		ageSecs          float64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var raw string
		if err := rows.Scan(&c.taskID, &c.worktree, &raw, &c.ageSecs); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &c.marker); err != nil {
			rows.Close()
			return fmt.Errorf("parsing merge conflict marker %s: %w", c.taskID, err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range cands {
		// Stale episode: armed but never consumed and no merger ever lived
		// long enough to be watchdogged — park needs-human instead of
		// bouncing the entry between claim and release forever.
		if time.Duration(c.ageSecs*float64(time.Second)) > mergerStaleAfter {
			note := fmt.Sprintf("merger episode attempt %d armed but never dispatched for %s — parking needs-human", c.marker.Attempt, mergerStaleAfter)
			applied, err := parkMergerConflict(ctx, pool, "", c.taskID, c.marker, note)
			if err != nil {
				log.Printf("pipeline: merger stale park %s: %v", c.taskID, err)
				continue
			}
			if applied {
				log.Printf("pipeline: merger: stale episode on %s → needs_human", c.taskID)
				notifyf(ctx, pool, "🆘 %s: merger episode (attempt %d) never dispatched — parked needs-human. Resolve by hand or re-approve.%s",
					taskTitle(ctx, pool, c.taskID), c.marker.Attempt, prLinkSuffix(ctx, pool, c.taskID))
			}
			continue
		}

		agentID, err := mintAgentID(ctx, pool, mergerRole, c.taskID)
		if err != nil {
			log.Printf("pipeline: merger: mint %s: %v", c.taskID, err)
			continue
		}
		runnerType, model, err := resolveTemplateExecFor(ctx, pool, MergerSoulTemplate)
		if err != nil {
			log.Printf("pipeline: merger: resolve exec for %s: %v", c.taskID, err)
		}
		err = spawn.SpawnReviewer(ctx, ReviewSpawnParams{
			AgentID:        agentID,
			TaskID:         c.taskID,
			WorktreePath:   c.worktree,
			Role:           mergerRole,
			SoulTemplateID: MergerSoulTemplate,
			RunnerType:     runnerType,
			Model:          model,
		})
		if err != nil {
			if handleUniqueLiveBlocker(ctx, pool, c.taskID, err, DefaultImplementorIdleAfter, "merger") {
				continue
			}
			// Spawn infra failed (no tmux, bad worktree…): retry next tick;
			// the watchdog/stale bounds keep this from hiding forever.
			log.Printf("pipeline: merger: spawn %s for %s: %v", agentID, c.taskID, err)
			continue
		}
		if err := enqueueMergerPrompt(ctx, pool, agentID, c.taskID, c.marker); err != nil {
			// Prompt miss heals on the next tick (mergerPromptPass).
			log.Printf("pipeline: merger: enqueue prompt %s: %v", c.taskID, err)
		}
		log.Printf("pipeline: merger: spawned merger %s for task %s (attempt %d, worktree %s)",
			agentID, c.taskID, c.marker.Attempt, c.worktree)
	}
	return nil
}

// mergerPromptHealSQL: a live merger whose episode's prompt row is missing
// (crash between spawn and enqueue) — healed exactly once by the dedup'd
// external_msg_id.
const mergerPromptHealSQL = `
SELECT a.id, t.id, m.content
FROM tasks t
JOIN agents a ON a.task_id = t.id AND a.status <> 'dead' AND a.role = '` + mergerRole + `'
JOIN LATERAL (
    SELECT content FROM task_context
    WHERE task_id = t.id AND kind = '` + mergeConflictKind + `'
    ORDER BY created_at DESC LIMIT 1
) m ON TRUE
WHERE t.status = 'ready_to_merge'
  AND NOT EXISTS (
        SELECT 1 FROM agent_inbox i
        WHERE i.agent_id = a.id
          AND i.origin_channel = 'task'
          AND i.external_msg_id = 'merger:' || t.id || ':' || (m.content::jsonb->>'entry')
                      || ':' || (m.content::jsonb->>'attempt'))`

func mergerPromptPass(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, mergerPromptHealSQL)
	if err != nil {
		return err
	}
	type gap struct{ agentID, taskID string; marker mergeConflictMarker }
	var gaps []gap
	for rows.Next() {
		var g gap
		var raw string
		if err := rows.Scan(&g.agentID, &g.taskID, &raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &g.marker); err != nil {
			rows.Close()
			return fmt.Errorf("parsing merge conflict marker %s: %w", g.taskID, err)
		}
		gaps = append(gaps, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range gaps {
		log.Printf("pipeline: merger: healing missing prompt %s attempt %d", g.taskID, g.marker.Attempt)
		if err := enqueueMergerPrompt(ctx, pool, g.agentID, g.taskID, g.marker); err != nil {
			log.Printf("pipeline: merger: heal prompt %s: %v", g.taskID, err)
		}
	}
	return nil
}

// mergerPromptBody is the episode briefing. The merger soul carries the
// full method + verdict contract; this carries the specifics: branch, base,
// conflicting files, attempt.
func mergePromptBody(marker mergeConflictMarker, taskID string) string {
	return fmt.Sprintf(
		"Rebase-conflict episode (attempt %d) for task %s. Your cwd is the task worktree on branch %s. "+
			"Run `git fetch origin && git rebase origin/%s`, then resolve every conflicted file PRESERVING BOTH SIDES' "+
			"semantics — never drop a feature or change from either side to make a conflict disappear. "+
			"After resolving, `git add` the files and `git rebase --continue`, then prove the resolution: "+
			"`go build ./...` plus `go test` on the touched packages. If the proof fails, fix only what the resolution broke. "+
			"A semantic conflict you cannot resolve mechanically (both sides changed the same logic incompatibly) is a stop: "+
			"run `git rebase --abort`, leave the worktree clean, and end with VERDICT: needs_human. "+
			"End your reply with exactly one line: VERDICT: merged | VERDICT: needs_human.\n\n"+
			"Conflicting files:\n%s",
		marker.Attempt, taskID, marker.Branch, marker.Base, strings.Join(marker.Files, "\n"))
}

func enqueueMergerPrompt(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string, marker mergeConflictMarker) error {
	content, err := json.Marshal(map[string]any{
		"type":    "merge",
		"task_id": taskID,
		"attempt": marker.Attempt,
		"prompt":  mergePromptBody(marker, taskID),
	})
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, _, err = mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "pipeline",
		OriginChannel: "task",
		ExternalMsgID: fmt.Sprintf("merger:%s:%d:%d", taskID, marker.EntryID, marker.Attempt),
		Content:       content,
	})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- verdict pass ---------------------------------------------------------

// liveMergersSQL drives the merger verdict pass: live merger agents on
// tasks still in ready_to_merge, with their episode marker.
const liveMergersSQL = `
SELECT a.id, a.tmux_session, a.tmux_window, t.id, t.title, m.content
FROM agents a
JOIN tasks t ON t.id = a.task_id
JOIN LATERAL (
    SELECT content FROM task_context
    WHERE task_id = t.id AND kind = '` + mergeConflictKind + `'
    ORDER BY created_at DESC LIMIT 1
) m ON TRUE
WHERE a.role = '` + mergerRole + `'
  AND a.status <> 'dead'
  AND t.status = 'ready_to_merge'`

type liveMerger struct {
	agentID, session, window, taskID, title string
	marker                                  mergeConflictMarker
}

func mergerVerdictPass(ctx context.Context, pool *pgxpool.Pool, sessionName string, killWindow func(session, windowID string) error) error {
	mergers, err := scanLiveMergers(ctx, pool, liveMergersSQL, nil)
	if err != nil {
		return err
	}
	for _, m := range mergers {
		verdict, found, malformed := latestMergeVerdict(ctx, pool, m.agentID)
		if malformed {
			log.Printf("pipeline: merger: %s emitted a malformed VERDICT line (contract violation) — waiting for a well-formed one", m.agentID)
		}
		if !found {
			continue
		}
		switch verdict {
		case VerdictMerged:
			applied, err := applyMergerMerged(ctx, pool, m.agentID, m.taskID, m.marker)
			if err != nil {
				log.Printf("pipeline: merger: apply merged verdict %s: %v", m.taskID, err)
				continue
			}
			if applied {
				label := m.title
				if label == "" {
					label = taskTitle(ctx, pool, m.taskID)
				}
				notifyf(ctx, pool, "🔀 %s: merger agent resolved the rebase conflict (attempt %d) — re-entering the merge queue.%s",
					label, m.marker.Attempt, prLinkSuffix(ctx, pool, m.taskID))
				log.Printf("pipeline: merger: verdict merged on %s (%s) — merge path resumes", m.taskID, m.agentID)
			}
		case VerdictNeedsHuman:
			note := "merger: semantic conflict or failing proofs after resolution — needs human"
			applied, err := parkMergerConflict(ctx, pool, m.agentID, m.taskID, m.marker, note)
			if err != nil {
				log.Printf("pipeline: merger: park %s: %v", m.taskID, err)
				continue
			}
			if applied {
				label := m.title
				if label == "" {
					label = taskTitle(ctx, pool, m.taskID)
				}
				notifyf(ctx, pool, "🆘 %s: merger agent could not resolve the rebase conflict on branch %s. Conflicting files:\n%s\nTask parked needs-human.%s",
					label, m.marker.Branch, strings.Join(m.marker.Files, "\n"), prLinkSuffix(ctx, pool, m.taskID))
				log.Printf("pipeline: merger: verdict needs_human on %s (%s) — parked", m.taskID, m.agentID)
			}
		}
		killReviewerPane(sessionName, m.session, m.window, killWindow)
	}
	return nil
}

func scanLiveMergers(ctx context.Context, pool *pgxpool.Pool, sql string, args []any) ([]liveMerger, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mergers []liveMerger
	for rows.Next() {
		var m liveMerger
		var raw string
		if err := rows.Scan(&m.agentID, &m.session, &m.window, &m.taskID, &m.title, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &m.marker); err != nil {
			return nil, fmt.Errorf("parsing merge conflict marker %s: %w", m.taskID, err)
		}
		mergers = append(mergers, m)
	}
	return mergers, rows.Err()
}

// latestMergeVerdict scans the merger's newest assistant outbox rows,
// newest first; the first well-formed verdict wins.
func latestMergeVerdict(ctx context.Context, pool *pgxpool.Pool, agentID string) (verdict string, found, malformed bool) {
	rows, err := pool.Query(ctx, `
		SELECT content->>'text' FROM agent_outbox
		WHERE agent_id = $1 AND content ? 'text'
		ORDER BY created_at DESC LIMIT 10
	`, agentID)
	if err != nil {
		return "", false, false
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return "", false, false
		}
		if v, ok := ParseMergeVerdict(text); ok {
			return v, true, malformed
		}
		if HasMalformedVerdictLine(text) {
			malformed = true
		}
	}
	return "", false, malformed
}

// applyMergerMerged consumes the episode (verdict row + retire) after a
// merged verdict. The queue entry is already 'pending' (the conflict arm
// released it) — the next merge pass re-runs the normal path: fetch →
// rebase (clean now) → lease push → CI gate → squash. No merge code runs
// here; the queue entry stays the source of truth.
func applyMergerMerged(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string, marker mergeConflictMarker) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// Guard on the task still being ready_to_merge: a late verdict on an
	// episode that raced to a park is recorded as a no-op.
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&status); err != nil {
		return false, err
	}
	if status != "ready_to_merge" {
		tx.Rollback(ctx)
		retireMerger(ctx, pool, agentID)
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, $3, $4)
	`, taskID, agentID, mergeVerdictKind, marker.episodeLabel()); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE agents SET status='dead', last_seen=NOW() WHERE id=$1 AND status <> 'dead'`, agentID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// parkMergerConflict is the merger episode's needs-human landing (verdict,
// watchdog and stale-park all land here): one tx that parks the task
// (guarded ready_to_merge), consumes the marker, conflicts the released
// pending entry with the marker's files, and retires the agent. The
// guarded entry UPDATE never touches a 'merging' entry a processor holds —
// processors guard on task status, which this tx flips atomically with the
// entry, so a merge can never complete behind a needs-human verdict.
func parkMergerConflict(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string, marker mergeConflictMarker, note string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET status = 'pending_approval'
		WHERE id = $1 AND status = 'ready_to_merge'
	`, taskID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		tx.Rollback(ctx)
		if agentID != "" {
			retireMerger(ctx, pool, agentID)
		}
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, $3, $4)
	`, taskID, agentID, mergeVerdictKind, marker.episodeLabel()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE merge_queue
		SET    status = 'conflict', conflict_files = $2, completed_at = NOW()
		WHERE  task_id = $1 AND status = 'pending'
	`, taskID, marker.Files); err != nil {
		return false, err
	}
	if agentID != "" {
		if _, err := tx.Exec(ctx, `UPDATE agents SET status='dead', last_seen=NOW() WHERE id=$1 AND status <> 'dead'`, agentID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	db.AddObservation(pool, taskID, "merger", note)
	return true, nil
}

func retireMerger(ctx context.Context, pool *pgxpool.Pool, agentID string) {
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='dead', last_seen=NOW() WHERE id=$1 AND status <> 'dead'`, agentID); err != nil {
		log.Printf("pipeline: merger: retire %s: %v", agentID, err)
	}
}

// ---- watchdog arm ----------------------------------------------------------

// mergerWatchdogPass parks a stalled merger episode: a live merger on a
// ready_to_merge task with no outbox activity AND no transcript growth for
// longer than the timeout (same stall definition as reviewers/fixers, MAQ-9).
func mergerWatchdogPass(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration, sessionName string, killWindow func(session, windowID string) error) error {
	stallFilter := `
  AND a.started_at < NOW() - make_interval(secs => $1)
  AND NOT EXISTS (
        SELECT 1 FROM agent_outbox o
        WHERE o.agent_id = a.id AND o.created_at > NOW() - make_interval(secs => $1))
  AND (a.last_transcript_at IS NULL
       OR a.last_transcript_at < NOW() - make_interval(secs => $1))`
	mergers, err := scanLiveMergers(ctx, pool, liveMergersSQL+stallFilter, []any{timeout.Seconds()})
	if err != nil {
		return err
	}
	for _, m := range mergers {
		note := fmt.Sprintf("watchdog: merger stalled past %s — needs human", timeout)
		applied, err := parkMergerConflict(ctx, pool, m.agentID, m.taskID, m.marker, note)
		if err != nil {
			return err
		}
		if applied {
			label := m.title
			if label == "" {
				label = taskTitle(ctx, pool, m.taskID)
			}
			log.Printf("pipeline: merger: watchdog retired stalled merger %s on %s → needs_human", m.agentID, m.taskID)
			notifyf(ctx, pool, "🆘 %s: %s%s", label, note, prLinkSuffix(ctx, pool, m.taskID))
			killReviewerPane(sessionName, m.session, m.window, killWindow)
		}
	}
	return nil
}
