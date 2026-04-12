package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/dispatcher"
	"github.com/spf13/cobra"
)

var dispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Send channel_deliveries rows to Telegram",
	Long: `Claims pending rows from channel_deliveries, calls Telegram's sendMessage,
and flips the row to 'sent' with the returned message_id. 429 responses
reschedule the row (status='pending', next_attempt_at = NOW() + retry_after).
Other errors bump 'attempts'; once attempts exceed the cap the row is marked
'failed'.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		token := os.Getenv("TELEGRAM_BOT_TOKEN")
		if token == "" {
			return fmt.Errorf("TELEGRAM_BOT_TOKEN not set")
		}
		api, err := tgbotapi.NewBotAPI(token)
		if err != nil {
			return fmt.Errorf("new bot api: %w", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			log.Println("dispatcher: shutdown requested")
			cancel()
		}()

		cfg := dispatcher.DefaultConfig()
		// MAILBOX_DISPATCHER=1 → live; default is shadow so channel_deliveries
		// rows are consumed without double-sending alongside the legacy path.
		cfg.Shadow = os.Getenv("MAILBOX_DISPATCHER") != "1"
		if cfg.Shadow {
			log.Println("dispatcher: SHADOW mode (MAILBOX_DISPATCHER!=1)")
		} else {
			log.Println("dispatcher: LIVE mode")
		}
		client := &dispatcher.BotAPIClient{API: api}
		return dispatcher.Run(ctx, pool, client, cfg)
	},
}

func init() {
	rootCmd.AddCommand(dispatchCmd)
}
