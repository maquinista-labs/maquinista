package main

import (
	"fmt"
	"os"

	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var (
	spawnWorktrees bool
	spawnRunner    string
	spawnScope     string
	spawnRepoRoot  string
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

		r, err := runner.Get(spawnRunner)
		if err != nil {
			return fmt.Errorf("unknown runner %q: %w", spawnRunner, err)
		}

		// Resolve scope. --worktrees is the deprecated alias for
		// --scope=agent; if both are set, --scope wins and --worktrees
		// prints a warning.
		scope := agent.WorkspaceScope(spawnScope)
		if scope == "" {
			if spawnWorktrees {
				fmt.Fprintln(os.Stderr, "warning: --worktrees is deprecated; use --scope=agent")
				scope = agent.ScopeAgent
			} else {
				scope = agent.ScopeShared
			}
		} else if spawnWorktrees {
			fmt.Fprintln(os.Stderr, "warning: both --scope and --worktrees set; --scope wins")
		}
		if err := scope.Validate(); err != nil {
			return err
		}

		// Resolve repo root. For scope=shared: --repo or process cwd
		// (window opens there). For scope=agent: --repo or the process
		// cwd's git root. Explicit, no silent fallback to a distant repo.
		repoRoot := spawnRepoRoot
		if repoRoot == "" {
			if scope == agent.ScopeAgent {
				root, rerr := git.RepoRoot(".")
				if rerr != nil {
					return fmt.Errorf("--scope=agent requires --repo or to be run from inside a git repository: %w", rerr)
				}
				repoRoot = root
			} else {
				cwd, _ := os.Getwd()
				repoRoot = cwd
			}
		}
		if scope == agent.ScopeAgent {
			if dirty, _ := git.HasUncommittedChanges(repoRoot); dirty {
				fmt.Println("warning: working tree has uncommitted changes")
			}
		}

		name := args[0]
		layout, err := agent.ResolveLayout(scope, repoRoot, name, "")
		if err != nil {
			return err
		}
		a, err := agent.SpawnWithLayout(pool, session, name, claudeMD, env, r, "executor", layout)
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
	spawnCmd.Flags().BoolVar(&spawnWorktrees, "worktrees", false, "deprecated alias for --scope=agent")
	spawnCmd.Flags().StringVar(&spawnScope, "scope", "", "workspace scope: shared | agent (default: shared; see plans/active/workspace-scopes.md)")
	spawnCmd.Flags().StringVar(&spawnRepoRoot, "repo", "", "project git repo root (defaults to process cwd's git root for --scope=agent)")
	spawnCmd.Flags().StringVar(&spawnRunner, "runner", "claude", "agent runner to use (claude, openclaude, opencode)")
	rootCmd.AddCommand(spawnCmd)
}
