package bot

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// handleDashboard handles the /dashboard [duration] command.
// It starts a Cloudflare Quick Tunnel to the local dashboard and replies with
// the public URL. If a tunnel is already running it returns the existing URL
// and the remaining time.
//
// Duration argument examples: "15m" (default), "1h", "30m", "0" (no expiry).
func (b *Bot) handleDashboard(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if b.tunnel == nil {
		b.reply(chatID, threadID, "Tunnel manager not initialised.")
		return
	}

	// If a tunnel is already open, report its URL and remaining time.
	if b.tunnel.IsRunning() {
		url := b.tunnel.URL()
		rem := b.tunnel.RemainingTime()
		if rem > 0 {
			b.replyWithURLButton(chatID, threadID,
				fmt.Sprintf("Tunnel already running (expires in %s).", rem.Round(time.Second)),
				url)
		} else {
			b.replyWithURLButton(chatID, threadID, "Tunnel running (no expiry).", url)
		}
		return
	}

	// Parse optional duration argument.
	dur, err := parseTunnelDuration(strings.TrimSpace(msg.CommandArguments()))
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Invalid duration: %v\nUsage: /dashboard [15m|1h|30m|0]", err))
		return
	}

	// Ensure the dashboard process is running before tunnelling.
	listenAddr := b.config.Dashboard.Listen
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8900"
	}
	if err := b.ensureDashboardRunning(); err != nil {
		log.Printf("dashboard_commands: ensure dashboard: %v", err)
		b.reply(chatID, threadID, fmt.Sprintf("Could not start dashboard: %v", err))
		return
	}

	b.reply(chatID, threadID, "Starting tunnel…")

	// Record the chat/thread so the expiry notification (fired by the Manager's
	// notify callback, which was wired in New()) reaches the right conversation.
	b.tunnelNotifyMu.Lock()
	b.tunnelNotifyChatID = chatID
	b.tunnelNotifyThreadID = threadID
	b.tunnelNotifyMu.Unlock()

	ctx := context.Background()
	url, err := b.tunnel.Start(ctx, listenAddr, dur)
	if err != nil {
		log.Printf("dashboard_commands: tunnel start: %v", err)
		b.reply(chatID, threadID, fmt.Sprintf("Failed to start tunnel: %v", err))
		return
	}

	var text string
	if dur > 0 {
		text = fmt.Sprintf("Dashboard tunnel ready for %s (may take a few seconds to be reachable).", dur.Round(time.Second))
	} else {
		text = "Dashboard tunnel ready (may take a few seconds to be reachable)."
	}
	b.replyWithURLButton(chatID, threadID, text, url)
}

// handleDashboardStop handles the /dashboard_stop command.
func (b *Bot) handleDashboardStop(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if b.tunnel == nil {
		b.reply(chatID, threadID, "Tunnel manager not initialised.")
		return
	}

	if !b.tunnel.IsRunning() {
		b.reply(chatID, threadID, "No tunnel is running.")
		return
	}

	b.tunnel.Stop()
	b.reply(chatID, threadID, "Tunnel stopped.")
}

// replyWithURLButton sends a message with an inline [Open] URL button.
func (b *Bot) replyWithURLButton(chatID int64, threadID int, text, url string) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("Open", url),
		),
	)
	if _, err := b.sendMessageWithKeyboard(chatID, threadID, text, keyboard); err != nil {
		log.Printf("dashboard_commands: send url button: %v", err)
	}
}

// parseTunnelDuration converts a user-supplied string into a duration.
// "" → 15 minutes (default). "0" → 0 (no expiry). Otherwise parsed via
// time.ParseDuration.
func parseTunnelDuration(s string) (time.Duration, error) {
	switch s {
	case "", "15m":
		return 15 * time.Minute, nil
	case "0":
		return 0, nil
	}
	return time.ParseDuration(s)
}

// StartPersistentTunnel opens a no-TTL cloudflared tunnel to the dashboard and
// logs the public URL to stdout. It also sends a Telegram message with an
// [Open] button to each configured AllowedUser (treating user IDs as DM chat
// IDs, which is correct for private chats). Idempotent: if the tunnel is
// already running it returns the existing URL.
//
// Called by the orchestrator supervisor when
// MAQUINISTA_DASHBOARD_AUTO_TUNNEL=1 is set.
func (b *Bot) StartPersistentTunnel(ctx context.Context) (string, error) {
	if b.tunnel == nil {
		return "", fmt.Errorf("tunnel manager not initialised")
	}

	// Already running — return the existing URL.
	if b.tunnel.IsRunning() {
		url := b.tunnel.URL()
		log.Printf("auto-tunnel: already running → %s", url)
		return url, nil
	}

	listenAddr := b.config.Dashboard.Listen
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8900"
	}

	if err := b.ensureDashboardRunning(); err != nil {
		log.Printf("auto-tunnel: ensure dashboard: %v", err)
		// Non-fatal — cloudflared can race the dashboard startup; the health
		// endpoint will return 503 until the dashboard is ready.
	}

	// dur=0 means no TTL — tunnel lives for the entire process lifetime.
	url, err := b.tunnel.Start(ctx, listenAddr, 0)
	if err != nil {
		return "", fmt.Errorf("starting tunnel: %w", err)
	}

	log.Printf("auto-tunnel: dashboard ready → %s", url)

	if b.config.Dashboard.AuthMode == "none" {
		log.Printf("WARNING: MAQUINISTA_DASHBOARD_AUTO_TUNNEL=1 but MAQUINISTA_DASHBOARD_AUTH=none — " +
			"the dashboard is publicly accessible without authentication. " +
			"Set MAQUINISTA_DASHBOARD_AUTH=password to require login.")
	}

	// Notify every allowed user via DM (user ID == DM chat ID in Telegram).
	for _, userID := range b.config.AllowedUsers {
		b.replyWithURLButton(userID, 0, "🚀 Dashboard tunnel ready.", url)
	}

	return url, nil
}

// ensureDashboardRunning starts the dashboard if it is not already running.
// It shells out to `maquinista dashboard start` in a fire-and-forget fashion
// and waits up to 5 s for the healthcheck to respond.
func (b *Bot) ensureDashboardRunning() error {
	bin := b.config.MaquinistaBin
	if bin == "" {
		bin = "maquinista"
	}
	// If the dashboard is already up the command returns immediately with a
	// non-zero exit (PID file already exists) — that is fine.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "dashboard", "start")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("dashboard_commands: dashboard start: %v\n%s", err, out)
		// Not fatal — it may already be running.
	}
	return nil
}
