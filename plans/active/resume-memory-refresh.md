# Resume-side memory refresh

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

The session-resume pipeline that landed alongside the tier-3 pivot
(see `archive/per-topic-agent-pivot.md` and the reconcile loop in
`cmd/maquinista/reconcile_agents.go`) brings long-lived Telegram
topics back across a `./maquinista stop` / `start` cycle by calling
`claude --resume <session_id>`. That preserves the agent's
conversation history verbatim.

It does **not** push any memory / soul deltas written while the
daemon was down.

`active/agent-memory-db.md` §Phase 3 explicitly documents
snapshot-at-spawn semantics:

> The slice is **frozen** for the lifetime of the tmux process —
> new memory rows written mid-session update the DB but do not
> rewrite the tempfile.

On a fresh spawn that's the right trade-off (prefix cache stability).
On a **resume** after a multi-day restart the same rule bites:
archival passages added by auto-flush or operator import while the
daemon was down are invisible to the resumed claude session. Same
for soul edits via `maquinista soul edit` / `import`.

## Scope

Two phases. Phase 1 handles the "deltas since the last spawn" case;
Phase 2 is the continuous drift story for very long-running
resumes.

### Phase 1 — First-turn "catch-up" inject on resume

When `respawnAgent` fires with `resumeSessionID != ""`, after the
runner's TUI is ready (see `waitForRunnerReady`), inject a synthesized
user turn that dumps whatever memory / soul deltas were written since
the session's `started_at`:

1. Read `agents.started_at` for the resuming agent (already populated
   by the Claude SessionStart hook on the original spawn).
2. `SELECT … FROM agent_memories WHERE agent_id=$1 AND created_at >
   started_at` → the archival rows added while offline. Same for
   `agent_blocks.updated_at > started_at`.
3. `SELECT updated_at FROM agent_souls WHERE agent_id=$1` — if it's
   newer than `started_at`, diff core_truths/boundaries/vibe and
   surface the deltas.
4. If any of the three produced content, enqueue one `agent_inbox`
   row with `from_kind='system'`, `origin_channel='a2a:catchup'`,
   content looking like:

   ```
   Catching you up on changes since {started_at}:

   ## New memories
   - …

   ## Memory blocks updated
   - persona: {diff}

   ## Soul updated
   - vibe: {new text}
   ```

   The regular inbox consumer / sidecar picks it up on its next poll
   and sends-keys it into the pty. Claude treats it as a system-ish
   message and acknowledges it.

5. After injection, `UPDATE agents SET started_at = NOW()` so the
   next resume catches up from this point, not the original spawn.

### Phase 2 — Continuous drift handling

Long-running resumed sessions accumulate stale context (the claude
session has been alive for weeks, new memories keep arriving). Two
options:

1. **Periodic catch-up** — a background goroutine that runs the same
   diff-inject flow every N hours. Gated by a config flag so the user
   opts in.
2. **Tool-driven pull** — expose a `memory_refresh` tool to the
   agent. The agent calls it when it notices its context feels stale
   (e.g. operator references a fact the agent doesn't remember).
   Cleaner because the LLM controls timing.

Option 2 is cheaper and leaves the agent in charge of its own
context. Phase 2 adopts that unless operators ask for Phase 2 option 1.

## Interaction with other plans

- Depends on `active/per-agent-sidecar.md` Phase 1 in the cleanest
  form — the sidecar owns the pty so it's the natural place to
  synthesize the catch-up turn at the exact right moment
  (post-TUI-ready, pre-first-user-message).

  Without per-agent sidecars, Phase 1 can still be implemented as a
  call in `respawnAgent` that writes the inbox row; the single
  mailbox_consumer then drives it.

- Needs `active/agent-memory-db.md` Phase 4 (auto-flush) to have
  landed so there's a reason to inject deltas on resume. That
  shipped in commit `8f7a9…` — good.

- Touches `active/agent-soul-db-state.md` Phase 4 semantics: if the
  operator edited the soul, Phase 1 surfaces the diff. Without this
  plan, soul edits are invisible until a fresh spawn.

## Verification

- **Phase 1 happy path.** `maquinista agent add alice --persona "I like
  rust"`; send a message in alice's topic; `./maquinista stop`; run
  `maquinista memory remember alice --tier long_term --category fact
  --title "Operator has a cat named Pico" --body "…"`; then
  `maquinista soul edit alice --vibe "More cats, always cats."`;
  `./maquinista start` — the first message alice processes includes
  a catch-up system turn referencing the new memory and the vibe
  change. Claude's reply acknowledges both.

- **Phase 1 empty path.** No memory / soul changes while the daemon
  was down → no catch-up row inserted.

- **Phase 2 tool path.** Agent calls `memory_refresh` → receives the
  same diff payload in the tool result.

## Files

- `cmd/maquinista/reconcile_agents.go` — extend `respawnAgent` with
  a call to a new helper `injectResumeCatchup(ctx, pool, agentID)`.
- `internal/memory/resume_catchup.go` — new: diff query + inbox
  synthesis.
- `internal/soul/compose.go` — add a `DiffSince(soulAID, timestamp)`
  helper for Phase 1.

## Open questions

1. **How noisy is the catch-up row?** Operators who routinely add 50
   memories between restarts will drown claude in unrelated notes.
   Cap at the top-K most relevant (pinned + recent long_term)?
2. **Respect `respect_context` flag on the soul?** The field already
   exists for budget-aware summarization; this plan should honor it
   by shrinking the catch-up payload when a row has
   `respect_context=TRUE`.
3. **What if claude has been /clear'd since the last spawn?** The
   stored session_id still resumes but the history is empty. The
   catch-up approach still works, just makes the first resumed turn
   carry more load. Good enough.
