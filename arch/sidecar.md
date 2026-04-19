# Sidecar (per-agent supervisor)

## Goal

Replace the current shared mailbox consumer + monitor poll loop with one
`SidecarRunner` goroutine per live agent. Each sidecar owns:

1. **Inbox drive loop** — listens on `NOTIFY agent_inbox_new`, claims rows
   for its agent, pipes text into the PTY via `PtyDriver`.
2. **Transcript tail** — streams transcript events via `TranscriptTailer`,
   appends `agent_outbox` rows.

Because both concerns live in the same goroutine, `in_reply_to` is
causally correct: the sidecar records the inbox row id before driving,
and every outbox row produced during that turn carries it.

## Current state (transitional)

The sidecar package (`internal/sidecar/sidecar.go`) is complete and
tested. It is **not yet wired** into the main daemon. The current runtime
uses:

- `runMailboxConsumer` — single global goroutine, handles inbox for all
  agents via `FOR UPDATE SKIP LOCKED`.
- `monitor.OutboxWriter` + `ActiveInboxMap` — best-effort `in_reply_to`
  stamping (timing is approximate because monitor polls asynchronously).

This is the gap `per-agent-sidecar.md` closes.

## SidecarRunner interfaces

```go
type PtyDriver interface {
    Drive(ctx context.Context, text string) error
}

type TranscriptTailer interface {
    Tail(ctx context.Context, ch chan<- TranscriptEvent) error
}
```

The production `PtyDriver` wraps `tmux.SendKeysWithDelay`.
The production `TranscriptTailer` wraps the monitor's transcript source
for the agent's window.

## Migration path

Per `plans/active/per-agent-sidecar.md`:

1. Wire sidecar at reconcile time: when `respawnAgent` creates a tmux
   window, also start a `SidecarRunner` goroutine for that agent.
2. Keep `runMailboxConsumer` alive in parallel during the transition to
   handle agents whose sidecars haven't started yet.
3. Once all agents have sidecars, retire `runMailboxConsumer` and the
   `ActiveInboxMap` shim.
4. Retire the monitor's `OutboxWriter` path (task 1.9).

## Crash recovery

A crashed sidecar leaves inbox rows in `status='processing'`. Lease
expiry (`lease_expires < NOW()`) makes them eligible for reclaim on the
next sidecar restart or mailbox consumer tick. No manual intervention
required.

## TODO

- [ ] Wire sidecar in respawnAgent / runDashboardAgentReconcile
- [ ] PtyDriver implementation wrapping tmux
- [ ] TranscriptTailer implementation per runner type
- [ ] Retire runMailboxConsumer once all agents have sidecars
- [ ] Retire monitor OutboxWriter path (task 1.9)
