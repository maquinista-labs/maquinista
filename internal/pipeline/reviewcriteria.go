package pipeline

// MAQ-35: per-repository review criteria. A repo can carry a MAQUINISTA.md
// at its root whose content is BINDING review criteria: dispatch loads it
// when building the reviewer round prompt and injects it as a dedicated
// section. The reviewer's cwd IS a repo root (every task worktree is a
// full checkout), so the worktree copy is "MAQUINISTA.md at the repo root"
// for the round in flight — a branch proposing new criteria is reviewed
// against its own proposal. Loading is best-effort on every path: a
// missing file (the common case — most repos carry no criteria) or an
// unreadable one ships the prompt without the section. A criteria file
// must never block a review round.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// reviewCriteriaFile is the per-repo criteria file name, loaded from the
// task worktree root (the repo root the reviewer reviews).
const reviewCriteriaFile = "MAQUINISTA.md"

// maxReviewCriteriaChars caps the injected section. A criteria file is a
// short, human-maintained contract; an oversized one is truncated (still
// injected — partial criteria with a visible truncation marker beat none).
const maxReviewCriteriaChars = 4000

// taskWorktreePath returns the task's worktree ("" when unset).
func taskWorktreePath(ctx context.Context, q queryRow, taskID string) (string, error) {
	var wt *string
	if err := q.QueryRow(ctx, `SELECT worktree_path FROM tasks WHERE id = $1`, taskID).Scan(&wt); err != nil {
		return "", err
	}
	if wt == nil {
		return "", nil
	}
	return *wt, nil
}

// loadReviewCriteria reads MAQUINISTA.md from the repo root (the task
// worktree). Returns "" when absent (not an error), unreadable (logged),
// or blank. Trimmed and size-capped.
func loadReviewCriteria(worktree string) string {
	if strings.TrimSpace(worktree) == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(worktree, reviewCriteriaFile))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("pipeline: dispatch: read %s in %s: %v — prompt ships without review criteria", reviewCriteriaFile, worktree, err)
		}
		return ""
	}
	content := strings.TrimSpace(string(raw))
	if content == "" {
		return ""
	}
	if runes := []rune(content); len(runes) > maxReviewCriteriaChars {
		content = string(runes[:maxReviewCriteriaChars]) + "\n… (truncated)"
	}
	return content
}

// renderReviewCriteria frames the repo's criteria for the round prompt:
// BINDING — they constrain the verdict, unlike the human-comments section
// (input only). A criterion demanding human approval escalates unless an
// explicit human approval is visible in the PR comments. Returns "" for
// empty content (the prompt ships without the section).
func renderReviewCriteria(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Repository review criteria (loaded from " + reviewCriteriaFile +
		" at the repo root — BINDING: these criteria constrain your verdict exactly like the verdict contract above. " +
		"Where a criterion requires human approval, an explicit human approval visible in the PR comments below satisfies it; " +
		"anything else escalates — request_changes or needs_human — never approve on your own authority):\n\n")
	b.WriteString(content)
	return b.String()
}
