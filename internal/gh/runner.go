// Package gh implements the GitHub side of pipeline GitHub traffic via the
// gh CLI: the production GhRunner for pipeline.MergeConfig (merge mode +
// reviewer PR-comment weighing) and the production pipeline.CommentSource
// for the comment-command surface (MAQ-12) — thin, ugly-on-purpose wrappers
// around gh subcommands, so all branching stays in the pipeline package
// where it is testable.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/maquinista-labs/maquinista/internal/pipeline"
)

// Runner executes gh against the current directory's repo.
type Runner struct{}

// New returns the CLI-backed GhRunner.
func New() *Runner { return &Runner{} }

// rollup is the slice of `gh pr view --json statusCheckRollup` we consume.
type rollup struct {
	StatusCheckRollup []struct {
		Status     string `json:"status"`     // COMPLETED | IN_PROGRESS | PENDING | ...
		Conclusion string `json:"conclusion"` // SUCCESS | FAILURE | ... (when COMPLETED)
	} `json:"statusCheckRollup"`
}

// PRChecks aggregates the PR's CI checks into the pipeline's vocabulary:
// "green" (all passed), "pending" (still running), "failed" (any failure),
// or "none" (no checks configured — vacuously green).
func (Runner) PRChecks(ctx context.Context, pr int) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view",
		fmt.Sprint(pr), "--json", "statusCheckRollup").Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view %d: %w", pr, err)
	}
	return parseChecks(out)
}

// parseChecks maps a statusCheckRollup JSON payload onto the checks state.
func parseChecks(out []byte) (string, error) {
	var r rollup
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	if len(r.StatusCheckRollup) == 0 {
		return "none", nil
	}
	pending, failed := false, false
	for _, c := range r.StatusCheckRollup {
		switch {
		case c.Status != "COMPLETED":
			pending = true
		case c.Conclusion != "SUCCESS" && c.Conclusion != "NEUTRAL" && c.Conclusion != "SKIPPED":
			failed = true
		}
	}
	switch {
	case failed:
		return "failed", nil
	case pending:
		return "pending", nil
	default:
		return "green", nil
	}
}

// PRMergeSquash squash-merges the PR. Branch deletion is handled by the
// merge flow itself, so it is not requested here.
func (Runner) PRMergeSquash(ctx context.Context, pr int) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "merge", fmt.Sprint(pr), "--squash")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr merge %d --squash: %s: %w", pr, string(out), err)
	}
	return nil
}

// PRComments returns the PR's conversation comments (issue comments)
// created after since, oldest first. The API's `since` filters on update
// time (edited old comments resurface), so the result is re-filtered on
// created_at here — one fetch, exact window. A zero since returns every
// comment (the reviewer-prompt caller).
func (Runner) PRComments(ctx context.Context, pr int, since time.Time) ([]pipeline.PRComment, error) {
	uri := fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments?per_page=100&since=%s",
		pr, since.UTC().Format(time.RFC3339))
	out, err := exec.CommandContext(ctx, "gh", "api", uri).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api issue comments %d: %w", pr, err)
	}
	return parseComments(out, since)
}

// parseComments maps the issue-comments JSON payload onto []pipeline.PRComment,
// flagging bot authors from every shape the field has taken (REST type,
// "[bot]" login suffix) and dropping anything not created after since.
func parseComments(out []byte, since time.Time) ([]pipeline.PRComment, error) {
	var raw []struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh api comments: %w", err)
	}
	var comments []pipeline.PRComment
	for _, c := range raw {
		if !c.CreatedAt.After(since) {
			continue
		}
		comments = append(comments, pipeline.PRComment{
			ID:        c.ID,
			Author:    c.User.Login,
			IsBot:     c.User.Type == "Bot" || strings.HasSuffix(c.User.Login, "[bot]"),
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return comments, nil
}

// PRPostComment posts body as a new comment on the PR.
func (Runner) PRPostComment(ctx context.Context, pr int, body string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "comment", fmt.Sprint(pr), "--body", body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr comment %d: %s: %w", pr, string(out), err)
	}
	return nil
}

// PRPostCommentURL posts body as a new PR comment and returns the comment's
// URL (gh prints it on success) — the delivery confirmation the MAQ-24
// Telegram-reply path quotes back into the Pipeline topic.
func (r Runner) PRPostCommentURL(ctx context.Context, pr int, body string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "comment", fmt.Sprint(pr), "--body", body)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr comment %d: %w", pr, err)
	}
	return parsePRCommentURL(out)
}

// parsePRCommentURL extracts the comment URL from `gh pr comment` stdout
// (e.g. https://github.com/o/r/pull/7#issuecomment-123456).
func parsePRCommentURL(out []byte) (string, error) {
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("gh pr comment: empty output — no comment url")
	}
	if !strings.Contains(url, "#issuecomment-") {
		return "", fmt.Errorf("gh pr comment: unexpected output %q — no comment url", url)
	}
	return url, nil
}

// PRUpdateBranch merges the PR's base branch into the PR branch via the
// update-branch endpoint (the "Update branch" button). expectedHeadSHA makes
// GitHub reject the request when the branch moved underneath us (HTTP 409).
// A 422 — base cannot be merged into the branch cleanly — maps to
// pipeline.ErrMergeUpConflict (a deterministic conflict, not infrastructure);
// a 409 maps to pipeline.ErrMergeUpRace (state changed, retry from scratch).
func (Runner) PRUpdateBranch(ctx context.Context, pr int, expectedHeadSHA string) error {
	uri := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/update-branch", pr)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "api", uri, "-X", "PUT", "-f", "expected_head_sha="+expectedHeadSHA)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if classified := parseUpdateBranchErr(stderr.String()); classified != nil {
			return classified
		}
		return fmt.Errorf("gh api update-branch %d: %s: %w", pr, strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// parseUpdateBranchErr classifies an update-branch failure from gh's stderr:
// 422 = base cannot merge into the branch cleanly; 409 = the head branch
// moved since expected_head_sha. nil = everything else (infrastructure —
// the caller decides).
func parseUpdateBranchErr(stderr string) error {
	switch {
	case strings.Contains(stderr, "(HTTP 422"):
		return pipeline.ErrMergeUpConflict
	case strings.Contains(stderr, "(HTTP 409"):
		return pipeline.ErrMergeUpRace
	default:
		return nil
	}
}

// PRState returns the PR's state: "OPEN", "CLOSED" or "MERGED" (the MAQ-24
// reply-comment guard: only an open PR accepts comments that a review round
// will weigh).
func (Runner) PRState(ctx context.Context, pr int) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", fmt.Sprint(pr),
		"--json", "state", "--jq", ".state").Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view %d state: %w", pr, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// IsCollaborator reports whether login is a repo collaborator — the default
// allowlist when PIPELINE_GH_ALLOWED_LOGINS is unset. gh api exits non-zero
// on the 404 (not a collaborator); any other failure is an error so the
// caller can retry instead of silently dropping the command.
func (Runner) IsCollaborator(ctx context.Context, login string) (bool, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/{owner}/{repo}/collaborators/"+login)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "(HTTP 404") {
			return false, nil
		}
		return false, fmt.Errorf("gh api collaborators/%s: %s: %w", login, strings.TrimSpace(stderr.String()), err)
	}
	return true, nil
}

// ReactToComment adds a +1 reaction to a comment — the command ack.
func (Runner) ReactToComment(ctx context.Context, commentID int64) error {
	uri := fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d/reactions", commentID)
	cmd := exec.CommandContext(ctx, "gh", "api", uri, "-f", "content=+1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh api reactions %d: %s: %w", commentID, string(out), err)
	}
	return nil
}

// PRHeadBranch returns the PR's head branch name (id-less resolution
// fallback: merge_queue rows name the same line of work by branch).
func (Runner) PRHeadBranch(ctx context.Context, pr int) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", fmt.Sprint(pr),
		"--json", "headRefName", "--jq", ".headRefName").Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view %d headRefName: %w", pr, err)
	}
	return strings.TrimSpace(string(out)), nil
}
