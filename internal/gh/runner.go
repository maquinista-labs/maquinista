// Package gh implements the GitHub side of pipeline GitHub traffic via the
// gh CLI (merge mode MAQ-16 PR comments). It is the production GhRunner for
// the pipeline package — a thin, ugly-on-purpose wrapper around a handful of
// subcommands, so all branching stays in the pipeline package where it is
// testable.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

// PRComment is one issue comment on a pull request (the slice of
// `gh pr view --json comments` callers need).
type PRComment struct {
	Author    string // login; bot/app logins carry the "[bot]" suffix
	IsBot     bool   // author is a bot/app account
	Body      string
	CreatedAt time.Time
}

// commentsPayload is the slice of `gh pr view --json comments` we consume.
// is_bot and __typename are both parsed defensively: older gh versions
// expose only __typename ("User"|"Bot"), newer ones add is_bot.
type commentsPayload struct {
	Comments []struct {
		Author struct {
			Login    string `json:"login"`
			IsBot    bool   `json:"is_bot"`
			TypeName string `json:"__typename"`
		} `json:"author"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"comments"`
}

// PRComments lists the PR's issue comments, oldest first (GitHub's order).
func (Runner) PRComments(ctx context.Context, pr int) ([]PRComment, error) {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view",
		fmt.Sprint(pr), "--json", "comments").Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d comments: %w", pr, err)
	}
	return parseComments(out)
}

// parseComments maps the comments JSON payload onto []PRComment, flagging
// bot authors from every shape the field has taken (is_bot, __typename,
// "[bot]" login suffix).
func parseComments(out []byte) ([]PRComment, error) {
	var p commentsPayload
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("gh pr view comments: %w", err)
	}
	comments := make([]PRComment, 0, len(p.Comments))
	for _, c := range p.Comments {
		comments = append(comments, PRComment{
			Author:    c.Author.Login,
			IsBot:     c.Author.IsBot || c.Author.TypeName == "Bot" || strings.HasSuffix(c.Author.Login, "[bot]"),
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
