package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var (
	runAgents    int
	runNames     string
	runAttach    bool
	runWorktrees bool
	runRunner    string
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Spawn agents in tmux",
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

		r, err := runner.Get(runRunner)
		if err != nil {
			return fmt.Errorf("unknown runner %q: %w", runRunner, err)
		}

		// Pre-flight checks for worktree mode.
		if runWorktrees {
			if _, err := git.RepoRoot("."); err != nil {
				return fmt.Errorf("--worktrees requires a git repository: %w", err)
			}
			if dirty, _ := git.HasUncommittedChanges("."); dirty {
				fmt.Println("warning: working tree has uncommitted changes")
			}
		}

		// Determine agent names.
		var names []string
		if runNames != "" {
			names = strings.Split(runNames, ",")
		} else {
			pid := os.Getpid()
			for i := 1; i <= runAgents; i++ {
				names = append(names, fmt.Sprintf("agent-%d-%d", pid, i))
			}
		}

		for _, name := range names {
			var a *agent.Agent
			var err error
			if runWorktrees {
				a, err = agent.SpawnWithWorktree(pool, session, name, claudeMD, env, r, "executor")
			} else {
				a, err = agent.Spawn(pool, session, name, claudeMD, env, r, "executor")
			}
			if err != nil {
				return fmt.Errorf("spawning %s: %w", name, err)
			}
			if a.WorktreeDir != nil {
				fmt.Printf("Spawned: %s  →  %s:%s  (worktree: %s, branch: %s)\n", a.ID, a.TmuxSession, a.TmuxWindow, *a.WorktreeDir, *a.Branch)
			} else {
				fmt.Printf("Spawned: %s  →  %s:%s\n", a.ID, a.TmuxSession, a.TmuxWindow)
			}
		}

		if runAttach {
			return tmux.AttachOrSwitch(session, "")
		}

		return nil
	},
}

func init() {
	runCmd.Flags().IntVar(&runAgents, "agents", 1, "number of agents to spawn")
	runCmd.Flags().StringVar(&runNames, "names", "", "comma-separated agent names")
	runCmd.Flags().BoolVar(&runAttach, "attach", false, "attach to tmux session after spawning")
	runCmd.Flags().BoolVar(&runWorktrees, "worktrees", false, "isolate each agent in a git worktree")
	runCmd.Flags().StringVar(&runRunner, "runner", "claude", "agent runner to use (claude, openclaude, opencode)")
	rootCmd.AddCommand(runCmd)
}

// findClaudeMD locates the claude/agent-loop.md file (the orchestrator prompt).
func findClaudeMD() (string, error) {
	candidates := []string{
		"claude/agent-loop.md",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "claude", "agent-loop.md"))
	}

	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
	}

	return "", fmt.Errorf("claude/agent-loop.md not found (run from project root)")
}
