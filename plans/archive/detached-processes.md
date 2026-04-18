# Detached execution for `maquinista start` and `maquinista dashboard start`

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres
> is the system of record**. This plan concerns *process lifecycle*,
> not persistent state — PID files are runtime scratch, not schema.

## Context

Today both long-running commands occupy the terminal:

- **`./maquinista start`** runs in the foreground. It writes
  `~/.maquinista/maquinista.pid` but the caller's shell is blocked
  on the bot / monitor / mailbox goroutines. Closing the terminal
  kills the daemon. Stdout / stderr go to the terminal; nothing is
  persisted to disk, so after-the-fact debugging means reproducing
  the failure.
- **`./maquinista dashboard start`** is *almost* detached — it
  writes `~/.maquinista/dashboard.pid`, pipes the Node child into
  `~/.maquinista/logs/dashboard.log` via the Supervisor, and owns
  a clean stop cascade. But the CLI process itself still blocks
  on signal, so the operator has to `&` / `nohup` / tmux it by
  hand to escape the terminal.

There are also three ergonomic gaps:

1. No top-level "start everything" — launching the full stack is
   two commands (`start` + `dashboard start`), in two terminals,
   or with operator-maintained shell scripts.
2. The orchestrator command tree is shaped weirdly: `start` is
   the bot+orchestrator daemon, but `dashboard` already has
   `start / stop / status / logs` subcommands. The two halves are
   not symmetric.
3. No single place to tail *both* log streams while debugging a
   cross-component issue (e.g. dashboard action fires an audit
   row that the dispatcher then re-emits to Telegram).

## Goals

1. Every long-running command detaches cleanly and returns the
   shell immediately.
2. The two daemons are symmetric: both have `start / stop /
   status / logs` under their own namespace.
3. A top-level `start` / `stop` pair launches / tears down the
   whole stack in one command.
4. A single `maquinista logs` command tails both streams, clearly
   labelled, so operators don't have to open two terminals.
5. All log files live under one conventional directory so log
   rotation / shipping / cleanup has one target.

## Non-goals

- Log rotation policy (size-based, time-based). Out of scope —
  the files are append-only for now; if they grow we add a
  rotator in a follow-up.
- Structured / JSON logging. The current streams are plain text
  from whatever each component prints. Rewriting every
  `log.Printf` is out of scope.
- systemd / launchd integration. The daemonization is pure Go,
  no external supervisor. Operators who want a system unit can
  wrap `maquinista start --foreground` in their own `.service`.
- Windows support. Detachment uses `syscall.SysProcAttr{Setsid:
  true}` — POSIX only. Matches the rest of the codebase.

---

## Target command tree

```
maquinista start                    # new: launches orchestrator + dashboard (detached)
maquinista stop                     # new: stops both
maquinista status                   # new: prints status of both
maquinista logs [-f] [--component]  # new: tails both streams

maquinista orchestrator start       # new: detached bot + dispatcher + mailbox + reconcile
maquinista orchestrator stop        # new
maquinista orchestrator status      # new
maquinista orchestrator logs [-f]   # new

maquinista dashboard start          # existing, now detached by default
maquinista dashboard stop           # existing
maquinista dashboard status         # existing
maquinista dashboard logs [-f]      # existing
```

Every `start` accepts `--foreground` (or `-F`) to opt out of
detachment. Handy for debugging and for users who want to wrap
the process in an external supervisor.

## Filesystem layout

All state lives under `~/.maquinista/` (unchanged):

```
~/.maquinista/
├── orchestrator.pid            # new (replaces maquinista.pid — see migration notes)
├── dashboard.pid               # existing
└── logs/
    ├── orchestrator.log        # new
    └── dashboard.log           # existing
```

`maquinista.pid` → `orchestrator.pid` is a rename. Stop path
reads both for one release cycle so in-flight daemons from a
pre-upgrade build are still killable.

## Scope — one commit per step

### Commit D.1 — Extract a reusable `daemonize` helper

Lift the PID + log + signal logic that today lives half in
`cmd_start.go` and half in `cmd_dashboard.go` / `supervisor.go`
into a small internal package.

New package `internal/daemonize/` with:

- `Spec{Name string; LogPath string; PIDPath string; Foreground bool}` — config.
- `Run(spec Spec, work func(ctx context.Context) error) error` — if
  `Foreground` is true, runs `work` in the current process with
  signal handling and PID-file bookkeeping; if false, re-execs
  `os.Args[0]` with `--foreground` appended, redirects stdout +
  stderr to `LogPath` (O_APPEND | O_CREATE, 0o644), detaches via
  `syscall.SysProcAttr{Setsid: true}`, prints the child PID, and
  exits 0. The child runs its own `Run(..., Foreground: true)`
  branch, writes its PID, handles signals, and cleans up.
- `Stop(spec Spec, grace time.Duration) error` — reads PID file,
  checks process is alive, sends SIGTERM, waits up to `grace`,
  escalates to SIGKILL, removes PID file on success.
- `Status(spec Spec) (pid int, alive bool, err error)`.
- `TailLogs(ctx context.Context, spec Spec, follow bool, w io.Writer) error` — shared tailer (existing dashboard-logs code moves here).

Tests: Go unit tests spawn a small test binary that sleeps and
respects SIGTERM; assert detach returns immediately, PID file
is correct, log file receives stdout, stop is graceful.

### Commit D.2 — Migrate dashboard to `daemonize`

Replace the ad-hoc PID / signal logic in `cmd_dashboard.go` with
`daemonize.Run(...)`. The Node child is still supervised via
the existing `dashboard.Supervisor` — only the *CLI wrapper*
changes. Default behaviour flips to detached; `--foreground`
restores the current blocking mode.

Smoke test: `dashboard start` returns within 500ms, `dashboard
status` shows `running`, `dashboard stop` cleans up, all existing
Go and Playwright tests stay green. Playwright specs that spawn
`dashboard start` in `global-setup` use `--foreground` (they
already supervise the process via the Node test-runner).

### Commit D.3 — Split `start` into `orchestrator start`

Move the current `runStart()` body to `runOrchestratorStart()`
under a new `orchestrator` cobra command. Wrap in
`daemonize.Run(...)` with `PIDPath = ~/.maquinista/orchestrator.pid`
and `LogPath = ~/.maquinista/logs/orchestrator.log`. Add
symmetric `orchestrator stop / status / logs`.

Backwards-compatibility shim: keep a top-level `start` that
delegates to `orchestrator start` **temporarily** so old scripts
still work, but print a deprecation line ("calling through
back-compat shim; use `maquinista start` for the full stack or
`maquinista orchestrator start` for just the bot") — D.4 removes
the shim when the top-level `start` gains its new meaning.

### Commit D.4 — Top-level `start` / `stop` / `status`

`runStart()` becomes an orchestrator + dashboard bootstrap:
call `orchestrator.Start()` (which now detaches), then
`dashboard.Start()`, then print a two-line summary with both
PIDs. If either half fails, stop the other so we never leave a
half-started stack.

`runStop()` calls both `Stop()` methods in the safe order:
dashboard first (so the UI stops sending actions), then
orchestrator (which finishes draining inbox). Each stop is
independent — a dead dashboard doesn't prevent orchestrator
shutdown.

`runStatus()` prints a small table:

```
Component      PID     State     Log
orchestrator   12345   running   ~/.maquinista/logs/orchestrator.log
dashboard      12389   running   ~/.maquinista/logs/dashboard.log
```

Also reads the legacy `~/.maquinista/maquinista.pid` and, if it
points to a live process, prints a migration note + kills it as
part of `stop`. Removed in the release after this one.

### Commit D.5 — `maquinista logs` command

New top-level `maquinista logs [-f|--follow] [--component
orchestrator|dashboard]`.

- Without `--component`, tails both files interleaved. Each line
  is prefixed with a short tag (`[orch]` / `[dash]`) and a
  wall-clock timestamp so the operator can disentangle them.
- With `--component`, delegates to the component-specific
  `daemonize.TailLogs()` (no prefix rewriting — full fidelity).
- `-f` follows both files; Ctrl-C cleanly stops both follows.

Implementation: two goroutines, one per file, writing into a
shared `chan string`; the main goroutine drains the channel,
applies the prefix, and writes to stdout. File-truncation
detection reuses the helper from D.1.

Test: Go integration test writes to both log files, launches
`maquinista logs`, asserts interleaved lines appear with correct
prefixes in the order they were written.

## Migration notes

- `~/.maquinista/maquinista.pid` → `~/.maquinista/orchestrator.pid`.
  The new `stop` checks both paths for one release and removes
  the legacy file after a successful kill.
- Any shell script that calls `./maquinista start` expecting it
  to block should add `--foreground`. Release notes call this
  out; the deprecated blocking behaviour is still available.
- The Playwright `global-setup` that launches the dashboard
  must pass `--foreground` because the test runner owns the
  supervision lifecycle. Commit D.2 updates it.

## Test matrix

Each commit ships with:

1. Go unit tests for the helper it introduces (`daemonize` in
   D.1; updated start / stop assertions in D.2–D.4).
2. Go integration tests that spawn the real binary on an
   ephemeral Postgres and assert: detach returns fast, PID file
   is written, log file receives output, stop is graceful,
   status is accurate.
3. A golden-file test for the `status` table format so it
   doesn't drift silently.

No Playwright changes beyond the `--foreground` tweak to
`global-setup` — the dashboard UI is unchanged.

## Rollout

Commits D.1 → D.5 land in order; each one leaves the tree green.
D.2 is the earliest visibly-different commit (dashboard start
returns immediately). D.4 is the user-visible win (`maquinista
start` launches everything). D.5 is the debugging quality-of-
life improvement and can ship independently of operator habit
changes.
