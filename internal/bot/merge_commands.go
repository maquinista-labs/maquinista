package bot

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// handleMergeCommand attempts a squash merge; on conflict, spawns a Claude topic.
func (b *Bot) handleMergeCommand(msg *tgbotapi.Message) {
	threadIDStr := strconv.Itoa(getThreadID(msg))

	branch := strings.TrimSpace(msg.CommandArguments())

	// Auto-detect branch from worktree binding
	if branch == "" {
		if wi, ok := b.state.GetWorktreeInfo(threadIDStr); ok && wi.Branch != "" {
			branch = wi.Branch
		}
	}

	// Still no branch — try to show unmerged branches as picker
	if branch == "" {
		b.showMergeBranchPicker(msg)
		return
	}

	b.executeMerge(msg, branch)
}

// executeMergeWithBranch is the pending-input entry point for merge.
// Supports partial regex matching against unmerged branches.
func (b *Bot) executeMergeWithBranch(msg *tgbotapi.Message, text string) {
	input := strings.TrimSpace(text)
	if input == "" {
		b.reply(msg.Chat.ID, getThreadID(msg), "Empty branch name.")
		return
	}

	// Try to resolve against unmerged branches
	branch, ok := b.resolveBranchName(msg, input)
	if !ok {
		return // picker shown or error sent
	}
	b.executeMerge(msg, branch)
}

// resolveBranchName resolves a partial input against unmerged branches.
// Returns the branch name and true if exactly one match, or shows picker/error.
func (b *Bot) resolveBranchName(msg *tgbotapi.Message, input string) (string, bool) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	repoRoot, err := b.getMergeRepoRoot(msg)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return "", false
	}

	baseBranch, err := git.CurrentBranch(repoRoot)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return "", false
	}

	branches, err := git.ListUnmergedBranches(repoRoot, baseBranch)
	if err != nil {
		// Can't list branches — treat input as literal
		return input, true
	}

	// Exact match first
	for _, br := range branches {
		if br == input {
			return br, true
		}
	}

	// Partial regex match
	re, err := regexp.Compile("(?i)" + input)
	if err != nil {
		// Invalid regex — treat as literal prefix
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(input))
	}

	var matches []string
	for _, br := range branches {
		if re.MatchString(br) {
			matches = append(matches, br)
		}
	}

	switch len(matches) {
	case 0:
		b.reply(chatID, threadID, fmt.Sprintf("No unmerged branch matching '%s'.", input))
		return "", false
	case 1:
		return matches[0], true
	default:
		b.showBranchPickerFromList(msg, matches)
		return "", false
	}
}

// showMergeBranchPicker lists unmerged branches as an inline keyboard.
func (b *Bot) showMergeBranchPicker(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	repoRoot, err := b.getMergeRepoRoot(msg)
	if err != nil {
		b.reply(chatID, threadID, "Send the branch name:")
		b.setPendingInput(msg.From.ID, "t_merge", chatID, threadID)
		return
	}

	baseBranch, err := git.CurrentBranch(repoRoot)
	if err != nil {
		b.reply(chatID, threadID, "Send the branch name:")
		b.setPendingInput(msg.From.ID, "t_merge", chatID, threadID)
		return
	}

	branches, err := git.ListUnmergedBranches(repoRoot, baseBranch)
	if err != nil || len(branches) == 0 {
		b.reply(chatID, threadID, "No unmerged branches found. Send a branch name:")
		b.setPendingInput(msg.From.ID, "t_merge", chatID, threadID)
		return
	}

	b.showBranchPickerFromList(msg, branches)
}

// showBranchPickerFromList displays branches as an inline keyboard for selection.
func (b *Bot) showBranchPickerFromList(msg *tgbotapi.Message, branches []string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, br := range branches {
		label := br
		if len(label) > 45 {
			label = label[:42] + "..."
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, "merge_br:"+br),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("Cancel", "merge_cancel"),
	))

	kb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	b.sendMessageWithKeyboard(chatID, threadID, "Select branch to merge:", kb)
}

// handleMergeCallback handles branch picker button presses.
func (b *Bot) handleMergeCallback(cq *tgbotapi.CallbackQuery) {
	data := cq.Data

	if data == "merge_cancel" {
		b.editMessageText(cq.Message.Chat.ID, cq.Message.MessageID, "Merge cancelled.")
		return
	}

	if strings.HasPrefix(data, "merge_br:") {
		branch := data[len("merge_br:"):]
		msg := syntheticMessage(cq)
		b.executeMerge(msg, branch)
	}
}

// getMergeRepoRoot resolves the repo root for the current topic.
func (b *Bot) getMergeRepoRoot(msg *tgbotapi.Message) (string, error) {
	userIDStr := strconv.FormatInt(msg.From.ID, 10)
	threadIDStr := strconv.Itoa(getThreadID(msg))
	return b.getRepoRoot(userIDStr, threadIDStr)
}

// executeMerge performs the squash merge operation.
func (b *Bot) executeMerge(msg *tgbotapi.Message, branch string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	repoRoot, err := b.getMergeRepoRoot(msg)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	// Get current branch as merge target
	baseBranch, err := git.CurrentBranch(repoRoot)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error getting current branch: %v", err))
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Squash-merging %s into %s...", branch, baseBranch))

	// Phase 1: try squash merge
	commitMsg := fmt.Sprintf("%s\n\nSquash-merged from branch %s", branchTitle(branch), branch)
	sha, err := git.MergeSquash(repoRoot, branch, baseBranch, commitMsg)
	if err == nil {
		shortSHA := sha
		if len(sha) > 8 {
			shortSHA = sha[:8]
		}
		b.reply(chatID, threadID, fmt.Sprintf("Merged %s into %s (%s)", branch, baseBranch, shortSHA))

		// Clean up worktree if this branch has one
		b.cleanupWorktreeForBranch(branch)
		return
	}

	// Check if it's a conflict error
	conflictErr, isConflict := err.(*git.ConflictError)
	if !isConflict {
		b.reply(chatID, threadID, fmt.Sprintf("Merge failed: %v", err))
		return
	}

	// Phase 2: conflict — reset and spawn Claude
	// Squash merge doesn't create MERGE_HEAD, so use reset --hard
	if resetErr := git.ResetHard(repoRoot); resetErr != nil {
		log.Printf("Error resetting after conflict in %s: %v", repoRoot, resetErr)
	}

	b.reply(chatID, threadID, fmt.Sprintf("Conflict in %d files. Creating merge topic...", len(conflictErr.Files)))

	// Create merge topic
	topicName := fmt.Sprintf("Merge: %s", branch)
	newThreadID, err := b.createForumTopic(chatID, topicName)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error creating merge topic: %v", err))
		return
	}

	// Create tmux window in repo root
	result, err := b.createWindowForDir(repoRoot, msg.From.ID, chatID, newThreadID)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error creating merge session: %v", err))
		return
	}

	// Store merge topic info in state
	newThreadIDStr := strconv.Itoa(newThreadID)
	b.state.SetWorktreeInfo(newThreadIDStr, state.WorktreeInfo{
		RepoRoot:     repoRoot,
		Branch:       branch,
		BaseBranch:   baseBranch,
		IsMergeTopic: true,
	})
	b.saveState()

	// Build conflict resolution prompt — use squash merge in the instructions too
	conflictList := strings.Join(conflictErr.Files, "\n  - ")
	prompt := fmt.Sprintf(`Merge branch %s into %s.

1. Run: git merge --squash %s
2. Resolve the conflicts in these files:
  - %s
3. Read both sides of each conflict and understand the intent of each change.
4. Resolve intelligently — don't just pick one side.
5. Run the test suite to verify: go build ./...
6. If tests pass, commit the squash merge. If not, fix and re-test.
7. When done, say "Merge complete" so I know you're finished.`,
		branch, baseBranch, branch, conflictList)

	// Wait for Claude to start, then send prompt
	time.Sleep(2 * time.Second)
	if err := b.sendPromptToTmux(result.WindowID, prompt); err != nil {
		log.Printf("Error sending merge prompt: %v", err)
		b.reply(chatID, newThreadID, "Session ready but failed to send merge prompt.")
	}

	b.reply(chatID, threadID, "Merge topic created. Claude is resolving conflicts.")
}

// branchTitle extracts a human-readable title from a branch name.
// "minuano/tramuntana-fix-bug-123" → "tramuntana-fix-bug-123"
func branchTitle(branch string) string {
	if idx := strings.LastIndex(branch, "/"); idx >= 0 {
		return branch[idx+1:]
	}
	return branch
}

// cleanupWorktreeForBranch removes the worktree and branch for a given branch name.
// Called after a successful merge to clean up.
func (b *Bot) cleanupWorktreeForBranch(branch string) {
	for _, threadID := range b.state.AllWorktreeThreadIDs() {
		wi, ok := b.state.GetWorktreeInfo(threadID)
		if !ok || wi.Branch != branch {
			continue
		}
		if wi.WorktreeDir != "" {
			if err := git.WorktreeRemove(wi.RepoRoot, wi.WorktreeDir); err != nil {
				log.Printf("Error removing worktree %s: %v", wi.WorktreeDir, err)
			}
		}
		if err := git.DeleteBranch(wi.RepoRoot, wi.Branch); err != nil {
			log.Printf("Error deleting branch %s: %v", wi.Branch, err)
		}
		b.state.RemoveWorktreeInfo(threadID)
		b.saveState()
		log.Printf("Cleaned up worktree for branch %s (thread %s)", branch, threadID)
		break
	}
}
