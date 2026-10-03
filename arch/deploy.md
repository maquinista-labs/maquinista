# Deploy — `maquinista deploy`

How already-merged main changes reach the barceloneta runtime. The verb
internalizes `scripts/deploy-barceloneta.sh` (MAQ-23); Otavio's
auto-deploy-on-new-master-commit wiring (out of scope there) invokes this
same verb later.

## Layout

- **playa** (`~/code/maquinista`) — dev box. Push to `origin/main` = the
  release act. `scripts/deploy-barceloneta.sh` ssh-trampolines itself to
  the box for one-shot manual deploys.
- **barceloneta** (`/home/barceloneta/code/maquinista`) — runtime box.
  systemd unit `maquinista`, User=barceloneta, ExecStart
  `maquinista orchestrator start -F --env .env`, sudo NOPASSWD for the
  restart. Agents live in tmux panes of session `maquinista`.

## The verb

```
maquinista deploy [--dry-run] [--yes]
```

Runs **on the box**. Repo resolution: `MAQUINISTA_DEPLOY_DIR` > dir of
the running binary (on the box: the ExecStart binary sits in the repo) >
`~/code/maquinista` > cwd. Unit: `MAQUINISTA_DEPLOY_UNIT` > `maquinista`.

Preflight — all refusals are hard except where `--yes` is noted:

1. **Self-deploy guard** — running inside a tmux pane of the
   orchestrator's session (the r1-suicide class: the restart kills the
   caller mid-deploy). `--yes` overrides. Unknown tmux state counts as
   box pane.
2. **Git state** — branch must be `main`; tree must be clean (tracked
   changes break `git pull --rebase` midway); after `git fetch`, local
   main must have **no unpushed commits** (`origin/main..main` empty).
   Being *behind* origin/main is the normal deploy case. Never
   overridable — deploy ships pushed main only.
3. **Workers mid-flight** — `agents WHERE task_id IS NOT NULL AND
   status != 'dead'` (the `uq_agents_task_live` predicate): a restart
   kills every tmux pane, decapitating task-bound workers. Refused
   unless `--yes`; swaps are only safe between rounds. DB unreachable ⇒
   refuse without `--yes` (can't verify safety).

`--dry-run` prints the plan after running the read-only preflight and
exits 0 — nothing is pulled, built, or restarted.

Execution order:

1. `git pull --rebase origin main`
2. `SKIP_DASHBOARD=1 make build-go BINARY=.maquinista.deploy-next` —
   staged so the running binary is never touched before the swap (§9).
   PATH is augmented with `/usr/local/go/bin` (go is not on the
   non-interactive ssh PATH).
3. Staged binary `migrate` — migrations only ever ADD files, so running
   the new binary against the live DB pre-restart is idempotent.
4. Binary swap, runbook §9 order: `sudo -n systemctl stop` → poll
   `pgrep -f 'maquinista orchestrator start'` until clear (60 s cap) →
   `cp` staging over `maquinista` (rename, 0755) → `sudo -n systemctl
   start`. On stop/wait/cp failure the unit is restarted on the old
   binary best-effort before erroring.
5. **Health check gates the exit code** (exit 0 only on green): unit
   must be active (15 s grace), stay active through a 30 s window
   measured from the restart, and `journalctl -u maquinista --since
   @<restart-epoch>` must contain no `panic|fatal|ERROR` lines (same
   grep the script ran; up to 10 shown on failure). Agents are
   reconciled from the DB on start (`reconcile_agents.go`), so no pane
   restoration is needed beyond the daemon restart.

## Invariants

- Deploy is **pull-based and push-gated**: the box never ships code that
  isn't on `origin/main`.
- The orchestrator's own binary is only replaced while the unit is
  stopped — no half-old process, no ETXTBSY.
- Restarts are destructive to tmux panes by design; the mid-flight check
  + `--yes` is the only sanctioned override, and reconciliation (not
  pane preservation) is the recovery story.
- The script keeps working as the playa trampoline until the deployed
  binary itself carries the verb (one script deploy after this lands);
  after that, `ssh barceloneta 'code/maquinista/maquinista deploy'` or
  the future master-commit hook replaces it.
