package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/maquinista-labs/maquinista/internal/webhooks"
	"github.com/spf13/cobra"
)

var webhookAddr string

var webhookServeCmd = &cobra.Command{
	Use:   "webhook-serve",
	Short: "HTTP ingress for webhook_handlers",
	Long: `POST /hooks/* requests are looked up in webhook_handlers, HMAC-verified,
their prompt template rendered against the JSON payload, and the result
enqueued as an agent_inbox row with from_kind='webhook' and
external_msg_id='hook:<handler_id>:<delivery_id>' (replay-safe).`,
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
			log.Println("webhook-serve: shutdown")
			cancel()
		}()
		cfg := webhooks.DefaultConfig(webhookAddr)
		log.Printf("webhook-serve: listening on %s", cfg.Addr)
		return webhooks.Run(ctx, pool, cfg)
	},
}

func init() {
	webhookServeCmd.Flags().StringVar(&webhookAddr, "addr", ":8080", "listen address")
	rootCmd.AddCommand(webhookServeCmd)
}
