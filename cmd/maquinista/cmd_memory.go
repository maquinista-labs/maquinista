package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
	"github.com/maquinista-labs/maquinista/internal/memory"
	"github.com/spf13/cobra"
)

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Manage agent archival memory (passages)",
}

var (
	memRememberTier     string
	memRememberCategory string
	memRememberTitle    string
	memRememberBody     string
	memRememberPin      bool
	memListTier         string
	memListCategory     string
	memListLimit        int
	memSearchLimit      int
)

var memRememberCmd = &cobra.Command{
	Use:   "remember <agent-id>",
	Short: "Insert an archival passage",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		id, err := memory.Remember(ctx, pool, memory.Memory{
			AgentID:   args[0],
			Dimension: "agent",
			Tier:      memRememberTier,
			Category:  memRememberCategory,
			Title:     memRememberTitle,
			Body:      memRememberBody,
			Source:    "operator",
			Pinned:    memRememberPin,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Inserted memory %d\n", id)
		return nil
	},
}

var memListCmd = &cobra.Command{
	Use:   "list <agent-id>",
	Short: "List archival passages",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := memory.List(ctx, pool, args[0], memory.ListFilter{
			Tier:     memListTier,
			Category: memListCategory,
			Limit:    memListLimit,
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	},
}

var memSearchCmd = &cobra.Command{
	Use:   "search <agent-id> <query>",
	Short: "Full-text search over archival passages",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := memory.Search(ctx, pool, args[0], args[1], memory.ListFilter{
			Limit: memSearchLimit,
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	},
}

var memShowCmd = &cobra.Command{
	Use:   "show <agent-id> <id>",
	Short: "Show one archival passage",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var id int64
		if _, serr := fmt.Sscanf(args[1], "%d", &id); serr != nil {
			return fmt.Errorf("id must be an integer: %w", serr)
		}
		m, err := memory.Get(ctx, pool, args[0], id)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(m)
	},
}

var memForgetCmd = &cobra.Command{
	Use:   "forget <agent-id> <id>",
	Short: "Delete an archival passage",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var id int64
		if _, serr := fmt.Sscanf(args[1], "%d", &id); serr != nil {
			return fmt.Errorf("id must be an integer: %w", serr)
		}
		return memory.Forget(ctx, pool, args[0], id)
	},
}

var memPinCmd = &cobra.Command{
	Use:   "pin <agent-id> <id>",
	Short: "Pin an archival passage (always injected at spawn)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return memSetPin(args[0], args[1], true)
	},
}

var memUnpinCmd = &cobra.Command{
	Use:   "unpin <agent-id> <id>",
	Short: "Unpin an archival passage",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return memSetPin(args[0], args[1], false)
	},
}

func memSetPin(agentID, rawID string, pinned bool) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id int64
	if _, serr := fmt.Sscanf(rawID, "%d", &id); serr != nil {
		return fmt.Errorf("id must be an integer: %w", serr)
	}
	return memory.Pin(ctx, pool, agentID, id, pinned)
}

// memAutoflushCmd enqueues a one-shot memory housekeeping turn for an agent.
// The scheduler fires this automatically via a per-agent scheduled_jobs row
// (registered at agent-add time). This command lets the operator trigger it
// manually or test the flow without waiting for the cron.
var memAutoflushCmd = &cobra.Command{
	Use:   "autoflush <agent-id>",
	Short: "Trigger a one-shot memory housekeeping turn for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMemoryAutoflush(args[0])
	},
}

func runMemoryAutoflush(agentID string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return enqueueAutoflush(ctx, pool, agentID)
}

// enqueueAutoflush injects the periodic memory housekeeping prompt into an
// agent's inbox. Called by the manual `memory autoflush` command and can be
// called programmatically if needed. The external_msg_id uses a
// minute-resolution timestamp so rapid double-fires collapse to one row.
func enqueueAutoflush(ctx context.Context, pool *pgxpool.Pool, agentID string) error {
	prompt := "Routine memory housekeeping: review our recent conversation and use memory_remember to save any durable facts, preferences, or lessons worth keeping long-term. Keep entries ≤1 paragraph; categorize as feedback/project/reference/fact/preference. If nothing is worth keeping, acknowledge briefly."
	content, _ := json.Marshal(map[string]string{"type": "text", "text": prompt})
	extMsgID := fmt.Sprintf("autoflush:%s:%s", agentID, time.Now().UTC().Format("2006-01-02T15:04"))

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	_, inserted, err := mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:        agentID,
		FromKind:       "system",
		FromID:         "autoflush",
		OriginChannel:  "a2a:autoflush",
		ExternalMsgID:  extMsgID,
		Content:        content,
	})
	if err != nil {
		return fmt.Errorf("enqueue autoflush: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if inserted {
		fmt.Printf("Autoflush enqueued for %s\n", agentID)
	} else {
		fmt.Printf("Autoflush already pending for %s (deduplicated)\n", agentID)
	}
	return nil
}

// memBlocksCmd shows the core memory blocks for an agent.
var memBlocksCmd = &cobra.Command{
	Use:   "blocks <agent-id>",
	Short: "Show all core memory blocks for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		blocks, err := memory.LoadAllBlocks(ctx, pool, args[0])
		if err != nil {
			return err
		}
		for _, b := range blocks {
			fmt.Printf("--- %s (limit=%d, v=%d) ---\n", b.Label, b.CharLimit, b.Version)
			if b.Value != "" {
				fmt.Println(strings.TrimRight(b.Value, "\n"))
			} else {
				fmt.Println("(empty)")
			}
			fmt.Println()
		}
		return nil
	},
}

// memRefreshCmd is the Phase 2 tool from resume-memory-refresh.md.
// The agent calls `maquinista memory refresh $AGENT_ID` mid-session when its
// context feels stale. It prints the catch-up diff since agents.started_at
// (same payload as the resume inject) and then advances started_at so the
// window slides forward. If nothing changed it exits silently with code 0.
var memRefreshCmd = &cobra.Command{
	Use:   "refresh <agent-id>",
	Short: "Print memory/soul deltas since last spawn (agent calls this mid-session)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentID := args[0]
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var startedAt time.Time
		if err := pool.QueryRow(ctx, `SELECT started_at FROM agents WHERE id=$1`, agentID).Scan(&startedAt); err != nil {
			return fmt.Errorf("agent not found: %w", err)
		}

		payload, err := memory.BuildCatchup(ctx, pool, agentID, startedAt)
		if err != nil {
			return fmt.Errorf("build catchup: %w", err)
		}
		if !payload.HasDelta {
			fmt.Println("No updates since last spawn.")
			return nil
		}

		fmt.Println(payload.Text)

		// Advance started_at so repeated refresh calls don't re-report the
		// same deltas.
		_, err = pool.Exec(ctx, `UPDATE agents SET started_at=NOW() WHERE id=$1`, agentID)
		return err
	},
}

func init() {
	memRememberCmd.Flags().StringVar(&memRememberTier, "tier", "long_term", "tier: long_term | daily | signal")
	memRememberCmd.Flags().StringVar(&memRememberCategory, "category", "fact", "category: feedback | project | reference | fact | preference | other")
	memRememberCmd.Flags().StringVar(&memRememberTitle, "title", "", "short title (≤120 chars)")
	memRememberCmd.Flags().StringVar(&memRememberBody, "body", "", "passage body (Markdown)")
	memRememberCmd.Flags().BoolVar(&memRememberPin, "pin", false, "pin this passage (always injected at spawn)")
	_ = memRememberCmd.MarkFlagRequired("title")
	_ = memRememberCmd.MarkFlagRequired("body")

	memListCmd.Flags().StringVar(&memListTier, "tier", "", "filter by tier")
	memListCmd.Flags().StringVar(&memListCategory, "category", "", "filter by category")
	memListCmd.Flags().IntVar(&memListLimit, "limit", 50, "max rows")

	memSearchCmd.Flags().IntVar(&memSearchLimit, "limit", 20, "max rows")

	memoryCmd.AddCommand(
		memRememberCmd,
		memListCmd,
		memSearchCmd,
		memShowCmd,
		memForgetCmd,
		memPinCmd,
		memUnpinCmd,
		memBlocksCmd,
		memAutoflushCmd,
		memRefreshCmd,
	)
	rootCmd.AddCommand(memoryCmd)
}
