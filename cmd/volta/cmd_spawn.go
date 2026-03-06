package main

import (
	"fmt"
	"os"

	"github.com/otaviocarvalho/volta/internal/agent"
	"github.com/otaviocarvalho/volta/internal/git"
	"github.com/otaviocarvalho/volta/internal/tmux"
	"github.com/spf13/cobra"
)

var (
	spawnWorktrees bool
)

var spawnCmd = &cobra.Command{
	Use:   "spawn <name>",
	Short: "Spawn a single named agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		session := getSessionName()
		if err := tmux.EnsureSession(session); err != nil {
			return err
		}

		claudeMD, err := findClaudeMD()
		if err != nil {
			return err
		}

		dbURL := dbURL
		if dbURL == "" {
			dbURL = os.Getenv("DATABASE_URL")
		}

		env := map[string]string{
			"DATABASE_URL": dbURL,
		}

		// Pre-flight checks for worktree mode.
		if spawnWorktrees {
			if _, err := git.RepoRoot("."); err != nil {
				return fmt.Errorf("--worktrees requires a git repository: %w", err)
			}
			if dirty, _ := git.HasUncommittedChanges("."); dirty {
				fmt.Println("warning: working tree has uncommitted changes")
			}
		}

		name := args[0]
		var a *agent.Agent
		if spawnWorktrees {
			a, err = agent.SpawnWithWorktree(pool, session, name, claudeMD, env)
		} else {
			a, err = agent.Spawn(pool, session, name, claudeMD, env)
		}
		if err != nil {
			return fmt.Errorf("spawning %s: %w", name, err)
		}

		if a.WorktreeDir != nil {
			fmt.Printf("Spawned: %s  →  %s:%s  (worktree: %s, branch: %s)\n", a.ID, a.TmuxSession, a.TmuxWindow, *a.WorktreeDir, *a.Branch)
		} else {
			fmt.Printf("Spawned: %s  →  %s:%s\n", a.ID, a.TmuxSession, a.TmuxWindow)
		}
		return nil
	},
}

func init() {
	spawnCmd.Flags().BoolVar(&spawnWorktrees, "worktrees", false, "isolate agent in a git worktree")
	rootCmd.AddCommand(spawnCmd)
}
