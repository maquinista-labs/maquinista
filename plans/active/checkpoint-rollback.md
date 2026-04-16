# Agent checkpoints & rollback

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

Plandex (`/home/otavio/code/plandex`) has a polished checkpoint /
rewind / branch subsystem. Relevant for maquinista because **agents
already run inside git worktrees** (`.maquinista/worktrees/<agentID>`),
so the shadow-git mechanics plandex invented are partly free for us.

Current gap: once an agent makes a bad edit — corrupts a file, runs
the wrong migration, "fixes" a test suite by deleting it — the
operator has no fast path to roll it back short of `git` gymnastics
in the worktree. Plandex showed this is solvable with ~1 kLOC. This
plan ports the relevant parts.

### What plandex does (precise)

- **Storage layout** (`app/server/db/fs.go:90-107`):
  `$base/orgs/<org>/plans/<plan>/{.git, context, conversation, results, applies, descriptions}`
  — per-plan git repo plus JSON sidecars.
- **Checkpoint == git commit + JSON sidecar**. The `PlanApply` JSON
  (`result_helpers.go:717-788`) records: apply UUID, timestamp, commit
  message, list of `PlanFileResult` IDs (which files/diffs were
  applied), `ConvoMessage` IDs (which turns produced them), user ID.
  The git commit is the durable part; the sidecar is the index.
- **When**: every `ApplyPlan` call, every rejected-apply, every
  context-load — each a git commit with a prefixed message
  (`✔ Applied…`, `🚫 Rejected…`, `📎 Context…`).
- **Rewind mechanics** (`app/cli/lib/rewind.go:107-321`):
  three-state diff — (1) target state at SHA, (2) current plan state
  at HEAD, (3) disk state. A **conflict** is a path where disk differs
  from current plan state (operator edited outside plandex). CLI
  prompts: revert overwriting, skip file revert but rewind plan,
  cancel. Server then runs `git reset --hard <sha>` (`git.go:460-467`).
- **Conversation is not deleted on rewind** — it's filtered at query
  time via `GetUndonePlanApplies` (`rewind.go:14-36`): any
  `PlanApply` with `CreatedAt >= target_time` is flagged as undone;
  the UI shows only pre-rewind messages. Disk keeps everything,
  enabling "redo" by re-applying the filtered applies.
- **Branches** (`branch_helpers.go:14-100`): first-class
  `branches(id, plan_id, parent_branch_id, name, status, …)` rows.
  `parent_branch_id` is self-referential — fork from any commit with
  `git checkout -b <name>` + DB row.
- **Atomicity**: `ExecRepoOperation` grabs a write lock; on failure
  `ClearRepoOnErr` does `git reset --hard && git clean -d -f`.

## Scope

Four phases. Phase 1 is the minimum viable win; 2 adds metadata and
observability; 3 is the rewind CLI; 4 adds branches.

### Phase 1 — Shadow-git commits per tool-write

Agents already operate inside `.maquinista/worktrees/<agentID>`,
which *is* a git worktree tied to a maquinista-owned branch
(`maquinista/<agentID>`, see `internal/agent/agent.go:72-127`). We
don't need a second "shadow" repo — the operator-hidden commits can
live on the existing worktree branch, squashed into real commits
later by the PR process.

Trigger points (all in the runner wrapper — a small shim that sits
between tmux's claude output and the filesystem):

1. **Per tool call that writes**: after each `Write` / `Edit` /
   `MultiEdit` / `NotebookEdit` tool completes, stage + commit with
   message `auto: <tool> <path>` (or `auto: <tool> <n> files`).
2. **Per turn boundary**: at the end of each assistant turn (before
   the runner waits for the next input), squash the per-tool commits
   from that turn into one `turn: <inbox_id>: <first-line-of-body>`
   commit — using `git reset --soft HEAD~N && git commit`. This
   preserves pre-turn state but hides tool-level noise.
3. **Per `agent_inbox` row processed**: same as per-turn, since the
   inbox row *is* the turn trigger. Store the resulting SHA on the
   corresponding `agent_outbox` row.

Plandex commits **only on apply events** (user-accept); maquinista
should be more aggressive because the tmux loop is fully autonomous —
the operator isn't in the "accept this diff" loop. Every write is a
de-facto apply.

**Exclusions**: commits honor `.gitignore` (same as plandex). No
second layer of filtering. If an operator doesn't want `node_modules`
auto-committed, they already handle that.

**Atomicity**: wrap each commit in a `flock`-style lock on the
worktree's `.git/maquinista.lock` file. Concurrent tool calls
serialize on it. On failure (partial stage, disk full) the lock
releases and the next commit attempt picks up whatever is staged.

### Phase 2 — `checkpoints` table + sidecar metadata

```sql
-- migration 018_agent_checkpoints.sql
CREATE TABLE agent_checkpoints (
  id              BIGSERIAL PRIMARY KEY,
  agent_id        TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  branch_name     TEXT NOT NULL,            -- maquinista/<agentID> or a fork
  parent_cp_id    BIGINT REFERENCES agent_checkpoints(id),
  git_sha         TEXT NOT NULL,            -- full 40-char SHA
  kind            TEXT NOT NULL CHECK (kind IN ('turn','tool','manual','fork','rewind')),
  turn_ref        UUID REFERENCES agent_inbox(id),  -- what triggered this turn
  outbox_ids      BIGINT[] NOT NULL DEFAULT '{}',   -- outbox rows emitted during this turn
  tool_name       TEXT,                     -- for kind='tool'
  files_changed   INTEGER NOT NULL DEFAULT 0,
  files_summary   JSONB NOT NULL DEFAULT '[]'::jsonb,
  -- shape: [{"path":"…", "op":"A|M|D", "bytes":123}, …]
  commit_message  TEXT NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  undone_at       TIMESTAMPTZ,              -- set when a later rewind supersedes this
  undone_by_cp    BIGINT REFERENCES agent_checkpoints(id)
);
CREATE INDEX agent_checkpoints_agent_created_idx
  ON agent_checkpoints (agent_id, created_at DESC);
CREATE INDEX agent_checkpoints_turn_idx
  ON agent_checkpoints (turn_ref);
```

Equivalent to plandex's `PlanApply` (`result_helpers.go:717-788`)
but relational instead of on-disk JSON. One row per commit. Kind
distinguishes granularity so the rewind UI can collapse `tool`-kind
rows under their parent `turn`-kind row.

`undone_at` / `undone_by_cp` implement plandex's
`GetUndonePlanApplies` filter without deleting rows (so redo works):
a rewind to checkpoint K sets `undone_at=now(), undone_by_cp=K` on
every row with `created_at > K.created_at` in the same branch.

The sidecar (plandex's `PlanApply.json`) isn't reproduced as a file —
the DB row replaces it. But we mirror plandex's atomicity: the row
insert and the git commit happen in the same logical operation, with
the git commit running **first** (cheap to make a commit without a DB
row, expensive to have a DB row pointing at a missing SHA). A
background reconciler deletes orphan rows whose SHAs don't resolve.

### Phase 3 — `maquinista checkpoint …` CLI + rewind with conflict detection

```
maquinista checkpoint list <agent-id> [--limit 50] [--kind turn|tool|all]
maquinista checkpoint show <agent-id> <cp-id|sha>
maquinista checkpoint tag  <agent-id> <cp-id|sha> <label>        # manual marker
maquinista checkpoint rewind <agent-id> <cp-id|sha>
  [--revert | --skip-revert]
  [--memory {keep|truncate}]          # default: keep (plandex-equivalent)
  [--outbox {keep|recall|drop}]       # default: keep
maquinista checkpoint redo <agent-id>                            # undo last rewind
```

`rewind` runs plandex's three-state diff (`rewind.go:107-214`):

1. **Target state** = `git diff <HEAD> <target_sha>` inside the
   worktree, filtered to paths the agent actually owns
   (excludes `.git`, `.maquinista/`, maquinista metadata).
2. **Current plan state** = HEAD of the worktree branch.
3. **Disk state** = `git status` on the worktree.

A path where disk differs from HEAD is a **manual operator edit**
and is flagged. The CLI prints the same three-option prompt plandex
uses:

```
⚠️  Agent worktree has changes outside checkpoint boundaries:
 • src/handlers_webhook.go  (1 modification outside tool edits)

Continue?
  [1] Revert all files to checkpoint state (overwrite manual edits)
  [2] Rewind plan history but leave files as they are on disk
  [3] Cancel rewind
```

Implementation mirrors `app/server/db/git.go:460-467`:

```go
// internal/checkpoint/rewind.go
func Rewind(ctx context.Context, tx pgx.Tx, agentID string, targetSHA string, opts RewindOpts) error {
  // 1. Flag undone: UPDATE agent_checkpoints SET undone_at=now(), undone_by_cp=<new>
  //    WHERE agent_id=$1 AND created_at > <target.created_at> AND undone_at IS NULL
  // 2. If opts.Revert: git -C <worktree> reset --hard <sha>
  //    Else:          git -C <worktree> reset --mixed <sha>  (keep files, move HEAD)
  // 3. Insert agent_checkpoints row with kind='rewind', parent_cp_id=<target>,
  //    files_summary captures the revert diff.
  // 4. If opts.OutboxMode==drop: UPDATE agent_outbox SET status='voided'
  //    WHERE created_at > target.created_at. Else no-op (plandex keeps everything).
  // 5. If opts.MemoryMode==truncate: delete agent_memories with
  //    source_ref starting with 'turn:<undone_turn_id>'. Else no-op.
  //    (Hermes also keeps memory per-session; we follow that default.)
}
```

Note: maquinista has a tighter coupling than plandex because agents
run autonomously. If the agent produced an outbox row that went to
Telegram *before* the rewind, we can't retract the Telegram message.
The CLI prints a warning: *"3 outbox rows were already delivered to
external channels; they will show `[rewound]` suffix in UI but cannot
be unsent."* A `maquinista checkpoint rewind --dry-run` flag prints
the warning without doing the revert.

`redo` is the inverse: find the most recent `kind='rewind'` row,
reset to its `parent_cp_id` branch tip, clear the `undone_*` columns
on everything between.

### Phase 4 — Branches / forks (deferred)

Plandex's `branches.parent_branch_id` pattern ports cleanly but
isn't urgent. Add when the first operator asks "I want to try two
approaches in parallel":

```sql
-- migration 019_agent_branches.sql
CREATE TABLE agent_branches (
  id                SERIAL PRIMARY KEY,
  agent_id          TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,         -- 'maquinista/<agentID>/<branchName>'
  parent_branch_id  INTEGER REFERENCES agent_branches(id),
  forked_from_cp    BIGINT REFERENCES agent_checkpoints(id),
  status            TEXT NOT NULL DEFAULT 'active',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  archived_at       TIMESTAMPTZ,
  UNIQUE (agent_id, name)
);
ALTER TABLE agent_checkpoints ADD COLUMN branch_id INTEGER
  REFERENCES agent_branches(id);
```

CLI:

```
maquinista branch list <agent-id>
maquinista branch fork <agent-id> <cp-id> <new-branch-name>    # git checkout -b + row
maquinista branch switch <agent-id> <branch-name>              # respawn agent on branch
maquinista branch archive <agent-id> <branch-name>
```

Switching a branch = spawning the agent on a different worktree
checkout. Tmux window dies and respawns pointing at the target
branch. Memory and soul are unaffected (per-agent, not per-branch).

**Merge** is deliberately unimplemented, same as plandex. Operators
who want merge use `git merge` in the worktree directly.

## Interaction with other plans

- **`agent-memory-db.md`** — rewinding **does not** delete memory
  rows by default. Plandex keeps conversation history on disk and
  filters at read time; mirror that: memory rows stay, but
  `FetchForInjection` ignores rows whose `source_ref` points to an
  undone turn. Adds one join on rewind, zero cost on normal reads.
- **`agent-to-agent-communication.md`** — if a rewound turn produced
  an outbox that fanned out to another agent's inbox, that inbox row
  is **not** retracted. Agents aren't transactional peers. The
  system-note appended to the conversation explains.
- **`agent-soul-db-state.md`** — soul edits are not captured by this
  subsystem (they live in `agent_souls`, not the worktree). Soul
  versioning is a separate concern (see that plan's open question 1).
- **`multi-agent-registry.md`** — reconcile must not clobber a
  deliberately rewound agent. Add a check: if the agent's HEAD is
  older than `agents.last_seen`, log but don't auto-advance.

## Files

New:

- `internal/db/migrations/018_agent_checkpoints.sql`
- `internal/db/migrations/019_agent_branches.sql` (Phase 4)
- `internal/checkpoint/commit.go` — per-tool and per-turn commit hooks
  triggered from the runner wrapper.
- `internal/checkpoint/rewind.go` — three-state diff + `Rewind()`.
- `internal/checkpoint/redo.go`
- `cmd/maquinista/cmd_checkpoint.go` — cobra group.
- `cmd/maquinista/cmd_branch.go` (Phase 4).

Modified:

- `internal/runner/claude.go` — wrap tool-result capture to trigger
  Phase-1 auto-commits. Tricky: claude's tool execution is in-process;
  we see completions via the monitor package (`internal/monitor/…`),
  so commits happen out-of-band, after the write has already landed.
  That's fine — the commit captures the post-write state.
- `internal/monitor/outbox.go` — attach the latest checkpoint id to
  each outbox row (new column `agent_outbox.checkpoint_id`).
- `internal/agent/agent.go` — `SpawnWithWorktree` becomes aware of
  branches (Phase 4).

## Verification per phase

- **Phase 1** — agent edits `file.go`; `git -C .maquinista/worktrees/<id> log`
  shows `auto: Edit file.go`. Agent finishes the turn; log now shows
  a squashed `turn: <inbox_id>: <body>` commit on top.
- **Phase 2** — `SELECT id, kind, git_sha, commit_message FROM
  agent_checkpoints WHERE agent_id='maquinista' ORDER BY id DESC LIMIT 5;`
  shows one row per commit with `turn_ref` linking back to the inbox
  row that triggered it.
- **Phase 3** — `maquinista checkpoint rewind maquinista <cp-id> --revert`
  on a clean worktree → files revert, undone_at flagged on three
  rows, new `kind='rewind'` row inserted. Repeat the command with a
  dirty worktree (manually edit a file first) → prompt appears;
  choose option 2 → file stays as-is but history rewinds. `maquinista
  checkpoint redo maquinista` → state returns to pre-rewind.
- **Phase 4** — `branch fork maquinista <cp-id> experiment` →
  new row + new git branch + agent_branches row. `branch switch
  maquinista experiment` → tmux window respawns on the new branch.
  Two independent histories diverge from the fork point.

## What we deliberately skip vs plandex

- **No per-file partial rewind.** Plandex doesn't either; operators
  who need this use `git checkout <sha> -- <path>` manually.
- **No organization / user / project hierarchy.** Maquinista is
  single-operator today; organization-level scoping is premature.
- **No separate "plan_config" JSONB.** Runner config already lives on
  `agents.runner_config`.
- **No PlanApply JSON sidecar files.** The DB row replaces them.
  Plandex's JSON-on-disk design predates their Postgres adoption.
- **No billing / token accounting per checkpoint.** Not a SaaS.

## Open questions

1. **Commit squash policy.** Phase 1 squashes per-turn — does that
   interfere with operators who attach to a tmux window and want to
   see tool-level commits live? Consider a `--no-squash` flag on the
   agent, plus the `kind='tool'` rows that expose the detail.
2. **Git author identity.** Per plandex, commits are authored by the
   user that owns the plan. Maquinista should attribute to the agent
   (`Otavio's Maquinista <agent-maquinista@local>`) so `git log`
   in the worktree is readable. Check this doesn't confuse `gh pr`
   review.
3. **Worktree locks.** Plandex's `ExecRepoOperation` is a single-
   writer lock. Maquinista's autonomous agents may contend with the
   operator running `git` commands in the worktree. Use `.git/index.lock`
   conventions; consider a custom `.maquinista-lock` advisory file
   with PID so tools can print "waiting for maquinista".
4. **Retention.** Checkpoints accumulate forever. Add a per-agent
   retention (`agent_settings.roster->>'checkpointRetentionDays'`,
   default 30)? Or rely on manual `maquinista checkpoint gc`?
5. **Outbox retraction for Telegram/Discord.** Should rewind attempt
   to delete the external message via bot APIs (most bots support
   message edit/delete within 48 h)? Default off — footgun if the
   operator actually wanted the message sent.
