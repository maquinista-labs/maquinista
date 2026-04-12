package prompt

import (
	"fmt"
	"strings"

	"github.com/maquinista-labs/maquinista/internal/db"
)

// TaskWithContext holds a task and its context entries for prompt generation.
type TaskWithContext struct {
	Task *db.Task
	Ctxs []*db.TaskContext
}

func envSection() string {
	return `## Environment

Your environment is already configured:
- ` + "`AGENT_ID`" + ` — your unique agent identifier
- ` + "`DATABASE_URL`" + ` — the PostgreSQL connection string
- ` + "`PATH`" + ` includes the scripts directory (maquinista-claim, maquinista-done, maquinista-observe, maquinista-handoff, maquinista-pick)
`
}

// BuildSinglePrompt generates a prompt for a single task.
func BuildSinglePrompt(task *db.Task, ctxs []*db.TaskContext) string {
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
	b.WriteString("1. Claim this task: `maquinista-pick " + task.ID + "`\n")
	b.WriteString("2. Read the context above (inherited findings, handoffs, test failures).\n")
	b.WriteString("3. Work on the task. Use `maquinista-observe " + task.ID + " \"<note>\"` to record findings.\n")
	b.WriteString("4. Use `maquinista-handoff " + task.ID + " \"<note>\"` before long operations.\n")
	b.WriteString("5. Commit your changes (skip if in worktree mode — `maquinista-done` auto-commits):\n")
	b.WriteString("   `git add <files> && git commit -m \"<message>\"`\n")
	b.WriteString("6. When done: `maquinista-done " + task.ID + " \"<summary>\"`\n")
	b.WriteString("\n**CRITICAL:** You MUST commit before calling `maquinista-done` (unless in worktree mode where `$WORKTREE_DIR` is set — then `maquinista-done` auto-commits). You MUST call `maquinista-done` to mark the task complete. Without it, the task stays claimed and blocks the pipeline. Do NOT use any other mechanism to track completion.\n")
	b.WriteString("\n**Rule:** Do NOT loop. Complete this single task and return to interactive mode.\n\n")

	b.WriteString(envSection())

	return b.String()
}

// BuildAutoPrompt generates a loop prompt for auto mode.
func BuildAutoPrompt(project string) string {
	var b strings.Builder

	b.WriteString("# Auto Mode — Project: " + project + "\n\n")
	b.WriteString("Work through the task queue for project `" + project + "` until it is empty.\n\n")

	b.WriteString("## Loop\n\n")
	b.WriteString("Repeat the following:\n\n")
	b.WriteString("1. **Claim**: Run `maquinista-claim --project " + project + "`\n")
	b.WriteString("   - If output is empty: the queue is empty. **Stop and return to interactive mode.**\n")
	b.WriteString("   - If JSON is returned: this is your task spec + context.\n\n")
	b.WriteString("2. **Read context** from the JSON:\n")
	b.WriteString("   - `body`: your complete specification\n")
	b.WriteString("   - `context[].kind == \"inherited\"`: findings from dependency tasks\n")
	b.WriteString("   - `context[].kind == \"handoff\"`: where a previous attempt left off\n")
	b.WriteString("   - `context[].kind == \"test_failure\"`: what broke last time — fix exactly this\n\n")
	b.WriteString("3. **Work** on the task. Record observations with `maquinista-observe <id> \"<note>\"`.\n\n")
	b.WriteString("4. **Handoff** before long operations: `maquinista-handoff <id> \"<note>\"`.\n\n")
	b.WriteString("5. **Commit** (skip if in worktree mode — `maquinista-done` auto-commits):\n")
	b.WriteString("   `git add <files> && git commit -m \"<message>\"`\n\n")
	b.WriteString("6. **Submit**: `maquinista-done <id> \"<summary>\"`\n")
	b.WriteString("   - Tests pass → task marked done, loop back to step 1\n")
	b.WriteString("   - Tests fail → failure recorded, task reset. Loop back to step 1.\n\n")

	b.WriteString("## Rules\n\n")
	b.WriteString("- Always commit before calling `maquinista-done` (unless in worktree mode).\n")
	b.WriteString("- Never mark a task done without calling `maquinista-done`. It runs the tests.\n")
	b.WriteString("- If you see a `test_failure` context entry: fix only what broke.\n")
	b.WriteString("- One task per loop iteration.\n")
	b.WriteString("- Stop when `maquinista-claim` returns no output.\n\n")

	b.WriteString(envSection())

	return b.String()
}

// BuildBatchPrompt generates a multi-task batch prompt.
func BuildBatchPrompt(entries []TaskWithContext) string {
	var b strings.Builder

	b.WriteString("# Batch Mode\n\n")
	b.WriteString(fmt.Sprintf("Complete the following %d task(s) in order.\n\n", len(entries)))

	for i, e := range entries {
		b.WriteString(fmt.Sprintf("---\n\n## Task %d: %s\n\n", i+1, e.Task.Title))
		b.WriteString("**ID:** `" + e.Task.ID + "`\n")
		b.WriteString(fmt.Sprintf("**Priority:** %d\n\n", e.Task.Priority))

		if e.Task.Body != "" {
			b.WriteString("### Specification\n\n")
			b.WriteString(e.Task.Body + "\n\n")
		}

		if len(e.Ctxs) > 0 {
			b.WriteString("### Context\n\n")
			for _, c := range e.Ctxs {
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
		b.WriteString("1. `maquinista-pick " + e.Task.ID + "`\n")
		b.WriteString("2. Work on the task. Use `maquinista-observe` for findings.\n")
		b.WriteString("3. Commit (skip if worktree mode): `git add <files> && git commit -m \"<message>\"`\n")
		b.WriteString("4. `maquinista-done " + e.Task.ID + " \"<summary>\"`\n\n")
	}

	b.WriteString("---\n\n")
	b.WriteString("**CRITICAL:** You MUST call `maquinista-done` for each task to mark it complete. Without it, tasks stay claimed and block the pipeline.\n\n")
	b.WriteString("**After completing all tasks, return to interactive mode.**\n\n")

	b.WriteString(envSection())

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
