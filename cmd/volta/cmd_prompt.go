package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

// taskWithContext holds a task and its context entries for prompt generation.
type taskWithContext struct {
	task *db.Task
	ctxs []*db.TaskContext
}

var promptCmd = &cobra.Command{
	Use:   "prompt",
	Short: "Generate self-contained prompts for Claude agents",
}

// --- single ---

var promptSingleCmd = &cobra.Command{
	Use:   "single <task-id>",
	Short: "Output a single-task prompt",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		task, ctxs, err := db.GetTaskWithContext(pool, args[0])
		if err != nil {
			return err
		}

		fmt.Println(buildSinglePrompt(task, ctxs))
		return nil
	},
}

// --- auto ---

var autoProject string

var promptAutoCmd = &cobra.Command{
	Use:   "auto",
	Short: "Output a loop prompt for auto mode",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		proj := autoProject
		if proj == "" {
			proj = os.Getenv("VOLTA_PROJECT")
		}
		if proj == "" {
			return fmt.Errorf("--project is required for auto mode")
		}

		fmt.Println(buildAutoPrompt(proj))
		return nil
	},
}

// --- batch ---

var promptBatchCmd = &cobra.Command{
	Use:   "batch <id1> [id2] ...",
	Short: "Output a multi-task batch prompt",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		var entries []taskWithContext
		for _, id := range args {
			task, ctxs, err := db.GetTaskWithContext(pool, id)
			if err != nil {
				return fmt.Errorf("loading task %q: %w", id, err)
			}
			entries = append(entries, taskWithContext{task, ctxs})
		}

		fmt.Println(buildBatchPrompt(entries))
		return nil
	},
}

func init() {
	promptAutoCmd.Flags().StringVar(&autoProject, "project", "", "project to claim from (required)")
	promptCmd.AddCommand(promptSingleCmd)
	promptCmd.AddCommand(promptAutoCmd)
	promptCmd.AddCommand(promptBatchCmd)
	rootCmd.AddCommand(promptCmd)
}

// --- prompt builders ---

func promptEnvSection() string {
	return `## Environment

Your environment is already configured:
- ` + "`AGENT_ID`" + ` — your unique agent identifier
- ` + "`DATABASE_URL`" + ` — the PostgreSQL connection string
- ` + "`PATH`" + ` includes the scripts directory (volta-claim, volta-done, volta-observe, volta-handoff, volta-pick)
`
}

func buildSinglePrompt(task *db.Task, ctxs []*db.TaskContext) string {
	var b strings.Builder

	b.WriteString("# Task: " + task.Title + "\n\n")
	b.WriteString("**ID:** `" + task.ID + "`\n")
	b.WriteString(fmt.Sprintf("**Priority:** %d\n", task.Priority))
	b.WriteString("\n")

	if task.Body != "" {
		b.WriteString("## Specification\n\n")
		b.WriteString(task.Body + "\n\n")
	}

	writeContext(&b, ctxs)

	b.WriteString("## Instructions\n\n")
	b.WriteString("1. Claim this task: `volta-pick " + task.ID + "`\n")
	b.WriteString("2. Read the context above (inherited findings, handoffs, test failures).\n")
	b.WriteString("3. Work on the task. Use `volta-observe " + task.ID + " \"<note>\"` to record findings.\n")
	b.WriteString("4. Use `volta-handoff " + task.ID + " \"<note>\"` before long operations.\n")
	b.WriteString("5. Commit your changes (skip if in worktree mode — `volta-done` auto-commits):\n")
	b.WriteString("   `git add <files> && git commit -m \"<message>\"`\n")
	b.WriteString("6. When done: `volta-done " + task.ID + " \"<summary>\"`\n")
	b.WriteString("\n**CRITICAL:** You MUST commit before calling `volta-done` (unless in worktree mode where `$WORKTREE_DIR` is set — then `volta-done` auto-commits). You MUST call `volta-done` to mark the task complete. Without it, the task stays claimed and blocks the pipeline. Do NOT use any other mechanism to track completion.\n")
	b.WriteString("\n**Rule:** Do NOT loop. Complete this single task and return to interactive mode.\n\n")

	b.WriteString(promptEnvSection())

	return b.String()
}

func buildAutoPrompt(project string) string {
	var b strings.Builder

	b.WriteString("# Auto Mode — Project: " + project + "\n\n")
	b.WriteString("Work through the task queue for project `" + project + "` until it is empty.\n\n")

	b.WriteString("## Loop\n\n")
	b.WriteString("Repeat the following:\n\n")
	b.WriteString("1. **Claim**: Run `volta-claim --project " + project + "`\n")
	b.WriteString("   - If output is empty: the queue is empty. **Stop and return to interactive mode.**\n")
	b.WriteString("   - If JSON is returned: this is your task spec + context.\n\n")
	b.WriteString("2. **Read context** from the JSON:\n")
	b.WriteString("   - `body`: your complete specification\n")
	b.WriteString("   - `context[].kind == \"inherited\"`: findings from dependency tasks\n")
	b.WriteString("   - `context[].kind == \"handoff\"`: where a previous attempt left off\n")
	b.WriteString("   - `context[].kind == \"test_failure\"`: what broke last time — fix exactly this\n\n")
	b.WriteString("3. **Work** on the task. Record observations with `volta-observe <id> \"<note>\"`.\n\n")
	b.WriteString("4. **Handoff** before long operations: `volta-handoff <id> \"<note>\"`.\n\n")
	b.WriteString("5. **Commit** (skip if in worktree mode — `volta-done` auto-commits):\n")
	b.WriteString("   `git add <files> && git commit -m \"<message>\"`\n\n")
	b.WriteString("6. **Submit**: `volta-done <id> \"<summary>\"`\n")
	b.WriteString("   - Tests pass → task marked done, loop back to step 1\n")
	b.WriteString("   - Tests fail → failure recorded, task reset. Loop back to step 1.\n\n")

	b.WriteString("## Rules\n\n")
	b.WriteString("- Always commit before calling `volta-done` (unless in worktree mode).\n")
	b.WriteString("- Never mark a task done without calling `volta-done`. It runs the tests.\n")
	b.WriteString("- If you see a `test_failure` context entry: fix only what broke.\n")
	b.WriteString("- One task per loop iteration.\n")
	b.WriteString("- Stop when `volta-claim` returns no output.\n\n")

	b.WriteString(promptEnvSection())

	return b.String()
}

func buildBatchPrompt(entries []taskWithContext) string {
	var b strings.Builder

	b.WriteString("# Batch Mode\n\n")
	b.WriteString(fmt.Sprintf("Complete the following %d task(s) in order.\n\n", len(entries)))

	for i, e := range entries {
		b.WriteString(fmt.Sprintf("---\n\n## Task %d: %s\n\n", i+1, e.task.Title))
		b.WriteString("**ID:** `" + e.task.ID + "`\n")
		b.WriteString(fmt.Sprintf("**Priority:** %d\n\n", e.task.Priority))

		if e.task.Body != "" {
			b.WriteString("### Specification\n\n")
			b.WriteString(e.task.Body + "\n\n")
		}

		if len(e.ctxs) > 0 {
			b.WriteString("### Context\n\n")
			for _, c := range e.ctxs {
				agent := "unknown"
				if c.AgentID != nil {
					agent = *c.AgentID
				}
				kind := strings.ToUpper(c.Kind)
				b.WriteString(fmt.Sprintf("**%s** (agent: %s)\n", kind, agent))
				b.WriteString(c.Content + "\n\n")
			}
		}

		b.WriteString("### Steps\n\n")
		b.WriteString("1. `volta-pick " + e.task.ID + "`\n")
		b.WriteString("2. Work on the task. Use `volta-observe` for findings.\n")
		b.WriteString("3. Commit (skip if worktree mode): `git add <files> && git commit -m \"<message>\"`\n")
		b.WriteString("4. `volta-done " + e.task.ID + " \"<summary>\"`\n\n")
	}

	b.WriteString("---\n\n")
	b.WriteString("**CRITICAL:** You MUST call `volta-done` for each task to mark it complete. Without it, tasks stay claimed and block the pipeline.\n\n")
	b.WriteString("**After completing all tasks, return to interactive mode.**\n\n")

	b.WriteString(promptEnvSection())

	return b.String()
}

func writeContext(b *strings.Builder, ctxs []*db.TaskContext) {
	if len(ctxs) == 0 {
		return
	}
	b.WriteString("## Context\n\n")
	for _, c := range ctxs {
		agent := "unknown"
		if c.AgentID != nil {
			agent = *c.AgentID
		}
		header := fmt.Sprintf("### %s (agent: %s)", strings.ToUpper(c.Kind), agent)
		if c.SourceTask != nil {
			header += fmt.Sprintf(" from: %s", *c.SourceTask)
		}
		b.WriteString(header + "\n\n")
		b.WriteString(c.Content + "\n\n")
	}
}
