# ADR-0008: Turn-End Completion Contract for Pipeline Agents — Watchdog Demoted to Backstop

- **Status:** Aceito (2026-10-08, Otavio — implementation tracked in MAQ tickets)
- **Date:** 2026-10-08
- **Deciders:** Otavio
- **Scope:** implementor/reviewer/fixer completion signaling, freeze watchdog semantics (MAQ-31), respawn-cap ledger, monitor turn-end detection
- **Depends on:** freeze watchdog (MAQ-31, arch/pipeline.md §Watchdog), transcript liveness veto (MAQ-9), worker/reviewer/fixer souls (migration 035), `maquinista-done` state transitions (dispatch.go, tools/tasks.go)
- **Amends:** none — refines MAQ-31's freeze semantics without superseding them

## Context

Pipeline agents have two completion mechanisms today:

1. **Event-driven (the designed path):** the agent's final act is
   `maquinista-done <task-id> "<summary>"`, which drives the state transition
   (dispatch.go:12,363,857; tasks.go:138). The contract is written into the
   surfaces the agent reads: the worker soul seed
   (migrations/035_seed_pipeline_souls.sql:36 — "finish with maquinista-done
   … never signal done with a red proof"), the CLI prompt builder
   (prompt.go:47-50, with a CRITICAL block), and the fixer/reround prompt
   bodies (dispatch.go:1089, reround.go:423).
2. **Silence-based backstop:** the freeze watchdog (MAQ-31,
   arch/pipeline.md §Watchdog) retires an agent with no outbox row AND no
   transcript growth past `MAQUINISTA_WATCHDOG_IDLE` (30m default), past the
   10m spawn grace; the implementor phase's claim then requeues to `ready`
   (respawn cap `MAQUINISTA_WATCHDOG_RESPAWN_CAP` default 3, ledger = freeze
   observation rows) or parks needs-human once the cap is spent.

**The incident (2026-10-08, MAQ-37 round r8).** Implementor
`…6b44dc7e…-r8` claimed the task at 10:00:22 and finished real work by
10:08:30 — merged origin/main into its branch (PR #44 left clean and
up-to-date), pushed, tests green, final summary written to the outbox
(1790-char row). It then ended its turn **without ever invoking
`maquinista-done`**: zero occurrences of the command anywhere in its pi
transcript, not even in reasoning. The claim prompt it received was the thin
`/work-on-task <task-id>` envelope — the contract lives in the soul, and
this round dropped it. Compare 07/10's r7 session on the same task, which ran
the done script repeatedly ("The done script ran the full test suite").

Nobody noticed the miss. At 10:38:46 the watchdog retired r8 exactly as
designed — but MAQ-37's respawn cap (3) was already spent on 07/10 freezes,
so the circuit breaker **parked the task needs-human instead of requeueing**.
A successful completion was classified, and punished, as a freeze failure:
+30 minutes of dead latency, the last respawn slot burned, and a false
"Needs Human" board state for work that was already done.

**Root cause is the signal model, not the watchdog** (the watchdog did its
job):

- "finished successfully" and "frozen mid-work" are **indistinguishable** —
  both are silence, because turn end is not a signal the pipeline consumes.
- The done-contract is **prompt-policed** (soft): compliance varies by round
  (r7 yes, r8 no), and nothing detects "turn ended without a done signal".
- The respawn-cap ledger counts all freezes **equally**: a silent success
  burns budget exactly like a genuine freeze→requeue→freeze loop.
- Silence latency is structural: every missed done signal pays the full 30m
  `WATCHDOG_IDLE` before the machine reacts.

## Options considered

| Option | Verdict |
|---|---|
| **A. Status quo** (silence-only, watchdog infers everything) | Rejected: the incident is its steady state — 30m latency + cap burn on every successful round that skips the done verb |
| **B. Stronger prompt enforcement** (louder CRITICAL blocks, soul edits) | Rejected: still prompt-policed; r8's transcript shows compliance variance is the failure mode, not wording |
| **C. Outbox sentinel marker** (agent must echo a magic string) | Rejected: same soft contract with a new parser to drift; adds nothing the done verb doesn't already do |
| **D. Sidecar injects done at PTY close** | Rejected: pane lifecycle ≠ turn lifecycle — pi/claude agents are multi-turn; a pane close is not a work completion, and mid-turn pane deaths would synthesize false dones |
| **E. Transcript turn-end as a first-class signal + one-shot completion nudge + cause-aware freeze ledger** | **Chosen** |

E wins because the monitor already tails every runner's transcript and
already parses per-runner shapes (pi, claude, opencode, openclaude) — turn
end (final assistant message with no pending tool call) is already observable
there, so no new liveness source is invented and no agent-side change is
required. It is also the only option that separates "ended cleanly" from
"froze mid-turn", which is precisely the distinction the watchdog lacks.

## Decision

1. **Turn-end event.** The monitor emits a turn-end signal for live pipeline
   agents: the transcript tail shows an assistant message closing the turn
   with no pending tool call. pi first (the runner all pipeline souls pin via
   `default_runner: pi`); claude/opencode parsers follow the same pattern.
2. **One-shot completion nudge.** On turn-end-without-done, the owning leg
   (task scheduler for the implementor phase; dispatch for reviewer/fixer
   legs) sends exactly ONE nudge per round — "your turn ended; finish with
   `maquinista-done`" — deduped by the same guarded-UPDATE exactly-once
   pattern the 🆘 retire uses. The nudge itself is round-capped: the
   existing round cap bounds it, so a genuinely confused agent still
   terminates into the human escape instead of looping.
3. **Cause-aware freeze ledger.** Freeze observation rows gain a cause:
   - `silent_success` — turn end was observed before the freeze window
     elapsed: retire WITHOUT burning respawn budget. If the work artifacts
     are already in place (open PR, branch up to date with base), the
     requeue-to-`ready` hop is skipped and the task goes straight to
     `review` — the fresh-round no-op verification, minus the respawn.
   - `true_freeze` — no turn end observed (crash, hang): unchanged
     behavior, counts against the cap; the circuit breaker keeps its teeth.
4. **Watchdog unchanged as backstop.** Bounds stay 30m/10m/cap-3. The
   watchdog stops being the *primary* completion detector and becomes what
   it should have been: the crash/freeze safety net.
5. **State-machine-first compliance.** This adds the missing transition —
   `turn-end-no-done → nudge → done | retire(silent_success)` — per
   AGENTS.md's rule: if a case lacks a transition, add the transition, not a
   one-off. Silence→freeze stops being the default path for round endings.

## Consequences

**Positive**

- A missed done signal costs one nudge round-trip (minutes) instead of
  30m + a respawn slot.
- Successful rounds stop landing in Needs Human; the board stops lying.
- The respawn cap regains its meaning: it counts freeze *loops*, not
  successes.
- No agent-side change, no new protocol, works across runners via the
  existing monitor source abstraction.

**Negative / risks**

- Turn-end parsing is per-runner: each source needs shape tests (pi
  verified live 08/10 — r8's transcript ends in a bare assistant message;
  claude/opencode to follow).
- Nudge ↔ watchdog races (both see the same agent): the guarded-UPDATE
  exactly-once pattern from the retire path is the template; a nudge lost to
  a concurrent retire degrades to today's behavior, never to a double-fire.
- A nudge re-prompting an agent that intentionally ended (nothing left to
  do) is possible; the one-shot cap and the silent-success classification
  make that cheap (one wasted prompt, no cap burn).

**Neutral**

- `maquinista-done` remains the ONLY accepted completion verb; the nudge
  never completes on the agent's behalf.

## Implementation plan (fases)

- **F1 — Monitor turn-end detection (pi), shadow mode.** Emit + log only;
  no action taken. Measure: how many round endings per day end without a
  done signal (incidence justifies F2). Numbers decide F2's order.
- **F2 — Scheduler nudge + cause-aware ledger.** One-shot nudge leg on the
  implementor phase; `cause` column on freeze observation rows;
  silent-success retire path (no cap burn, direct-to-review when artifacts
  allow). Guarded updates throughout.
- **F3 — Review legs + docs.** Nudge for reviewer/fixer rounds;
  arch/ updates below; dashboard surface for the nudge event (optional).

## arch/ impact (AGENTS.md sync duty)

- `arch/pipeline.md` — §Watchdog: turn-end signal precedes freeze;
  silent-success vs true-freeze classification; cap ledger semantics.
- `arch/messaging.md` — monitor path: new turn-end event emission alongside
  outbox writes.
- `arch/agent-lifecycle.md` — completion contract: done verb is the sole
  accepted completion; the nudge is the recovery transition.
- `arch/database.md` — freeze observation rows gain the cause column (F2).

## References

- Incident journal: `10:38:46 taskscheduler: watchdog retired frozen
  implementor-…6b44dc7e…-r8 — respawn cap (3) reached, parked needs-human`
  (2026-10-08).
- r8 pi transcript (zero `maquinista-done` occurrences, bare assistant
  turn-end at close) vs r7 transcript (07/10, done script runs) —
  `~/.pi/agent/sessions/--home-barceloneta-code-maquinista.maq37--/` on
  barceloneta.
- arch/pipeline.md §Watchdog (MAQ-31); internal/taskscheduler/freeze.go
  header; migration 035 worker soul; prompt.go BuildSinglePrompt.
- Lineage: MAQ-31 (freeze arms), MAQ-14 (stuck implementor), MAQ-9
  (transcript liveness veto).
- **Tracks:** [MAQ-43](https://linear.app/brisaai/issue/MAQ-43) (F1, shadow
  turn-end signal) · [MAQ-44](https://linear.app/brisaai/issue/MAQ-44) (F2/F3,
  nudge + cause-aware ledger), related to each other in Linear.
