package pipeline

// Comment-driven re-round (MAQ-30). MAQ-16 gave the reviewer eyes on human
// PR comments and MAQ-24 lands Telegram replies as PR comments — but
// nothing re-spawned a round when a human commented after the verdict:
//
//   - a task parked pending_approval + "already fixed X on the branch" sat
//     until the operator ran resolve/approve by hand;
//   - a task in ready_to_merge + a human objection in the PR was merged
//     anyway by the auto-merge drain (a comment was not a gate input);
//   - a fixer round landing mid-round could miss a comment posted after its
//     prompt was built (that one stays inherent — the next reviewer reads
//     it, which is why an in-flight round is a no-op below).
//
// The missing piece was the trigger: PR comment → round. The existing 60 s
// comment pass (RunCommentCommands) already fetches every comment on open
// pipeline PRs; the dispatcher now routes NON-verb comments from allowed
// logins into the state machine here:
//
//   - pending_approval / changes_requested → a fresh fixer round keyed to
//     the current review episode, with the comment quoted as the work order
//     (and the reviewer findings as context). A pending_approval park is
//     flipped back to changes_requested first — the state every fixer leg
//     (watchdog arm, prompt heal, candidates) keys on. The existing loop
//     closure re-enters review, and the round cap still applies at the
//     next verdict: a comment never touches review_rounds, so it cannot
//     reset or bypass the cap.
//   - ready_to_merge → guarded flip back to review; the dispatch loop's
//     spawn pass then mints the fresh reviewer (posting the MAQ-25 🔁
//     round-start marker and folding the comment into the prompt as MAQ-16
//     gate input). Any PENDING merge_queue entry is failed in the same
//     transaction so the drain can neither merge over the objection nor
//     churn claim-and-release behind a task that left ready_to_merge. An
//     entry already held in 'merging' wins: the flip is skipped (the human
//     was too late for this drain).
//   - review → no-op (a round is in flight; the next prompt build reads
//     the comment). done / non-pipeline / unknown statuses → no-op.
//
// Exactly-once rides the shared gh_comment_commands claim (PK = the GitHub
// comment id, ON CONFLICT DO NOTHING): a duplicate comment never
// double-spawns. Bot authors and non-allowed logins never trigger (both are
// claimed, so the silence is remembered); a verb comment (maquinista
// approve) never reaches this path — the verb dispatcher claims it under
// the verb's own audit row first. The fixer legs additionally respect the
// worktree_path guard (a fixer without a worktree cannot exist), the
// merger-episode guard (a rebase resolution in flight must not be
// corrupted — same check ProcessMergeGH makes), and the episode dedup (one
// fixer per review episode, shared with fixerCandidatesSQL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// reroundVerb is the gh_comment_commands.verb audit value stamped on
// non-command comments (they never parse as a verb).
const reroundVerb = "reround"

// dispatchCommentRound is the MAQ-30 arm of DispatchCommentCommand: every
// non-command comment on a watched PR lands here. Contract mirrors the verb
// arm — returns the disposition recorded; a transient error ("", err)
// claims nothing so the next pass retries the comment; everything after the
// claim is recorded once and never retried (the claim is the memory).
func dispatchCommentRound(ctx context.Context, d CommentDeps, authz *commentAuthorizer, pr int, c PRComment) (string, error) {
	// Bot authors never trigger and never even cost a collaborator check.
	if c.IsBot {
		if _, err := claimCommentCommand(ctx, d.Pool, c, reroundVerb, ""); err != nil {
			return "", err
		}
		if err := setCommentDisposition(ctx, d.Pool, c.ID, DispNoOp, "bot author never triggers a round"); err != nil {
			log.Printf("pipeline: comment %d disposition: %v", c.ID, err)
		}
		return DispNoOp, nil
	}

	allowed, err := authorizeCommenter(ctx, d, authz, c.Author)
	if err != nil {
		return "", err // transient: retry next pass, nothing claimed
	}
	if !allowed {
		// Claim so the silence is remembered (same as the verb arm).
		if _, err := claimCommentCommand(ctx, d.Pool, c, reroundVerb, ""); err != nil {
			return "", err
		}
		if err := setCommentDisposition(ctx, d.Pool, c.ID, DispUnauthorized, "login not allowed"); err != nil {
			log.Printf("pipeline: comment %d disposition: %v", c.ID, err)
		}
		return DispUnauthorized, nil
	}

	taskID, err := resolveTaskByPR(ctx, d.Pool, d.Source, pr)
	if err != nil {
		if !errors.Is(err, errNoTaskForPR) {
			return "", err // transient
		}
		if _, cerr := claimCommentCommand(ctx, d.Pool, c, reroundVerb, ""); cerr != nil {
			return "", cerr
		}
		if derr := setCommentDisposition(ctx, d.Pool, c.ID, DispNoOp,
			fmt.Sprintf("no task maps to PR #%d", pr)); derr != nil {
			log.Printf("pipeline: comment %d disposition: %v", c.ID, derr)
		}
		return DispNoOp, nil
	}

	claimed, err := claimCommentCommand(ctx, d.Pool, c, reroundVerb, taskID)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "duplicate", nil // already processed — exactly-once holds
	}

	detail, err := triggerCommentRound(ctx, d, taskID, c)
	disp := DispOK
	if detail != "" {
		disp = DispNoOp
	} else if err != nil {
		disp = DispError
	}
	if derr := setCommentDisposition(ctx, d.Pool, c.ID, disp, detail); derr != nil {
		log.Printf("pipeline: comment %d disposition: %v", c.ID, derr)
	}
	return disp, err
}

// triggerCommentRound runs the task-state machine for one claimed,
// authorized, non-verb comment. A non-empty noop is the clean no-op detail
// (no state touched); err is a post-claim failure (recorded as disposition
// 'error' — the comment is consumed; a fresh comment retries).
func triggerCommentRound(ctx context.Context, d CommentDeps, taskID string, c PRComment) (noop string, err error) {
	var status string
	var rounds int
	var worktree, issueID string
	if err := d.Pool.QueryRow(ctx, `
		SELECT status, review_rounds, COALESCE(worktree_path, ''),
		       COALESCE(metadata->>'ticket_issue_id', '')
		FROM tasks WHERE id = $1
	`, taskID).Scan(&status, &rounds, &worktree, &issueID); err != nil {
		return "", fmt.Errorf("pipeline: reround: loading task %s: %w", taskID, err)
	}
	if issueID == "" {
		return "task is not on the pipeline — no rounds to re-open", nil
	}

	switch status {
	case "review":
		return "review round in flight — the comment rides the next round's prompt", nil
	case "done":
		return "task is done — nothing to re-open", nil
	case "ready_to_merge":
		return reopenReview(ctx, d, taskID, worktree, c)
	case "pending_approval", "changes_requested":
		return spawnCommentFixer(ctx, d, taskID, rounds, status, worktree, c)
	default:
		return fmt.Sprintf("task status %q takes no comment trigger", status), nil
	}
}

// reopenReview is the ready_to_merge arm: a human objection becomes a gate
// input by flipping the task back to review (the dispatch loop's spawn pass
// does everything else — fresh reviewer, 🔁 marker, MAQ-16 comment fold-in)
// and failing any pending merge_queue entry in the same tx.
func reopenReview(ctx context.Context, d CommentDeps, taskID, worktree string, c PRComment) (noop string, err error) {
	// Worktree guard: the reviewer spawn pass requires one; flipping without
	// it would strand the task in review, unwatched by every leg.
	if worktree == "" {
		return "task has no worktree — a fresh review round could not spawn", nil
	}
	// A drain pass holding the entry in 'merging' will complete the squash;
	// flipping now would leave the task in review behind an already-merged
	// PR. Skip — the objection arrives too late for this merge.
	var merging bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM merge_queue WHERE task_id = $1 AND status = 'merging')`,
		taskID).Scan(&merging); err != nil {
		return "", fmt.Errorf("pipeline: reround: merge-in-flight check %s: %w", taskID, err)
	}
	if merging {
		return "merge in flight — the objection cannot gate this drain", nil
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx, `
		UPDATE tasks SET status = 'review'
		WHERE id = $1 AND status = 'ready_to_merge'
		RETURNING id
	`, taskID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "task raced out of ready_to_merge", nil
		}
		return "", fmt.Errorf("pipeline: reround: flip %s to review: %w", taskID, err)
	}
	// Fail the pending entry: terminal, so the drain neither merges over the
	// objection nor churns claim-and-release while the fresh round runs. The
	// next approval enqueues a fresh entry (the dedup only counts live ones).
	if _, err := tx.Exec(ctx, `
		UPDATE merge_queue
		SET    status = 'failed', error_msg = $2, completed_at = NOW()
		WHERE  task_id = $1 AND status = 'pending'
	`, taskID, "human PR comment re-opened review — a fresh entry enqueues at the next approval"); err != nil {
		return "", fmt.Errorf("pipeline: reround: failing pending merge entry for %s: %w", taskID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}

	notifyTaskf(ctx, d.Pool, taskID, "💬 %s: @%s commented on the PR — back to review for a fresh round; the objection gates the merge.",
		taskTitle(ctx, d.Pool, taskID), c.Author)
	log.Printf("pipeline: reround: %s flipped ready_to_merge → review by @%s's PR comment (pending merge entry failed)", taskID, c.Author)
	return "", nil
}

// spawnCommentFixer is the pending_approval/changes_requested arm: a fresh
// fixer for the current review episode, work order = the comment. Mirrors
// fixerPass's ordering (spawn FIRST, then the episode tx — a spawn failure
// must not consume the episode) and reuses its guards.
func spawnCommentFixer(ctx context.Context, d CommentDeps, taskID string, rounds int, status, worktree string, c PRComment) (noop string, err error) {
	// Worktree guard — never bypassed (the fixer works the same worktree/PR).
	if worktree == "" {
		return "task has no worktree — a fixer round cannot spawn", nil
	}
	// Merger-episode guard: a rebase resolution in flight must not be
	// corrupted by a concurrent fixer in the same worktree (the same check
	// ProcessMergeGH makes before touching the worktree).
	pending, err := mergerEpisodePending(ctx, d.Pool, taskID)
	if err != nil {
		return "", fmt.Errorf("pipeline: reround: merger episode check %s: %w", taskID, err)
	}
	if pending {
		return "merger episode in flight — the comment rides the post-merge review", nil
	}
	// One fixer per review episode, shared with fixerCandidatesSQL: a live
	// fixer pane or a claimed episode both make this comment ride the next
	// reviewer prompt instead of minting a second fixer.
	var liveFixer bool
	if err := d.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM agents
			WHERE task_id = $1 AND status <> 'dead' AND role = '`+fixerRole+`')
	`, taskID).Scan(&liveFixer); err != nil {
		return "", fmt.Errorf("pipeline: reround: live fixer check %s: %w", taskID, err)
	}
	if liveFixer {
		return "fixer round in flight — the comment rides the next review round", nil
	}
	var episodeClaimed bool
	if err := d.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM task_context
			WHERE task_id = $1 AND kind = 'fix' AND content = 'round ' || $2::text)
	`, taskID, strconv.Itoa(rounds)).Scan(&episodeClaimed); err != nil {
		return "", fmt.Errorf("pipeline: reround: episode check %s: %w", taskID, err)
	}
	if episodeClaimed {
		return fmt.Sprintf("fixer episode %d already claimed — the comment rides the next review round", rounds), nil
	}

	if d.Spawn == nil {
		return "", fmt.Errorf("pipeline: reround: no spawner wired (CommentDeps.Spawn)")
	}
	agentID, err := mintAgentID(ctx, d.Pool, fixerRole, taskID)
	if err != nil {
		return "", fmt.Errorf("pipeline: reround: mint fixer for %s: %w", taskID, err)
	}
	runnerType, model, err := resolveTemplateExecFor(ctx, d.Pool, FixerSoulTemplate)
	if err != nil {
		// Non-fatal: empty overrides fall through to the runner's own
		// resolution chain (same stance as the fixer pass).
		log.Printf("pipeline: reround: resolve fixer exec for %s: %v", taskID, err)
	}
	if err := d.Spawn.SpawnReviewer(ctx, ReviewSpawnParams{
		AgentID:        agentID,
		TaskID:         taskID,
		WorktreePath:   worktree,
		Role:           fixerRole,
		SoulTemplateID: FixerSoulTemplate,
		RunnerType:     runnerType,
		Model:          model,
	}); err != nil {
		if isUniqueLiveErr(err) {
			// Another spawn won the one live slot this instant (the dispatch
			// loop's fixer pass, a reviewer, a merger — any live agent) — the
			// episode is covered by whichever pane won; clean no-op.
			return "a concurrent spawn holds the live slot — the episode is covered", nil
		}
		return "", fmt.Errorf("pipeline: reround: spawn fixer %s for %s: %w", agentID, taskID, err)
	}

	// Episode tx: unpark (pending_approval is not a state any fixer leg
	// watches — changes_requested is) + the fix row that claims the episode
	// and dedups the one-liner. One tx, so the task is never parked-with-
	// fixer-marker-less nor marked-without-moving.
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if status == "pending_approval" {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status = 'changes_requested'
			WHERE id = $1 AND status = 'pending_approval'
		`, taskID); err != nil {
			return "", fmt.Errorf("pipeline: reround: unpark %s: %w", taskID, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'fix', $3)
	`, taskID, agentID, "round "+strconv.Itoa(rounds)); err != nil {
		return "", fmt.Errorf("pipeline: reround: insert fix row for %s: %w", taskID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	notifyTaskf(ctx, d.Pool, taskID, "🔧 %s: fixer round %d started — working @%s's PR comment.",
		taskTitle(ctx, d.Pool, taskID), rounds, c.Author)

	// The fix prompt carries the comment as the work order; a failure logs —
	// fixerPromptHealSQL heals request_changes episodes on the next tick
	// (parked-without-verdict tasks have no heal source; the watchdog bounds
	// a promptless fixer either way).
	if err := enqueueCommentFixPrompt(ctx, d.Pool, agentID, taskID, rounds, c); err != nil {
		log.Printf("pipeline: reround: fix prompt for %s (fixer %s spawned, prompt may be missing): %v", taskID, agentID, err)
	}
	// MAQ-25 narration, reason = the comment (the same 🔧 pickup line the
	// standard fixer leg posts; the round-scoped needle dedups across legs).
	postPickupComment(ctx, d.Pool, d.Gh, taskID,
		fixerPickupNeedle(rounds),
		fixerPickupBody(taskIssueKey(ctx, d.Pool, taskID), rounds, commentPickupReason(c.Body)))
	log.Printf("pipeline: reround: spawned fixer %s for %s (round %d) on @%s's PR comment", agentID, taskID, rounds, c.Author)
	return "", nil
}

// latestVerdictAgent returns the agent that recorded the task's newest
// request_changes verdict — the findings source for a comment-triggered fix
// prompt. ok=false when the task has no request_changes verdict (a
// needs-human or merge-gate park): the prompt then carries the comment only.
func latestVerdictAgent(ctx context.Context, pool *pgxpool.Pool, taskID string) (agentID string, ok bool, err error) {
	err = pool.QueryRow(ctx, `
		SELECT agent_id FROM task_context
		WHERE task_id = $1 AND kind = 'verdict'
		  AND content LIKE 'VERDICT: request_changes%'
		ORDER BY created_at DESC LIMIT 1
	`, taskID).Scan(&agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return agentID, true, nil
}

// enqueueCommentFixPrompt enqueues the comment-triggered fix briefing under
// the standard fix external_msg_id — the inbox dedup then also covers the
// prompt-heal path (which rebuilds from reviewer findings alone; the
// comment lives on the PR for the next reviewer regardless).
func enqueueCommentFixPrompt(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string, round int, c PRComment) error {
	findings := ""
	if vAgent, ok, err := latestVerdictAgent(ctx, pool, taskID); err != nil {
		log.Printf("pipeline: reround: verdict agent lookup %s: %v (prompt ships without findings)", taskID, err)
	} else if ok {
		if f, ferr := latestFindings(ctx, pool, vAgent); ferr != nil {
			log.Printf("pipeline: reround: findings read %s: %v (prompt ships without them)", taskID, ferr)
		} else {
			findings = f
		}
	}
	content, err := json.Marshal(map[string]any{
		"type":    "fix",
		"task_id": taskID,
		"round":   round,
		"prompt":  commentRoundFixPromptBody(taskID, round, c.Author, c.Body, findings),
	})
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, _, err := mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "pipeline",
		OriginChannel: "task",
		ExternalMsgID: fmt.Sprintf("fix:%s:%d", taskID, round),
		Content:       content,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// commentRoundFixPromptBody is the comment-triggered fix briefing. The
// fixer soul carries the method (same as fixerPromptBody); this carries the
// work order: the triggering human comment, plus the reviewer's findings as
// context when the task has a request_changes verdict.
func commentRoundFixPromptBody(taskID string, round int, author, comment, findings string) string {
	comment = strings.TrimSpace(comment)
	if runes := []rune(comment); len(runes) > maxCommentBodyChars {
		comment = string(runes[:maxCommentBodyChars]) + " …"
	}
	b := fmt.Sprintf(
		"Fix round for task %s (review round %d re-opened by a human comment on the PR). "+
			"Your cwd is the task worktree — same branch, same PR as the rounds before. "+
			"Address the human comment below (plus the reviewer findings, when present), "+
			"re-run the affected proofs, and push. "+
			"Finish with: maquinista-done %s \"<summary naming what the comment asked for>\".\n\n"+
			"Human comment from @%s that triggered this round:\n%s",
		taskID, round, taskID, author, comment)
	if strings.TrimSpace(findings) != "" {
		b += "\n\nReviewer findings (latest round, may predate the comment):\n" + findings
	}
	return b
}
