# EX-06 Telegram plumbing — checks

Run: `make vet && make test` (db-backed pipeline tests need `make up`).

## Migration 036

- [ ] `pipeline` agent row exists: role `notifier`, status `stopped`,
      handle `Pipeline`; idempotent seed (`ON CONFLICT DO NOTHING`).
- [ ] Partial unique index on `merge_queue(task_id) WHERE status IN
      ('pending','merging')` exists.
- [ ] `merge_queue.attempts INT NOT NULL DEFAULT 0` exists.
- [ ] Re-run migrate → no errors (idempotent).

## db helpers

- [ ] `EnqueueMerge` twice for one task → one pending entry, no error.
- [ ] `ReleaseMergeEntry` on a `merged` entry → row stays `merged`.
- [ ] `BumpMergeAttempts` increments and returns 1, 2, …

## Notify path

- [ ] `Notify` inserts an `agent_outbox` row for `pipeline` whose
      `content->>'text'` equals the message.
- [ ] `notifyf` on a broken pool logs and returns, does not panic.
- [ ] Relay delivers the row to the Pipeline topic binding
      (`channel_deliveries` row appears after relay tick — manual check
      against a live stack, covered structurally by fanout tests).

## Emission points

- [ ] approve verdict → outbox row containing `ready_to_merge` and the
      approve verb hint.
- [ ] request_changes verdict → outbox row naming the round + fixer.
- [ ] needs_human verdict / round cap → outbox row (question).
- [ ] watchdog park → outbox row with the stall note.
- [ ] gh merge success → outbox row with PR number, base, SHA.
- [ ] rebase conflict → outbox row listing conflict files.
- [ ] CI failed at cap → entry `failed`, task `pending_approval`, outbox
      row; below cap → entry released, **no** outbox row.
- [ ] infra failMerge → outbox row (warning).
- [ ] Every emission is once per transition (repeat pass → no duplicates).

## Env & nits

- [ ] `PIPELINE_AUTO_MERGE` accepts `1/true/True/TRUE/yes/y` (and rejects
      junk → false).
- [ ] `MAQUINISTA_MERGE_ATTEMPTS_MAX` parsed, default 5.
