package bot

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/maquinista-labs/maquinista/internal/monitor"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

const (
	bashCaptureMaxPolls  = 30
	bashCapturePollDelay = 1 * time.Second
	bashCaptureInitDelay = 2 * time.Second
	bashCaptureMaxChars  = 3800
)

// bashCapture tracks an active bash capture goroutine.
type bashCapture struct {
	cancel context.CancelFunc
}

var (
	bashCaptures   = make(map[string]*bashCapture) // "userID:threadID" → capture
	bashCapturesMu sync.Mutex
)

func bashCaptureKey(userID int64, threadID int) string {
	return fmt.Sprintf("%d:%d", userID, threadID)
}

// cancelBashCapture cancels any running bash capture for the given topic.
func cancelBashCapture(userID int64, threadID int) {
	bashCapturesMu.Lock()
	defer bashCapturesMu.Unlock()
	key := bashCaptureKey(userID, threadID)
	if cap, ok := bashCaptures[key]; ok {
		cap.cancel()
		delete(bashCaptures, key)
	}
}

// startBashCapture launches a goroutine that polls the tmux pane for bash command output.
func (b *Bot) startBashCapture(userID int64, chatID int64, threadID int, windowID, command string) {
	// Cancel any existing capture for this topic
	cancelBashCapture(userID, threadID)

	ctx, cancel := context.WithCancel(context.Background())
	key := bashCaptureKey(userID, threadID)

	bashCapturesMu.Lock()
	bashCaptures[key] = &bashCapture{cancel: cancel}
	bashCapturesMu.Unlock()

	go b.captureBashOutput(ctx, userID, chatID, threadID, windowID, command)
}

// captureBashOutput polls the tmux pane for command output and sends/edits messages.
func (b *Bot) captureBashOutput(ctx context.Context, userID int64, chatID int64, threadID int, windowID, command string) {
	defer func() {
		bashCapturesMu.Lock()
		delete(bashCaptures, bashCaptureKey(userID, threadID))
		bashCapturesMu.Unlock()
	}()

	// Wait for command to start producing output
	select {
	case <-ctx.Done():
		return
	case <-time.After(bashCaptureInitDelay):
	}

	var messageID int
	var lastOutput string

	for i := 0; i < bashCaptureMaxPolls; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		paneText, err := tmux.CapturePane(b.config.TmuxSessionName, windowID, false)
		if err != nil {
			if tmux.IsWindowDead(err) {
				log.Printf("Bash capture: window %s is dead, stopping capture", windowID)
			} else {
				log.Printf("Bash capture: error capturing pane: %v", err)
			}
			return
		}

		output := monitor.ExtractBashOutput(paneText, command)
		if output == "" {
			select {
			case <-ctx.Done():
				return
			case <-time.After(bashCapturePollDelay):
			}
			continue
		}

		// Skip if unchanged
		if output == lastOutput {
			select {
			case <-ctx.Done():
				return
			case <-time.After(bashCapturePollDelay):
			}
			continue
		}

		lastOutput = output

		// Truncate if too long
		displayOutput := output
		if len(displayOutput) > bashCaptureMaxChars {
			displayOutput = "... " + displayOutput[len(displayOutput)-bashCaptureMaxChars:]
		}

		if messageID == 0 {
			// First output: send new message
			msg, err := b.sendMessageInThread(chatID, threadID, displayOutput)
			if err != nil {
				log.Printf("Bash capture: error sending message: %v", err)
				return
			}
			messageID = msg.MessageID
		} else {
			// Subsequent: edit in place
			if err := b.editMessageText(chatID, messageID, displayOutput); err != nil {
				log.Printf("Bash capture: error editing message: %v", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(bashCapturePollDelay):
		}
	}
}
