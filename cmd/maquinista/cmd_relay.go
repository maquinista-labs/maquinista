package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/maquinista-labs/maquinista/internal/relay"
	"github.com/spf13/cobra"
)

var relayCmd = &cobra.Command{
	Use:   "relay",
	Short: "Run the outbox relay (fan-out + mention parsing)",
	Long: `Claims rows from agent_outbox, inserts channel_deliveries rows for the
origin topic and all owner/observer bindings, parses [@agent_id: ...] mentions
into new agent_inbox rows, and marks the outbox row as routed. All inside one
transaction so a crash preserves the row's 'pending' state.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			log.Println("relay: shutdown requested")
			cancel()
		}()

		workerID := fmt.Sprintf("relay-%s", uuid.New().String()[:8])
		log.Printf("relay: starting (worker=%s)", workerID)
		return relay.Run(ctx, pool, workerID)
	},
}

func init() {
	rootCmd.AddCommand(relayCmd)
}
