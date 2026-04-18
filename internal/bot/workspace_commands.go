package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/git"
)

// handleWsCommand is the top-level dispatcher for /ws subcommands.
// Usage:
//
//	/ws                   — list workspaces for the current topic's agent
//	/ws new <label>       — create workspace (scope=agent, repo from cwd)
//	/ws new <label> --scope task --repo /path
//	/ws switch <label>    — activate an existing workspace
//	/ws archive <label>   — soft-delete a workspace
func (b *Bot) handleWsCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use /ws.")
		return
	}

	// Resolve the agent bound to this topic.
	agentID, err := b.resolveTopicAgentID(msg, pool)
	if err != nil {
		b.reply(chatID, threadID, err.Error())
		return
	}

	args := strings.Fields(strings.TrimSpace(msg.CommandArguments()))
	if len(args) == 0 {
		b.wsList(msg, pool, agentID)
		return
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "list", "ls":
		b.wsList(msg, pool, agentID)
	case "new":
		b.wsNew(msg, pool, agentID, rest)
	case "switch", "use":
		b.wsSwitch(msg, pool, agentID, rest)
	case "archive", "rm":
		b.wsArchive(msg, pool, agentID, rest)
	default:
		b.reply(chatID, threadID, "Unknown /ws subcommand: "+sub+"\nUsage: /ws [list|new <label>|switch <label>|archive <label>]")
	}
}

// resolveTopicAgentID finds the agent ID for the current topic via
// the window binding → agents.tmux_window lookup (same pattern as
// /agent_rename).
func (b *Bot) resolveTopicAgentID(msg *tgbotapi.Message, pool *pgxpool.Pool) (string, error) {
	userID := fmt.Sprintf("%d", msg.From.ID)
	tid := fmt.Sprintf("%d", getThreadID(msg))

	windowID, bound := b.state.GetWindowForThread(userID, tid)
	if !bound {
		return "", fmt.Errorf("No agent bound to this topic. Send a message first to spawn one.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var agentID string
	if err := pool.QueryRow(ctx, `SELECT id FROM agents WHERE tmux_window=$1 LIMIT 1`, windowID).Scan(&agentID); err != nil {
		return "", fmt.Errorf("Could not resolve agent for this topic: %v", err)
	}
	return agentID, nil
}

// wsList shows the workspaces for an agent with the active one starred.
func (b *Bot) wsList(msg *tgbotapi.Message, pool *pgxpool.Pool, agentID string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var active *string
	if err := pool.QueryRow(ctx, `SELECT active_workspace_id FROM agents WHERE id=$1`, agentID).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			b.reply(chatID, threadID, "No such agent: "+agentID)
			return
		}
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	rows, err := pool.Query(ctx, `
		SELECT id, scope, repo_root, COALESCE(worktree_dir,''), COALESCE(branch,'')
		FROM agent_workspaces
		WHERE agent_id=$1 AND archived_at IS NULL
		ORDER BY created_at ASC
	`, agentID)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	defer rows.Close()

	var lines []string
	lines = append(lines, fmt.Sprintf("Workspaces for %s:", agentID))
	for rows.Next() {
		var id, scope, repo, worktree, branch string
		if err := rows.Scan(&id, &scope, &repo, &worktree, &branch); err != nil {
			b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
			return
		}
		marker := "  "
		if active != nil && *active == id {
			marker = "★ "
		}
		label := id[strings.Index(id, "@")+1:]
		lines = append(lines, fmt.Sprintf("%s%s  [%s]  %s", marker, label, scope, shortRepo(repo)))
	}
	if err := rows.Err(); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(lines) == 1 {
		b.reply(chatID, threadID, "No workspaces. Use /ws new <label> to create one.")
		return
	}
	b.reply(chatID, threadID, strings.Join(lines, "\n"))
}

// wsNew creates a new workspace and activates it.
// Format: /ws new <label> [--scope agent|task|shared] [--repo /path]
func (b *Bot) wsNew(msg *tgbotapi.Message, pool *pgxpool.Pool, agentID string, args []string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if len(args) == 0 {
		b.reply(chatID, threadID, "Usage: /ws new <label> [--scope agent|task|shared] [--repo /path]")
		return
	}

	label := args[0]
	scope := "agent" // default
	repo := ""

	// Parse optional flags.
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--scope":
			i++
			if i >= len(args) {
				b.reply(chatID, threadID, "--scope requires a value (shared|agent|task)")
				return
			}
			scope = args[i]
		case "--repo":
			i++
			if i >= len(args) {
				b.reply(chatID, threadID, "--repo requires a path")
				return
			}
			repo = args[i]
		}
	}

	// Validate scope.
	s := agent.WorkspaceScope(scope)
	if err := s.Validate(); err != nil {
		b.reply(chatID, threadID, err.Error())
		return
	}

	// Resolve repo root.
	if s != agent.ScopeShared {
		if repo == "" {
			if r, err := b.deriveRepoRoot(msg); err == nil {
				repo = r
			}
		}
		if repo == "" {
			b.reply(chatID, threadID, fmt.Sprintf("--scope=%s requires --repo (or a bound session with a git repo)", s))
			return
		}
		if _, err := git.RepoRoot(repo); err != nil {
			b.reply(chatID, threadID, fmt.Sprintf("Not a git repository: %s", repo))
			return
		}
	} else if repo == "" {
		if r, err := b.deriveRepoRoot(msg); err == nil {
			repo = r
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsID := agentID + "@" + label
	tx, err := pool.Begin(ctx)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	defer tx.Rollback(ctx)

	// Build workspace row.
	var worktreeDir, branch *string
	if s != agent.ScopeShared {
		layout, err := agent.ResolveLayout(s, repo, agentID, "")
		if err != nil {
			b.reply(chatID, threadID, fmt.Sprintf("Error resolving layout: %v", err))
			return
		}
		worktreeDir = &layout.WorktreeDir
		branch = &layout.Branch
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_workspaces (id, agent_id, scope, repo_root, worktree_dir, branch)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, wsID, agentID, string(s), repo, worktreeDir, branch); err != nil {
		if wsIsUniqueViolation(err) {
			b.reply(chatID, threadID, fmt.Sprintf("Workspace %q already exists for %s", label, agentID))
			return
		}
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET active_workspace_id=$1 WHERE id=$2`, wsID, agentID); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error activating: %v", err))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Created + activated workspace %s (scope=%s, repo=%s)", label, s, shortRepo(repo)))
}

// wsSwitch activates an existing workspace.
func (b *Bot) wsSwitch(msg *tgbotapi.Message, pool *pgxpool.Pool, agentID string, args []string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if len(args) == 0 {
		b.reply(chatID, threadID, "Usage: /ws switch <label>")
		return
	}

	label := args[0]
	wsID := agentID + "@" + label

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Validate workspace exists and isn't archived.
	var ownerID string
	var archivedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT agent_id, archived_at FROM agent_workspaces WHERE id=$1`, wsID).Scan(&ownerID, &archivedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			b.reply(chatID, threadID, fmt.Sprintf("No such workspace: %s", label))
			return
		}
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if ownerID != agentID {
		b.reply(chatID, threadID, fmt.Sprintf("Workspace %s belongs to %s, not this agent", label, ownerID))
		return
	}
	if archivedAt != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Workspace %s is archived", label))
		return
	}

	if _, err := pool.Exec(ctx, `UPDATE agents SET active_workspace_id=$1 WHERE id=$2`, wsID, agentID); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Switched to workspace %s", label))
}

// wsArchive soft-deletes a workspace.
func (b *Bot) wsArchive(msg *tgbotapi.Message, pool *pgxpool.Pool, agentID string, args []string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if len(args) == 0 {
		b.reply(chatID, threadID, "Usage: /ws archive <label>")
		return
	}

	label := args[0]
	wsID := agentID + "@" + label

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Refuse to archive the active workspace.
	var active *string
	if err := pool.QueryRow(ctx, `SELECT active_workspace_id FROM agents WHERE id=$1`, agentID).Scan(&active); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if active != nil && *active == wsID {
		b.reply(chatID, threadID, fmt.Sprintf("Cannot archive %s: it's the active workspace — switch first", label))
		return
	}

	tag, err := pool.Exec(ctx, `
		UPDATE agent_workspaces SET archived_at = NOW()
		WHERE id=$1 AND agent_id=$2 AND archived_at IS NULL
	`, wsID, agentID)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if tag.RowsAffected() == 0 {
		b.reply(chatID, threadID, fmt.Sprintf("No such (or already-archived) workspace: %s", label))
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Archived workspace %s (worktree left on disk)", label))
}

// deriveRepoRoot tries to get the git repo root from the current window's CWD.
func (b *Bot) deriveRepoRoot(msg *tgbotapi.Message) (string, error) {
	uid := fmt.Sprintf("%d", msg.From.ID)
	tid := fmt.Sprintf("%d", getThreadID(msg))
	windowID, bound := b.state.GetWindowForThread(uid, tid)
	if !bound {
		return "", fmt.Errorf("no bound session")
	}
	ws, ok := b.state.GetWindowState(windowID)
	if !ok || ws.CWD == "" {
		return "", fmt.Errorf("no CWD for session")
	}
	return git.RepoRoot(ws.CWD)
}

// shortRepo shortens a repo path for display.
func shortRepo(repo string) string {
	if len(repo) > 40 {
		return "..." + repo[len(repo)-37:]
	}
	return repo
}

// wsIsUniqueViolation checks for Postgres duplicate-key errors.
func wsIsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint")
}
