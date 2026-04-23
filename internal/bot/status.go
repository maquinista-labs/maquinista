package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/monitor"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// statusKey is a composite key for per-(user, thread) status tracking.
type statusKey struct {
	UserID   int64
	ThreadID int
}

// animFrames are the cycling emoji markers prepended to status messages.
var animFrames = []string{"☕", "⏳", "✨", "🔮"}

// StatusPoller polls Claude's terminal for status line changes and sends updates.
type StatusPoller struct {
	bot                *Bot
	monitor            *monitor.Monitor
	pool               *pgxpool.Pool
	mu                 sync.RWMutex
	lastStatus         map[statusKey]string // last status text per user+thread
	lastNotifiedStatus map[string]string    // windowID → last pg_notify'd status
	missCount          map[string]int       // windowID → consecutive miss count
	animFrame          map[statusKey]int    // animation frame per user+thread
	pollInterval       time.Duration
}

// missThreshold is how many consecutive polls must miss the status
// before we consider it truly cleared (prevents flicker from unreliable detection).
const missThreshold = 3

// NewStatusPoller creates a new StatusPoller.
func NewStatusPoller(bot *Bot, mon *monitor.Monitor, pool *pgxpool.Pool) *StatusPoller {
	return &StatusPoller{
		bot:                bot,
		monitor:            mon,
		pool:               pool,
		lastStatus:         make(map[statusKey]string),
		lastNotifiedStatus: make(map[string]string),
		missCount:          make(map[string]int),
		animFrame:          make(map[statusKey]int),
		pollInterval:       1 * time.Second,
	}
}

// Run starts the status polling loop. Blocks until ctx is cancelled.
func (sp *StatusPoller) Run(ctx context.Context) {
	log.Println("Status poller starting...")
	ticker := time.NewTicker(sp.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Status poller stopped.")
			return
		case <-ticker.C:
			sp.poll()
		}
	}
}

// allActiveWindows returns the union of Telegram-bound windows and all DB
// agents with an active tmux_window (so dashboard-only agents are included).
func (sp *StatusPoller) allActiveWindows() map[string]bool {
	windows := sp.bot.state.AllBoundWindowIDs()
	if sp.pool == nil {
		return windows
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := sp.pool.Query(ctx, `
		SELECT tmux_window FROM agents
		WHERE tmux_window <> '' AND status NOT IN ('archived','stopped')
	`)
	if err != nil {
		log.Printf("status poller: allActiveWindows query error: %v", err)
		return windows
	}
	defer rows.Close()
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err == nil && w != "" {
			windows[w] = true
		}
	}
	return windows
}

func (sp *StatusPoller) poll() {
	windows := sp.allActiveWindows()
	log.Printf("status poller: polling %d windows", len(windows))

	for windowID := range windows {
		users := sp.bot.state.FindUsersForWindow(windowID)

		// Capture pane (plain text, no ANSI)
		paneText, err := tmux.CapturePane(sp.bot.config.TmuxSessionName, windowID, false)
		if err != nil {
			if tmux.IsWindowDead(err) {
				log.Printf("Status poller: window %s is dead, cleaning up", windowID)
				// Save chat IDs before cleanup removes them
				type notifyTarget struct {
					chatID   int64
					threadID int
				}
				var targets []notifyTarget
				for _, ut := range users {
					if cid, ok := sp.bot.state.GetGroupChatID(ut.UserID, ut.ThreadID); ok {
						tid, _ := strconv.Atoi(ut.ThreadID)
						targets = append(targets, notifyTarget{cid, tid})
					}
				}
				// Clean up UI states for all users on this window
				for _, ut := range users {
					uid, _ := strconv.ParseInt(ut.UserID, 10, 64)
					tid, _ := strconv.Atoi(ut.ThreadID)
					cancelBashCapture(uid, tid)
					clearInteractiveUI(uid, tid)
					// Clear cached status
					sp.mu.Lock()
					delete(sp.lastStatus, statusKey{uid, tid})
					sp.mu.Unlock()
				}
				cleanupDeadWindow(sp.bot, windowID)
				for _, t := range targets {
					sp.bot.reply(t.chatID, t.threadID, "Session died. Send a message to restart.")
				}
			}
			continue
		}

		// Look up TranscriptSource for this window's runner
		runnerName := sp.bot.state.GetWindowRunner(windowID)
		src, srcErr := monitor.GetSource(runnerName)

		// Check interactive UI once per pane
		var isInteractive bool
		if srcErr == nil {
			isInteractive = src.IsInteractiveUI(paneText)
		} else {
			isInteractive = monitor.IsInteractiveUI(paneText)
		}

		// Extract status line (only if not interactive)
		var statusText string
		var hasStatus bool
		if !isInteractive {
			if srcErr == nil {
				statusText, hasStatus = src.ExtractStatusLine(paneText)
			} else {
				statusText, hasStatus = monitor.ExtractStatusLine(paneText)
			}

			if hasStatus {
				sp.mu.Lock()
				sp.missCount[windowID] = 0
				sp.mu.Unlock()
				log.Printf("status poller: window=%s hasStatus=true text=%q", windowID, statusText)
			} else {
				sp.mu.Lock()
				sp.missCount[windowID]++
				sp.mu.Unlock()
			}
		} else {
			log.Printf("status poller: window=%s isInteractive=true — skipping status extract", windowID)
		}

		// Forward status to dashboard via pg_notify (deduped per window).
		sp.notifyDashboardStatus(windowID, statusText, hasStatus)

		// Update for each observing user
		for _, ut := range users {
			userID, _ := strconv.ParseInt(ut.UserID, 10, 64)
			threadID, _ := strconv.Atoi(ut.ThreadID)
			chatID, ok := sp.bot.state.GetGroupChatID(ut.UserID, ut.ThreadID)
			if !ok {
				continue
			}

			// Interactive UI detection per user
			interactiveWin, inMode := getInteractiveWindow(userID, threadID)
			shouldCheckNew := true

			if inMode && interactiveWin == windowID {
				if isInteractive {
					continue // UI still showing, skip
				}
				// UI gone — clear, don't re-check this cycle
				clearInteractiveUI(userID, threadID)
				shouldCheckNew = false
			} else if inMode {
				// Interactive mode for a different window — stale, clear it
				clearInteractiveUI(userID, threadID)
			}

			if shouldCheckNew && isInteractive {
				sp.bot.handleInteractiveUI(chatID, threadID, userID, windowID)
				continue
			}

			// Status line handling
			key := statusKey{userID, threadID}

			sp.mu.RLock()
			lastText := sp.lastStatus[key]
			misses := sp.missCount[windowID]
			sp.mu.RUnlock()

			if hasStatus {
				// Deduplicate: skip if same text
				if statusText == lastText {
					continue
				}

				sp.mu.Lock()
				sp.lastStatus[key] = statusText
				frame := sp.animFrame[key]
				sp.animFrame[key] = (frame + 1) % len(animFrames)
				sp.mu.Unlock()

				displayText := animFrames[frame] + " " + statusText
				sp.bot.reply(chatID, threadID, displayText)
			} else if lastText != "" && misses >= missThreshold {
				// Status cleared — only after consecutive misses to avoid flicker
				sp.mu.Lock()
				delete(sp.lastStatus, key)
				delete(sp.animFrame, key)
				sp.mu.Unlock()

				if sp.monitor != nil {
					if start, ok := sp.monitor.GetAndClearTurnStart(windowID); ok {
						sp.bot.reply(chatID, threadID, formatDuration(time.Since(start)))
					}
				}
			}
		}
	}
}

// notifyDashboardStatus pg_notifies the agent_status channel when the window's
// status changes. effectiveText is "" when the status clears.
func (sp *StatusPoller) notifyDashboardStatus(windowID, statusText string, hasStatus bool) {
	if sp.pool == nil {
		log.Printf("status poller: notifyDashboard window=%s skipped: no pool", windowID)
		return
	}

	sp.mu.RLock()
	misses := sp.missCount[windowID]
	last := sp.lastNotifiedStatus[windowID]
	sp.mu.RUnlock()

	var effective string
	if hasStatus {
		effective = statusText
	} else if misses < missThreshold {
		return // not yet confident the status is gone — don't notify
	}
	// effective == "" means status cleared

	if effective == last {
		return // no change
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	agentID, err := monitor.ResolveAgentFromWindow(ctx, sp.pool, windowID)
	if err != nil {
		log.Printf("status poller: notifyDashboard window=%s resolve error: %v", windowID, err)
		return
	}
	if agentID == "" {
		log.Printf("status poller: notifyDashboard window=%s no agent found", windowID)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"agent_id": agentID,
		"text":     effective,
	})
	if _, err := sp.pool.Exec(ctx, "SELECT pg_notify($1, $2)", "agent_status", string(payload)); err != nil {
		log.Printf("status poller: pg_notify agent_status: %v", err)
		return
	}

	log.Printf("status poller: notified dashboard agent=%s text=%q", agentID, effective)
	sp.mu.Lock()
	sp.lastNotifiedStatus[windowID] = effective
	sp.mu.Unlock()
}

// formatDuration formats a duration as "Brewed for Xm Ys" or "Brewed for Ys".
func formatDuration(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("Brewed for %ds", secs)
	}
	mins := secs / 60
	secs = secs % 60
	return fmt.Sprintf("Brewed for %dm %ds", mins, secs)
}
