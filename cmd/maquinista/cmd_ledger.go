package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/spf13/cobra"
)

var (
	ledgerBy    string
	ledgerUser  string
	ledgerTask  string
	ledgerSince string
	ledgerLimit int
)

var ledgerCmd = &cobra.Command{
	Use:   "ledger",
	Short: "Cost-per-task ledger: token + wall-clock rollups per user/task/session (MAQ-47)",
	Long: `Roll up agent_turn_costs and turn-end events (cost_ledger) by session,
task, or user: turns, tokens, wall-clock seconds, and cost in cents —
both as captured at insert time and re-priced at current model rates.
No billing; this is the ADR-0009 F0 pricing-calibration input.

  maquinista ledger --by task
  maquinista ledger --by user --since 720h
  maquinista ledger --by session --task 121ac07a-...`,
	RunE: func(cmd *cobra.Command, args []string) error {
		by := db.LedgerBy(ledgerBy)
		switch by {
		case db.LedgerBySession, db.LedgerByTask, db.LedgerByUser:
		default:
			return fmt.Errorf("--by must be one of session|task|user (got %q)", ledgerBy)
		}

		since, err := parseLedgerSince(ledgerSince)
		if err != nil {
			return err
		}

		if err := connectDB(); err != nil {
			return err
		}

		var userFilter, taskFilter *string
		if ledgerUser != "" {
			userFilter = &ledgerUser
		}
		if ledgerTask != "" {
			taskFilter = &ledgerTask
		}

		rows, err := db.LedgerRollup(context.Background(), pool, by,
			userFilter, taskFilter, since, ledgerLimit)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Println("No ledger entries.")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tTURNS\tIN TOK\tOUT TOK\tCACHE TOK\tTURN s\tSPAN s\tCAPTURED ¢\tCURRENT ¢\tLAST ACTIVE")
		for _, r := range rows {
			span := "—"
			if r.WallSeconds != nil {
				span = fmt.Sprintf("%.0f", *r.WallSeconds)
			}
			current := "n/a"
			if r.CurrentCents != nil {
				current = fmt.Sprintf("%d", *r.CurrentCents)
			}
			last := "—"
			if r.LastTurnAt != nil {
				last = r.LastTurnAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%.0f\t%s\t%d\t%s\t%s\n",
				r.Key, r.Turns, r.InputTokens, r.OutputTokens,
				r.CacheReadTokens+r.CacheWriteTokens,
				r.TurnSeconds, span, r.CapturedCents, current, last)
		}
		return w.Flush()
	},
}

// parseLedgerSince accepts a Go duration (720h), an N-days shorthand
// (30d), or a date (2006-01-02, optionally with 15:04 time).
func parseLedgerSince(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		t := time.Now().Add(-d)
		return &t, nil
	}
	// N-days shorthand (time.ParseDuration has no day unit).
	var days int
	if _, err := fmt.Sscanf(s, "%dd", &days); err == nil && days > 0 && s == fmt.Sprintf("%dd", days) {
		t := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		return &t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("--since: use a duration (720h, 30d) or a date (2026-10-01), got %q", s)
}

func init() {
	ledgerCmd.Flags().StringVar(&ledgerBy, "by", "task", "rollup grain: session|task|user")
	ledgerCmd.Flags().StringVar(&ledgerUser, "user", "", "filter by user_id")
	ledgerCmd.Flags().StringVar(&ledgerTask, "task", "", "filter by task id")
	ledgerCmd.Flags().StringVar(&ledgerSince, "since", "", "only sessions active since: duration (720h) or date (2026-10-01)")
	ledgerCmd.Flags().IntVar(&ledgerLimit, "limit", 25, "max rows")
	rootCmd.AddCommand(ledgerCmd)
}
