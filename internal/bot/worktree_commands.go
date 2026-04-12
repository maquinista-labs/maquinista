package bot

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// handlePickwCommand creates a worktree and sends a task prompt to the existing session.
// Supports: /pickw (shows task list), /pickw <full-id>, /pickw <partial-id>
func (b *Bot) handlePickwCommand(msg *tgbotapi.Message) {
	partialID := strings.TrimSpace(msg.CommandArguments())

	task, ok := b.resolveTaskID(msg, partialID, "pickw")
	if !ok {
		return // picker shown or error sent
	}

	b.executePickwTask(msg.Chat.ID, getThreadID(msg), msg.From.ID, task.ID)
}

// executePickwTask runs the /pickw logic for a resolved task ID.
// Unlike the old implementation, this reuses the existing Claude session
// in the current topic instead of creating a new topic/window/process.
// It only adds git isolation via a worktree.
func (b *Bot) executePickwTask(chatID int64, threadID int, userID int64, taskID string) {
	threadIDStr := strconv.Itoa(threadID)
	userIDStr := strconv.FormatInt(userID, 10)

	project, ok := b.state.GetProject(threadIDStr)
	if !ok {
		b.reply(chatID, threadID, "No project bound. Use /p_bind <name> first.")
		return
	}

	repoRoot, err := b.getRepoRoot(userIDStr, threadIDStr)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	baseBranch, err := git.CurrentBranch(repoRoot)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error getting branch: %v", err))
		return
	}

	branch := fmt.Sprintf("minuano/%s-%s", project, taskID)
	worktreeDir := filepath.Join(repoRoot, ".minuano", "worktrees", fmt.Sprintf("%s-%s", project, taskID))

	b.reply(chatID, threadID, fmt.Sprintf("Creating worktree for %s...", taskID))

	if err := git.WorktreeAdd(repoRoot, worktreeDir, branch); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error creating worktree: %v", err))
		return
	}

	// Resolve existing window for this topic (like /pick)
	windowID, bound := b.state.GetWindowForThread(userIDStr, threadIDStr)
	if !bound {
		git.WorktreeRemove(repoRoot, worktreeDir)
		git.DeleteBranch(repoRoot, branch)
		b.reply(chatID, threadID, "Topic not bound to a session.")
		return
	}

	// Store worktree info against current thread
	b.state.SetWorktreeInfo(threadIDStr, state.WorktreeInfo{
		WorktreeDir: worktreeDir,
		Branch:      branch,
		RepoRoot:    repoRoot,
		BaseBranch:  baseBranch,
		TaskID:      taskID,
	})
	b.saveState()

	// Generate task prompt
	prompt, err := b.minuanoBridge.PromptSingle(taskID)
	if err != nil {
		log.Printf("Error generating prompt for %s: %v", taskID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Worktree ready but failed to generate prompt: %v", err))
		return
	}

	// Wrap prompt with worktree instructions
	wrappedPrompt := fmt.Sprintf(
		"IMPORTANT: Work in the git worktree at %s (branch: %s). "+
			"cd to that directory before doing anything. "+
			"Make all changes and commits there, NOT in the main repo.\n\n%s",
		worktreeDir, branch, prompt,
	)

	if err := b.sendPromptToTmux(windowID, wrappedPrompt); err != nil {
		log.Printf("Error sending prompt to tmux: %v", err)
		b.reply(chatID, threadID, "Error: failed to send prompt.")
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Working on task %s in worktree (branch: %s)", taskID, branch))
}

// getRepoRoot returns the git repo root for the current window's CWD.
// If the CWD itself is not a git repo, it tries CWD/<project> as a fallback.
func (b *Bot) getRepoRoot(userIDStr, threadIDStr string) (string, error) {
	windowID, bound := b.state.GetWindowForThread(userIDStr, threadIDStr)
	if !bound {
		return "", fmt.Errorf("topic not bound to a session")
	}
	ws, ok := b.state.GetWindowState(windowID)
	if !ok || ws.CWD == "" {
		return "", fmt.Errorf("no CWD known for current session")
	}

	// Try CWD directly
	root, err := git.RepoRoot(ws.CWD)
	if err == nil {
		return root, nil
	}

	// Fallback: try CWD/<project> (e.g. /home/user/code/terminal-game)
	if project, ok := b.state.GetProject(threadIDStr); ok {
		projectDir := filepath.Join(ws.CWD, project)
		if root, err := git.RepoRoot(projectDir); err == nil {
			return root, nil
		}
	}

	return "", fmt.Errorf("git rev-parse --show-toplevel in %s: not a git repository", ws.CWD)
}

