# Pipeline

The ticket-system ↔ maquinista bridge (ADR-0005, ADR-0006): how ticket-system
issues become tasks, and how task state mirrors back onto the board. Concern
owner: `internal/pipeline/`. Agents never call the ticket system directly —
the determinism boundary is the `tasks` state machine, and the sync loop is
the only writer of board state. Provider-neutral by design (ADR-0006): core
speaks `TicketProvider` + canonical `Column`; vendor specifics live in the
provider implementation (`linear.go` for Linear).

## Intake (claim loop)

`pipeline.RunBridge` polls the ticket system every `MAQUINISTA_TICKETS_POLL`
(default 60 s, the ADR-0005 intake bound):

- fetches the team's issues in workflow state **Todo** carrying the
  **pipeline** label — via `TicketProvider.IntakeIssues` (provider chosen by
  `MAQUINISTA_TICKETS_PROVIDER`, default `linear`)
- `ClaimIssue` inserts, in ONE transaction:
  - a `tasks` row — status `ready`, project `MAQUINISTA_TICKETS_PROJECT`
    (fallback `MAQUINISTA_PROJECT`), title `[<Key>] <title>`, metadata
    `ticket_issue_id` + `ticket_url`, and — since MAQ-13 — `worktree_path`
    pointing at the issue's sibling worktree
  - a `ticket_issue_map` row — `pending_state` = "In Progress"
- the map row's primary key is the provider issue ID, so the INSERT **is**
  the claim: `ON CONFLICT DO NOTHING` + rollback makes re-claims no-ops
- **Sibling worktree provisioning (MAQ-13):** before claiming, the bridge
  provisions the issue's worktree (`EnsureIssueWorktree`,
  `internal/pipeline/worktree.go`) — house convention: `<repoBase>.<slug>`
  sibling of the repo root (e.g. `~/code/maquinista.maq13`), branch `<slug>`
  (`maq13`), branched from `origin/main` (fallback `origin/HEAD`, `HEAD`);
  slug = lowercased issue key with non-alphanumerics stripped. Idempotent:
  existing worktrees are reused, existing branches are attached, non-git
  directories at the target path are refused. The repo root comes from
  `MAQUINISTA_TICKETS_REPO`, defaulting to the orchestrator cwd's git root;
  unresolvable is logged once at startup. Provisioning is best effort: a
  failure claims the task WITHOUT `worktree_path` and the task scheduler
  parks it needs-human (below) — the loud path, never a spawn wedge. This
  replaces the manual `git worktree add` + SQL fix the MAQ-9..12 incident
  needed
- claimed tasks flow through the task-scheduler → EnsureAgent → agent_inbox
  path (wired in `orchestrator start` since EX-07, see "Task scheduler"
  below); bridge tasks carry no `metadata.role` yet (default `implementor`,
  the pipeline-worker soul from migration 035)

## Task scheduler (EX-07)

`taskscheduler.Run` runs inside `orchestrator start` (cmd_start.go), in the
same tickets-enabled block as review dispatch. Wake triggers: LISTEN
`task_events` + 30 s poll fallback. "Live" for task-scoped agent rows means
`status IN ('running','idle','working','spawning')` — `stopped` does NOT
block a claim (MAQ-18): SpawnFresh pre-registers rows as `stopped`, the
sidecar marks vanished windows `stopped`, and `maquinista stop` parks rows
that way; a task wedged behind such a row would be silently unclaimable.
The dashboard "stopped + empty tmux_window = needs provisioning" state is
role=`user`/task_id NULL only, so it never collides with this.
`DispatchOne` claims one `ready` task with no live agent (`FOR UPDATE SKIP
LOCKED`, `uq_agents_task_live` keeps replicas honest), releases stale
`stopped`/`archived` rows of previous attempts to `dead` inside the claim
TX (freeing the unique-live slot for the fresh spawn), flips the task to
`claimed`, then the cmd-side `ensureTaskWorker` adapter spawns the
implementor:

- worktree guard: the task must have a usable `worktree_path` — SpawnFresh
  does not stat; a bad path fails the spawn with a readable error and
  DispatchOne reverts the task to `ready` for the next tick
- **worktree-less park (MAQ-13):** a `ready` task with no `worktree_path` is
  never claimed at all — DispatchOne parks it `pending_approval` inside the
  claim tx with a `task_context` verdict note and ONE Pipeline-topic ping.
  This replaced the old revert-to-`ready` loop that spun ensure_agent errors
  at ~43 lines/sec during the MAQ-9..12 incident
- **unspawnable backstop (MAQ-13):** `taskscheduler.ParkUnspawnable` runs on
  every scheduler wake and parks `claimed` tasks that have no
  `worktree_path`, no live agent, and have been claimed longer than
  `MAQUINISTA_WORKTREE_GRACE` (default 10m) — legacy wedged rows and tasks
  that lost the race between claim and ensure. Exactly once by guarded
  transition (`claimed` → `pending_approval` + note in one tx)
- id mint: `<role>-<taskID>[-rN]` via `pipeline.MintWorkerID` (same shape as
  the reviewer mint; role default `implementor`, overridable via
  `tasks.metadata->>'role'`)
- exec contract: `pipeline.ResolveWorkerExec` reads the pipeline-worker
  soul's frozen extras (`default_runner` + `reasoning_class`); on read
  failure the spawn falls back to `cfg.DefaultRunner` (workers are cheap and
  replaceable — review dispatch treats the same failure as hard)
- everything else is `agentspawn.SpawnFresh`: soul clone (pipeline-worker),
  memory seed, tmux window, sidecar inbox goroutine
- the scheduler then enqueues `/work-on-task <id>` (external_msg_id
  `task:<id>` dedup) and sets `tasks.claimed_by`; `HealMissingInbox` covers
  the crash-between-claim-and-enqueue wedge; once the spawn has an owner
  the claim announces itself on the Pipeline topic (MAQ-22 one-liner,
  `pipeline.Notifyf`; the exactly-once guard is the guarded claim itself —
  one `ready`→`claimed` flip per claim — not the agent id: `EnsureAgent`
  returns a non-empty id on both outcomes, so the `ErrAgentAlreadyLive`
  path (a racing spawn's already-live pane, which the claim routes a fresh
  /work-on-task to) announces too; the note interpolates the task's
  `metadata->>'role'`, defaulting to implementor)
- two MAQ-18 safety nets run each wake: `LogBlockedReadyTasks` journals
  every `ready` task skipped because a live agent still holds it (no
  silent skips), and `ReapStaleClaims` releases `claimed` tasks back to
  `ready` when all their task-scoped agent rows are non-live and the
  claim is older than 5 min (mid-flight implementor death; the bound
  protects fresh claims whose agent row is still in SpawnFresh's stopped
  pre-registration phase) — each release announces the requeue-after-heal
  on the Pipeline topic (MAQ-22; the guarded UPDATE's RETURNING is the
  exactly-once guard)
- **freeze arm + restart sweep (MAQ-31):** `RetireFrozenClaims` runs each
  wake BEFORE the reaper — a live row on a `claimed` task that meets the
  shared freeze predicate (pipeline.FreezeFilterSQL: no outbox row AND no
  transcript growth past `MAQUINISTA_WATCHDOG_IDLE`, older than
  `MAQUINISTA_WATCHDOG_SPAWN`) is retired (🆘 exactly once via the guarded
  retire) so the reaper's all-rows-non-live check passes on the same wake
  and the claim requeues to `ready` for a fresh `-rN` implementor.
  `HealRestartCohort` runs once at unit start: live task rows whose
  `last_seen` predates the boot and that never signaled post-boot — no
  outbox row ever AND no transcript growth since the boot (the crash-
  restart cohort) — are healed on the first pass — implementor ghosts via
  the reaper, reviewer ghosts via dispatchPass, fixer episodes re-armed
  (fix row released atomically with the retire), newborns spared; a pane
  that survived the crash and is mid-turn re-binds and streams, so its
  post-boot transcript growth vetoes the heal (MAQ-9, boot-relative)

The standalone `maquinista task-scheduler` subcommand keeps the
orchestrator.EnsureAgent stub (row-only, no pty) for debugging alongside a
running bot.

## Mirror (sync)

`pipeline.RunSync` reconciles every 10 s. The bookkeeping is diff-based:
`pending_state` is the desired board column, `last_synced_state` the last
one successfully pushed; a row is due when they differ and
`next_attempt_at` ≤ now.

Mapping (`DerivedState`), task status → canonical column:

| tasks.status | Column |
|---|---|
| `ready`, `claimed` | In Progress |
| `review` | In Review |
| `changes_requested` | Changes Requested |
| `ready_to_merge` | Ready to Merge |
| `pending_approval`, `failed` | Needs Human |
| `done` | Done |
| anything else | (skipped) |

Canonical names are the stored values; the provider maps them to its own
column ids via `TicketProvider.Columns` before pushing (`SetIssueColumn`).

- the board mirror is **purely derived** from `tasks.status` — dispatch
  (below) never writes board state, it only advances task statuses and
  lets the sync loop mirror them. The explicit `pending_state` override
  mechanism remains as an escape hatch for future manual interventions;
  nothing in the automated loop writes it
- failed pushes retry with exponential backoff: 15 s base, doubling, 10 min
  cap; `attempts`/`next_attempt_at` carry the state across restarts
- diff-based reconcile is self-healing: a missed tick or an outage
  degrades to backoff, never to a lost transition
- a state name missing from the team (operator renamed a column) backs off
  like any other failure instead of hot-looping
- **PR-link sync (MAQ-10)** — the same tick also runs `SyncIssueLinks`:
  every mapped task whose `pr_url` is set and differs from
  `ticket_issue_map.pr_url_synced` (the last URL successfully written,
  migration 037) gets one `TicketProvider.AddIssueLink` write — for Linear,
  a comment carrying the raw URL. `pr_url_synced` is stamped only after a
  successful push, so ticks are idempotent (exactly one write per URL) and
  a failed push retries on the next tick. Tasks without a PR select
  nothing — no empty-link writes.

## Review dispatch (EX-03)

`pipeline.RunDispatch` (in `internal/pipeline/dispatch.go`) runs four passes
per tick (default 10 s, same cadence as sync). It never talks to the ticket
system — everything below is `tasks`/`agents` bookkeeping, and the board
sees the results only through the sync mirror.

**Entry.** `db.MarkDone` branches: a pipeline task (metadata
`ticket_issue_id`) marked done by its worker goes to `review`, not `done` —
review is part of the done path, not an optional extra. Plain tasks are
unaffected.

**Spawn pass.** For every pipeline task in `review` with a worktree and no
live reviewer agent:

- mints a fresh id `reviewer-<task>[-rN]` (never reuses an id)
- **zero-author guard**: the task's author is the agent that recorded the
  latest `task_context` result; a minted id equal to that author is
  pathological — the task parks in `pending_approval` instead of spawning
  (structural: fresh mints can't collide; the explicit check catches
  identity corruption)
- resolves runner + model from the `pipeline-reviewer` template's frozen
  extras (`default_runner`, `reasoning_class`) — `ResolveExec`: class
  `high` → `MAQUINISTA_PI_MODEL_HIGH`, else `MAQUINISTA_PI_MODEL`, empty
  model = the runner's own chain
- spawns via `ReviewSpawner` (wraps `agentspawn.SpawnFresh`: agents row
  task-bound, soul clone, tmux pane, sidecar), then bumps
  `tasks.review_rounds` (autocommit — its presence makes the next tick a
  no-op for the spawn pass; the bump is also the exactly-once guard for
  the MAQ-22 "reviewer claimed — review round N" one-liner), builds the
  round prompt (see **Human PR comments**, below — deliberately with NO
  transaction open, so the gh comment fetch never holds a DB tx), and
  enqueues the prompt in its own tx (`external_msg_id =
  review:<task>:<round>` dedups). A crash between bump and enqueue heals
  via the prompt pass
- on a `uq_agents_task_live` spawn failure, runs the **stuck-implementor
  self-heal (MAQ-14)**: if the blocking live row is the task's implementor
  whose last outbox activity is older than
  `MAQUINISTA_IMPLEMENTOR_IDLE_AFTER` (default 10m), it is auto-retired
  (`status='dead'`, guarded UPDATE — fires once, which is also the
  exactly-once Pipeline-topic notification dedup) and the reviewer spawns
  next tick. Fresh implementors are left alone (silent retry); the same
  heal guards the fixer pass.

**Prompt heal.** A crash between spawn and enqueue leaves a live reviewer
with no prompt; the heal pass re-enqueues exactly one (dedup'd) on the next
tick. The heal builds the prompt through the same code as the spawn pass,
so it carries the human-comment section too.

**Human PR comments (MAQ-16).** When a round prompt is built, dispatch
fetches the PR's issue comments (`GhRunner.PRComments` — `gh pr view --json
comments` in production) and folds the human ones into the prompt as
verdict INPUT:

- **cutoff** — the start time of the previous round's reviewer agent
  (newest `agents` row with role reviewer, other than the current one;
  `started_at` is the row's creation time). No prior reviewer row (round 1)
  = the PR-open baseline: every human comment counts.
- **filters** — bot authors (all three spellings: `is_bot`, `__typename
  Bot`, `[bot]` login suffix), the pipeline's own comments — the MAQ-16
  `[review round N]` verdicts and the MAQ-25 pickup markers (the gh CLI may
  be authenticated as a human account, so the marker — not the author —
  identifies them; `isOwnPRComment`), blank bodies, and everything
  at/before the cutoff.
- **framing** — the section is rendered explicitly as INPUT ONLY (never
  approve/request_changes verbs; the verb surface stays MAQ-11's
  Telegram/Linear/comments path). Oversized bodies are trimmed per comment
  (1000 chars) and the section as a whole (4000 chars) keeps the newest
  comments.
- **degradation** — no `GhRunner` wired, no `pr_url`, or any gh failure →
  the prompt ships without the section; nothing else changes.

**Spawn pickup markers (MAQ-25).** At spawn time — before the agent does
any work — dispatch posts a one-line pickup comment on the task's open PR
(`postPickupComment`, the MAQ-16 `PRComments`+`PRPostComment` transport),
so the PR page reads as a timeline while a round is mid-flight:

- reviewer round N (in the spawn pass, after the round bump): `🔁 [MAQ-n]
  review round N started`
- fixer episode (in the fixer pass, after the fix row): `🔧 [MAQ-n] fixer
  round N picked this up - <first finding line>` — the reason is distilled
  from the reviewer's findings (`fixPickupReason`: first non-blank,
  non-`VERDICT:` line, capped at 120 chars; fallback "addressing review
  findings")
- merge leg (in `ProcessMergeGH`, after the human-gate/status/merger-episode
  guards pass): `🚀 [MAQ-n] merge gate running`

The `[MAQ-n]` tag is the task's Linear issue key (`ticket_issue_map
.issue_key`, title-prefix fallback; keyless tasks ship without it). Exactly
one comment per spawn: a round-scoped needle scan of the PR's existing
comments (`reviewPickupNeedle` / `fixerPickupNeedle` / `mergePickupNeedle`)
dedups reposts; every failure — no PR, gh outage — only logs and never
blocks or fails the spawn/record path.

**Verdict pass.** Scans each live reviewer's newest outbox rows for the
contract verdict line (`ParseVerdict`, line-anchored, exact three-value
vocabulary). The first well-formed line wins; a malformed `VERDICT:`-ish
line is logged loudly and never transitions. On a verdict:

- **PR verdict comment (MAQ-16)** — BEFORE the guarded transition, dispatch
  posts one `[review round N]` comment on the PR (`GhRunner.PRPostComment`):
  marker + the verdict line + the reviewer's findings tail — the PR page is
  self-describing. Posting runs first so a crash between post and
  transition heals on the next tick (verdict re-parsed; the round marker
  dedups the repost — exactly one comment per round across
  requeue/prompt-heal paths), whereas the other order would lose the
  comment (the transition retires the reviewer). Everything is best-effort:
  no `pr_url`, a gh outage, or an unreadable findings tail logs and falls
  through — the verdict transition is never blocked.
- task transition: `approve` → `ready_to_merge`,
  `request_changes` → `changes_requested`, `needs_human` →
  `pending_approval` (guarded on the task still being in `review` — a raced
  row is left untouched). **Round cap (EX-04):** the transition is decided
  atomically inside the guarded UPDATE — a `request_changes` landing when
  `review_rounds` has already reached
  `MAQUINISTA_REVIEW_ROUNDS_MAX` (default 3) parks the task in
  `pending_approval` instead, with the verdict row noting the cap.
- verdict recorded in `task_context` (kind `verdict`)
- reviewer retired (`agents.status = 'dead'`) — frees the one-live-slot
  per task (`uq_agents_task_live`) for the next round's mint — and its
  tmux window is killed (best-effort)

**Fixer pass (EX-04).** A pipeline task parked in `changes_requested` with a
worktree and a `request_changes` verdict row is re-claimed by a fresh fixer
session in the SAME worktree/PR:

- mints `fixer-<task>[-rN]` (role `fixer`), resolves exec from the
  `pipeline-fixer` template's frozen extras (class `standard` →
  `MAQUINISTA_PI_MODEL`), spawns via the same `ReviewSpawner`
- the episode is keyed by `review_rounds` (frozen while parked — it only
  bumps at the next reviewer spawn); a `task_context` fix row
  (content `round <N>`) commits FIRST and stops re-spawning for the episode;
  that marker is also the exactly-once guard for the MAQ-22
  "fixer round N started" one-liner
- the fix prompt (`external_msg_id = fix:<task>:<round>` dedup) embeds the
  reviewer's newest message tail (≤6000 chars — the soul contract puts the
  numbered findings at the top of the final reply); a prompt miss heals on
  the next tick (same shape as the reviewer prompt heal)
- **no zero-author guard by design** — a fixer continuing the previous
  fixer's work is the point; the round N+1 reviewer is always a fresh mint
- **loop closure needs no new transition code**: the fixer ends with
  `maquinista-done` → `db.MarkDone` done-path branch → `review` → the
  reviewer spawn pass mints a fresh reviewer and bumps the round

**Watchdog (freeze detection, MAQ-31).** The liveness signal is
`agent_outbox` freshness — the ground-truth activity stream (§3) — with a
second veto channel: a live pipeline agent (reviewer in `review`, fixer in
`changes_requested`, merger in `ready_to_merge`) is **frozen** when it has
no outbox row AND no transcript growth (`agents.last_transcript_at`,
MAQ-9 — a healthy agent mid-command streams tool events, not outbox text)
for `MAQUINISTA_WATCHDOG_IDLE` (default 30m), past the newborn grace
`MAQUINISTA_WATCHDOG_SPAWN` (default 10m — prompt delivery races pi cold
boot, so younger agents are untouchable). Pane existence and
`agents.last_seen` are explicitly NOT signals: on the 06/10 night two
agents (an implementor pre-PR, a reviewer mid-review) sat `running` for
60+ min with live panes and zero outbox rows while the old watchdog — whose
newborn exemption was the full 2h stall bound, and which had no arm for
implementors at all — never fired. On freeze the row is retired (the
guarded UPDATE is the exactly-once 🆘 dedup; the retired agent's undriven
task prompts are dropped so episode dedup keys free up), the pane is
killed, and the work re-dispatches per role: reviewer → fresh `-rN`
reviewer IN-ROUND (task stays `review`; the spawn pass mints), fixer →
episode re-armed (the round's fix row is released; task stays
`changes_requested`), implementor → claim requeued to `ready` by the
stale-claim reaper (the task scheduler runs the same freeze predicate
every wake — `taskscheduler.RetireFrozenClaims`). Every auto-retire
notifies; silence is never a heal. The merger freeze still parks
needs-human (the money path keeps a human gate). A **restart-cohort sweep**
(`taskscheduler.HealRestartCohort`) runs once at unit start: live task rows
whose `last_seen` predates the boot (crash restart — a graceful stop
deletes task agents outright) and that never signaled post-boot — no
outbox row ever AND no transcript growth since the boot — are healed on
the first pass instead of waiting for a human to notice a stalled board.
The boot-relative transcript veto keeps the sweep honest about panes that
SURVIVED the crash: one that is mid-turn re-binds and streams tool events
(never outbox text) moments after start, and growth since the boot is
liveness — not a ghost. Rows younger than the spawn grace are left to the
continuous arm (the 04/10 lesson: never murder a newborn on sight). The
malformed-verdict case is still left to the watchdog: the parser never
guesses, the freeze bound is the backstop.

## Env contract

| Variable | Meaning | Default |
|---|---|---|
| `MAQUINISTA_TICKETS_PROVIDER` | provider name for `pipeline.NewProvider` | `linear` |
| `MAQUINISTA_TICKETS_API_KEY` | ticket-system API key | required to enable |
| `MAQUINISTA_TICKETS_TEAM_ID` | team/board id intake polls | required to enable |
| `MAQUINISTA_TICKETS_PROJECT` | project_id stamped on claimed tasks | falls back to `MAQUINISTA_PROJECT` |
| `MAQUINISTA_TICKETS_REPO` | repo root the bridge provisions sibling worktrees from (MAQ-13) | the orchestrator cwd's git root |
| `MAQUINISTA_WORKTREE_GRACE` | how long an unspawnable claimed task (no worktree, no live agent) sits before the scheduler parks it needs-human | `10m` |
| `MAQUINISTA_TICKETS_APPROVERS` | ticket-system identities (email or display name, comma-separated) allowed to drive comment verbs; empty = fail-closed | (nobody) |
| `MAQUINISTA_TICKETS_POLL` | claim-loop interval | `60s` |
| `MAQUINISTA_WATCHDOG_IDLE` | freeze silence bound: a live pipeline agent with no outbox row AND no transcript growth this long (past the spawn grace) is auto-retired and its work re-dispatched (MAQ-31) | `30m` |
| `MAQUINISTA_WATCHDOG_SPAWN` | freeze newborn grace: agents younger than this are never frozen (pi cold boot) | `10m` |
| `MAQUINISTA_IMPLEMENTOR_IDLE_AFTER` | stuck-implementor self-heal bound: outbox idleness past this retires a `uq_agents_task_live`-blocking implementor row | `10m` |
| `MAQUINISTA_REVIEW_ROUNDS_MAX` | fixer-loop cap: the request_changes landing at/after this review round parks the task | `3` |
| `MAQUINISTA_PI_MODEL_HIGH` | model for `reasoning_class: high` reviewers | falls back to `MAQUINISTA_PI_MODEL` |
| `PIPELINE_MERGE_MODE` | merge driver: `local` (MergeNoFF in repo) or `gh` (remote PR flow) | `local` |
| `PIPELINE_AUTO_MERGE` | gh mode only: truthy (`1`/`true`/`yes`/`t`/`y`) lets the queue merge without the approve verb | `0` |
| `PIPELINE_MERGE_AGENT` | gh mode only: truthy arms the merger-agent conflict leg (MAQ-15) — rebase conflicts spawn a `pipeline-merger` agent instead of parking needs-human | `0` |
| `MAQUINISTA_MERGE_ATTEMPTS_MAX` | red-PR reclaim cap before the task parks needs-human | `5` |
| `PIPELINE_GH_ALLOWED_LOGINS` | GitHub logins allowed to issue PR comment commands; unset = repo-collaborator check via gh | (collaborators) |
| `PIPELINE_GH_COMMENTS_POLL` | comment-command poll interval (floor 30s) | `60s` |

Without key + team the bridge is a logged no-op; nothing else in the
orchestrator changes. The Linear provider additionally honors the legacy
`LINEAR_API_KEY` as a key fallback (migration sugar; core never reads it).
Core neutrality is enforced by check: under `internal/pipeline/` only
`linear.go` + `linear_test.go` may mention linear; under `internal/db/` and
`cmd/` none may.

### Merge mode: gh (EX-05)

`PIPELINE_MERGE_MODE=gh` extends the determinism boundary to merging. The
driver is still the merge_queue entry — one row per merge attempt, same
statuses (`pending → merging → merged|conflict|failed`) as the local flow:

- **Enqueue pass** — each dispatch tick, tasks landing in `ready_to_merge`
  with a PR URL and a worktree get an entry (branch derived from the
  worktree's HEAD, base from `origin/HEAD`).
- **Drain — daemon-native executor (MAQ-21)** — the orchestrator runs the
  merge executor itself: `pipeline.RunMergeDrain` (started next to the
  dispatch loop, gh mode + auto-merge only) claims the OLDEST `pending`
  entry every 10 s and drives it through the full flow below. Exactly-once
  by claim (`pending → merging` held for the duration; every terminal arm
  records queue+task state before returning); an infra error with no
  terminal arm releases the entry back to `pending` for a later pass. This
  replaces the external bash watcher (`merge-watcher.service` →
  `~/.local/bin/merge-watcher.sh`), which re-ran `maquinista approve` in a
  polling loop, could not reuse the daemon's guarded transitions, and
  caused the 03/10 attempt-counter flood (128 duplicate notifies) — with
  the daemon draining, no second executor may poll beside it (the
  orchestrator tick's legacy claim-and-forget hook retired with the
  drain). At startup the drain also releases `merging` entries whose claim
  is older than 30 min — a dead executor's unreleased claim would
  otherwise wedge forever (claims are not leased; the threshold clears a
  full gate pass with margin, so no in-flight pass is ever stolen).
  Inert in local mode and when auto-merge is off (those merges belong to
  the approve verb). `maquinista merge` remains as a manual one-shot on the
  same code path.
- **Processing** — `maquinista merge` (or the approve verb, below, or the
  drain above) claims an entry and drives the remote: `fetch` → up-to-date
  fast path (MAQ-26: if `origin/<base>` is already an ancestor of
  `origin/<branch>`, skip the rewrite) → `rebase origin/<base>` →
  `push --force-with-lease` → CI gate
  (`gh pr view statusCheckRollup`) → `gh pr merge --squash`. After the
  squash lands: entry `merged` with the squash SHA, task
  `ready_to_merge → done` with `pr_state=merged`, one observation, a
  best-effort board push to Done, then worktree + local + remote branch
  cleanup.
- **CI gate** — pending checks release the entry back to `pending` (a later
  pass retries); failed checks bump `attempts` and release below the cap
  with a `🟥 gate red (ci)` one-liner (MAQ-22 — one per distinct red,
  bounded by the cap); at the cap (default 5,
  `MAQUINISTA_MERGE_ATTEMPTS_MAX`) the entry fails, the task parks
  `pending_approval`, and the Pipeline topic gets the question (EX-06).
  No checks configured counts as green.
- **Quality gate (MAQ-20 build + MAQ-21 tests)** — between the CI gate and
  the squash, two legs in order (`runMergeGate`): the branch is never
  merged uncompiled or red. Both legs materialize `origin/<branch>` — the
  exact tree a squash-merge takes, rebase included — in a detached
  throwaway worktree under `os.TempDir()` (`maquinista-mergegate-*`) and
  run there (`internal/pipeline/mergegate.go`):
  1. **build** — `go build ./...` (MAQ-20: PR #21 redeclared consts across
     files and merged green; duplicate consts are invisible in a per-file
     diff read but loud in compiler output).
  2. **test** — `go test` on the packages the branch touches (MAQ-21,
     decision C(b): `go build` does not compile `_test.go` files, so a red
     test or a removed test-only dependency slips past the build leg).
     Package selection (`touchedPackages`): the directory of every changed
     `.go` file vs the merge-base with the base branch — base drift never
     widens the set; a package the branch DELETES drops out, as does a
     directory kept alive by a non-Go file after its last `.go` file is
     deleted (testing either would red-flag a legitimate removal — the
     directory case hard-fails `go test` with `no Go files`);
     `go.mod`/`go.sum` changes widen to the whole module (dependency
     shifts can break any package's tests while the build stays green);
     non-Go changes (docs, CI configs) select nothing.

  Deterministic failure of either leg: entry `failed` (terminal — the
  branch must change; re-approval after a fix enqueues a fresh one), task
  parks `pending_approval`, and both the Pipeline-topic question and a
  `merger` observation name the FAILING STEP (`go build ./...` or the
  exact `go test <pkgs>` command, paths capped at 8 + count) and carry the
  first ~20 output lines. Infra trouble (toolchain missing, worktree add
  failure, >10 min build or >10 min test run) fails the entry without
  blaming the branch — re-approve retries. A branch root without `go.mod`
  passes vacuously (non-Go repos unchanged). The worktree is removed on
  every outcome; there is deliberately NO config knob — a skippable gate
  would reintroduce the PR-#21 failure. All merge surfaces gate: the
  approve verb (Telegram, ticket comment, CLI), auto-merge, and the drain
  converge on `ProcessMergeGH`, as does the merger-agent re-run after a
  MAQ-15 conflict resolution. Added latency is one warm `go build ./...`
  (~60-90s on the box) plus the touched packages' tests (seconds for most;
  the DB-heavy integration packages run minutes — the ~3–5 min budget is
  typical, 10m per leg is insurance).
- **Conflicts — auto merge-up leg (MAQ-26)** — with `PIPELINE_MERGE_AGENT=0`
  (default), a rebase conflict no longer parks needs-human instantly: the
  gate first tries an automatic merge-up, folding `origin/<base>` into the
  BRANCH REF (never the task worktree — a fixer may be mid-episode in it).
  GitHub's update-branch API first (`GhRunner.PRUpdateBranch`, with the
  observed head SHA so a racing push 409s into a free re-release), then a
  local fallback: `origin/<branch>` materialized in a detached throwaway
  worktree (`maquinista-mergeup-*`), `--no-ff` merge of the base, published
  as a strict fast-forward push. Success is re-verified (`origin/<base>`
  must be an ancestor of `origin/<branch>` after a fetch — a claimed-but-
  unsynced API success counts as a failure, never as mergeable) and the
  normal gate → squash path resumes on the healed ref (the gates and
  `finishMergeGH` read `origin/<branch>` throughout). Each FAILED attempt
  consumes one unit of `merge_queue.mergeup_attempts` (migration 040,
  deliberately separate from the CI/merger `attempts` budget) and comments
  on the PR through `GhRunner.PRPostComment` — what was tried, the
  conflicting files, the budget state. At the cap (N=2, a constant — a
  tunable parking cap would be argued down) the conflict parks needs-human
  exactly as before: a conflict surviving two merge-ups is a semantic
  overlap, which is what the park is for (a `maquinista resolve` comment
  still spawns a merger session). Below the cap the entry is released for
  a later pass. The MAQ-15 merger-agent leg, when armed, takes precedence
  and is unchanged.
- **Conflicts — merger-agent leg (MAQ-15)** — under `PIPELINE_MERGE_AGENT=1`, a
  rebase conflict no longer parks immediately: the processor bumps the
  entry's `attempts` (the same budget as the CI cap), parks a
  `merge_conflict` marker row (`task_context`, JSON: entry/attempt/base/
  branch/files) and releases the entry back to `pending` in ONE tx. The
  dispatch loop's merger pass spawns a fresh `pipeline-merger` agent in the
  task worktree (prompt dedup id `merger:<task>:<entry>:<attempt>`), the
  merger rebases, resolves conflicts PRESERVING both sides' semantics,
  proves the resolution (`go build ./...` + `go test` on touched packages)
  and ends with exactly `VERDICT: merged` or `VERDICT: needs_human`. On
  `merged` the verdict pass consumes the episode and the released entry
  re-runs the normal path unchanged (fetch → rebase → lease push → CI gate
  → squash). On `needs_human` (or a frozen merger — outbox + transcript
  silent past the freeze bounds, MAQ-31 — or a marker left unconsumed for
  2h) the task
  parks `pending_approval` and the entry goes `conflict` with the marker's
  files — the pre-MAQ-15 landing, unchanged. Below the attempts cap the
  episode arms; at the cap the conflict parks directly. Episode identity is
  `(entry_id, attempt)` — attempts reset on freshly enqueued entries, so
  re-approved tasks run the merger path exactly once per entry. Processors
  guard on task status and on in-flight episodes (live merger or unconsumed
  marker → release untouched), so no merge can complete behind a
  needs-human verdict and no worktree is touched mid-resolution. With
  `PIPELINE_MERGE_AGENT=0` (default) conflicts take the MAQ-26 merge-up leg
  above.
- **Human gate** — `PIPELINE_AUTO_MERGE=0` (default) makes every processing
  pass release the entry untouched; `maquinista approve <task>` on a
  `ready_to_merge` task runs the full flow immediately, overriding the gate
  for that one merge. (MAQ-12 adds the id-less third surface: comment
  `maquinista approve` on the PR itself — see "GitHub comment commands"
  below; the Telegram/CLI id-carrying verbs are unchanged.)
- **Telegram plumbing (EX-06)** — merge lifecycle notes (merged, conflict,
  infra failure, CI-cap) ride the stock delivery path via the synthetic
  `pipeline` notifier agent (migration `036`): `Notify` opens a tx, appends
  one `agent_outbox` row for the agent, commits — the relay's binding leg
  fans it into `channel_deliveries` for the Pipeline topic provisioned by
  the bot (`ensurePipelineTopic`). Failures are logged, never escalated.
  Every task mention that has a `pr_url` carries the link — verdict
  summaries (`notifyVerdict`), watchdog parks, and all merge-flow notes —
  via `prLinkSuffix`; tasks without a PR keep the old linkless text
  (MAQ-10: no null/empty links). MAQ-22 extends the journey to EVERY
  lifecycle transition, each emitted inside its guarded UPDATE branch so
  it fires exactly once per transition: task claimed (implementor, task
  scheduler; reviewer round N, review dispatch; fixer round N, fixer
  pass — the merger arm was already announced at conflict time), PR opened
  (`tools.SetPRUrl`'s guarded flip; a new URL re-announces, an idempotent
  re-set does not), gate red (CI below cap), and requeue-after-heal (the
  stale-claim reaper). Cross-package arms use the exported `Notifyf` /
  `TaskTitle`; the dead-Telegram contract holds — a failed note is logged
  and never fails the transition it reports.
- GitHub is behind `pipeline.GhRunner` (interface: `PRChecks`,
  `PRMergeSquash`, MAQ-16's `PRComments` + `PRPostComment`, and MAQ-26's
  `PRUpdateBranch` — the update-branch endpoint, whose 422/409 map to
  `ErrMergeUpConflict`/`ErrMergeUpRace`); production uses the gh CLI
  (`internal/gh`). The dispatch loop gets the same runner wired in
  `orchestrator start` (`DispatchConfig.Gh`) for the MAQ-16 PR surface; a
  nil runner disables it.

## GitHub comment commands (MAQ-12)

`pipeline.RunCommentCommands` (in `internal/pipeline/comments.go`, wired in
`orchestrator start` next to dispatch/sync, **gh merge mode only**) turns
the PR conversation into a control surface: an allowed GitHub login comments
`maquinista <verb> [args...]` on a PR and the verb runs with the task
resolved from the PR alone — no ids in the command (the PR maps 1:1 to the
task via `tasks.pr_url`).

- **Parser** (`ParseCommentCommand`) — first non-empty line, case-
  insensitive, tolerant of a leading `/` and whitespace. Anything else is
  prose and ignored entirely (never claimed, never counted).
- **Dispatch table** — verb → `CommentVerbHandler` via
  `RegisterCommentVerb`; `approve` and `resolve` ship. Adding a verb is exactly one
  registration call: parser, auth, resolution, idempotency and acks are
  shared. Unknown verbs parse and record a `no_op`.
- **`resolve` verb (merger spawn)** — fills the EX-05 "merger-agent spawn
  deferred" hole on demand. On a parked PR (`pending_approval` — rebase-
  conflict or CI-cap park), commenting `maquinista resolve` spawns a merger
  session: role `merger`, `pipeline-merger` soul (migration 035 — "rebase,
  resolve conflicts, propose, never force"), in the task's worktree. The
  prompt carries the parked branch plus the exact conflict file list from
  `merge_queue.conflict_files`, and directs: rebase onto origin/main,
  resolve every conflict, address every unresolved review-comment thread,
  push `--force-with-lease`, post the merge proposal — **the merger never
  merges**; `approve` re-runs the gate. Exactly-once rides the shared
  comment claim (dedup id `resolve:<task>:<comment>`); an episode marker in
  `task_context` audits the spawn. Gates: task must be `pending_approval`
  and carry a worktree — otherwise a clean `no_op`.
- **Target resolution (id-less)** — primary: `tasks.pr_url` ending in
  `/pull/<n>`; fallback: `merge_queue.branch` matching the PR head branch.
  No task, or the verb not applicable to its state (`approve` wants
  `ready_to_merge`) → one clean `no_op`, no state damage. Handlers re-check
  state; the merge transitions themselves are status-guarded.
- **Auth** — commenter login ∈ `PIPELINE_GH_ALLOWED_LOGINS`; unset →
  repo-collaborator check via gh (cached 10 min per login). Non-allowed
  logins are ignored silently — but claimed, so the silence is remembered.
  Transient GitHub/DB errors claim nothing and retry next pass.
- **Exactly-once** — `gh_comment_commands` (migration `037`): PK = the
  globally unique GitHub comment id, `ON CONFLICT DO NOTHING` — the INSERT
  is the claim, so a duplicate command comment never re-runs its verb and
  can never double-merge (the merge_queue live-entry index is the second
  guard). `disposition` audits the outcome (`ok` / `no_op` /
  `unauthorized` / `error`).
- **Polling, not webhooks** (home infra, no public endpoint) — one comments
  fetch (`gh api`, `since=` cursor) per watched PR per tick; watched PRs =
  open pipeline PRs (`review`/`changes_requested`/`ready_to_merge`/
  `pending_approval`). Cadence 60 s default, 30 s floor. The cursor only
  advances on a clean pass — a failed fetch **or** a failed dispatch (a
  transient dispatch error claims nothing) re-reads its window next pass;
  claims keep that exactly-once — and starts at now−10 min after a daemon
  restart.
- **Ack** — accepted commands get a +1 reaction on the comment (best-effort)
  and a note to the Pipeline topic via the EX-06 notify path.
- GitHub specifics live behind `pipeline.CommentSource` (interface:
  `PRComments` + `IsCollaborator` + `ReactToComment` + `PRHeadBranch`);
  production is the same gh CLI wrapper (`internal/gh`).

## Role souls

EX-02 seeds five pipeline role templates (migration
`035_seed_pipeline_souls.sql`, `028` style — catalog entries only; the
dispatcher clones them per spawn):

| Template | Role | reasoning_class |
|---|---|---|
| `pipeline-worker` | spec-first task execution (tlc-spec-lean: PLAN/CHECKS/BUILD/VERIFY, validators, `maquinista-done`) | `standard` |
| `pipeline-reviewer` | independent diff review — fresh per round, zero-author | `high` |
| `pipeline-arbiter` | adjudication of contested / repeated `request_changes` | `high` |
| `pipeline-fixer` | reviewer-findings resolution in the same worktree/PR | `standard` |
| `pipeline-merger` | rebase-conflict resolution in the task worktree — keep both sides, prove build+test green, one verdict (pivoted by MAQ-15, migration 038) | `standard` |

Cross-exercise contracts frozen here (EX-03 dispatch + verdict parsing build
on these literals):

- **Verdict line** — reviewer/arbiter sessions end their output with exactly
  one line: `VERDICT: approve` / `VERDICT: request_changes` /
  `VERDICT: needs_human`. Merger sessions (MAQ-15) end with exactly
  `VERDICT: merged` / `VERDICT: needs_human` (`ParseMergeVerdict`).
- **Dispatch hints** — every pipeline template carries `extras` keys
  `default_runner` (all `pi`) and `reasoning_class`; EX-03 resolves them to
  runner + model at dispatch.

Souls stay runner-agnostic (ADR-0005 revisit triggers): re-binding a role to
a new harness is an extras edit, not a soul rewrite.

## Comment actions (MAQ-11)

The human release authority is a comment from an allowed approver on a
surface he already has open — the Telegram Pipeline topic or the ticket
issue. Both surfaces route into ONE verb arm, `pipeline.ApproveRef`
(`approve.go`): resolve the task reference (full uuid or unambiguous
prefix), no-op unless the task is `ready_to_merge` (approve never forces a
transition), then `RunMergeOnApprove` — the same queue flow the CLI uses,
overriding the auto-merge gate for that one merge. No new merge code path.
The verb arms a merge audit observation (`approved via … by <who>`).

- **Telegram** — `/approve <task-ref>` as a bot command (any topic,
  `ALLOWED_USERS` gate is the stock one in `handleUpdate`), or a bare
  `approve <task-ref>` typed in the Pipeline topic
  (`handlePipelineApproveText` intercepts BEFORE the routing ladder — the
  synthetic pipeline agent has no sidecar, so the ladder would strand the
  message in its inbox). The in-topic form is regex-strict (`approve <one
  ref>`) and gated on the thread's owner binding being the `pipeline`
  agent, so ordinary agent conversations are never hijacked. The ack is
  immediate; the merge runs async and its outcome rides the standard
  notifier notes into the same topic.
- **Reply comments (MAQ-24)** — a plain reply to a pipeline notification in
  the Pipeline topic is posted verbatim as a PR comment on that task's open
  PR (`internal/bot/pipeline_reply.go` intercept, after the verb arm;
  `internal/pipeline/telegram_comment.go` for resolution/claim/post). See
  "Telegram reply → PR comment" below.
- **Ticket comments** — `RunCommentApprovals` polls at the sync cadence
  (10 s, same pass family). Each pass: ready_to_merge tasks with a
  `ticket_issue_map` row → `CommentFetcher.RecentComments` (optional
  provider extension; Linear implements it, providers without comments are
  a logged no-op) → verb parse (whole body must be `approve` or
  `maquinista approve`, punctuation-tolerant — prose mentioning the word
  never merges) → approver gate (`MAQUINISTA_TICKETS_APPROVERS`, email or
  display name, case-insensitive, empty = fail-closed) → consume →
  approve.
- **Exactly-once** — the comment id is consumed into `ticket_comment_log`
  (INSERT ON CONFLICT DO NOTHING + RETURNING) BEFORE the verb runs, so a
  second identical comment or a pagination overlap loses the race and never
  reaches the merge; `merge_queue`'s partial live index is the second
  guard. A failing verb still consumes its comment — the operator
  re-approves with a new comment or the CLI.
- **Notifier verbs** — the merge-proposal note teaches the comment forms
  (short id, typeable from a phone): reply `approve <short-id>` in the
  Pipeline topic or comment `approve` on the ticket issue. The CLI verb
  (`maquinista approve`, now routed through `ApproveRef` too) keeps working.
  Notes about `pending_approval` tasks (round cap, needs-human escalation,
  CI-cap) keep the CLI-only form — the comment verb deliberately does not
  act on `pending_approval`.

## Telegram reply → PR comment (MAQ-24)

Feedback no longer requires a browser: a non-verb reply to a pipeline
notification in the Pipeline topic lands as a PR comment on that task's
open PR, and the next review round weighs it through MAQ-16's existing
human-comment machinery (the comment is authored by the gh CLI account and
carries no `[review round N]` marker, so it counts as human INPUT
unchanged).

- **Target resolution, no prose parsing** — task-aware notifications stamp
  their task id into the outbox content (`NotifyTask`/`notifyTaskf`; every
  verdict/merge/watchdog note uses them). The bot resolves a replied-to
  Telegram message via `channel_deliveries.external_msg_id` (the message id
  the dispatcher recorded) → `agent_outbox` → `content->>'task_id'`. A miss
  (not a notification, or a legacy row without the stamp) falls through to
  the routing ladder exactly as before.
- **Exactly-once** — `telegram_pr_comments` (migration `039`): PK
  `(chat_id, message_id)`, the INSERT is the claim made BEFORE posting, so
  a retried/redelivered update of the same reply never double-posts.
  `disposition` audits the outcome (`ok` / `no_op` / `error`), and the
  comment URL is stored + quoted back into the topic as the delivery
  confirmation.
- **Open-PR guard** — `pipeline.PostPRComment` gates on `tasks.pr_url`
  being a GitHub pull URL and the PR state being OPEN (`gh pr view
  --json state`); anything else is `ErrNoOpenPR` → one graceful "no open
  PR" reply, nothing posted (AC: no-PR tasks are safe). gh specifics live
  behind `pipeline.PRCommentPoster` (`PRState` + `PRPostCommentURL`);
  production is the shared gh CLI wrapper (`internal/gh`), which captures
  the comment URL `gh pr comment` prints.
- **Verbs stay MAQ-11's** — the in-topic `approve <ref>` intercept runs
  first (handlers.go ordering), and `maquinista <verb>`-shaped replies are
  refused here: posting them would arm the MAQ-12 GitHub comment-command
  surface from chat text. approve/reject/resolve behavior is unchanged.

## TODO

- fixer re-claim loop: **shipped (EX-04)** — see "Fixer pass" above; round
  cap + fixer watchdog included
- merge mode: **shipped (EX-05)** — see "Merge mode: gh" above; enqueue
  pass, rebase + CI gate, approve-verb override included
- merger conflict leg: **shipped (MAQ-15)** — merger-agent episodes on
  rebase conflicts; see the Conflicts bullet in "Merge mode: gh"
- Telegram plumbing: **shipped (EX-06)** — synthetic notifier agent,
  Pipeline topic, merge lifecycle notes; approve comment verbs (MAQ-11)
  extend it, see "Comment actions" above
- comment actions: **shipped (MAQ-11)** — see "Comment actions" above;
  park/rerun/retry verbs are follow-ups once approve proves the pattern
- worker spawn wiring: **shipped (EX-07)** — see "Task scheduler" above;
  task-scheduler in `orchestrator start`, ensureTaskWorker → SpawnFresh

