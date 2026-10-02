// Package gh implements the GitHub side of gh merge mode via the gh CLI.
// It is the production GhRunner for pipeline.MergeConfig — a thin, ugly-on-
// purpose wrapper around two subcommands, so all branching stays in the
// pipeline package where it is testable.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
