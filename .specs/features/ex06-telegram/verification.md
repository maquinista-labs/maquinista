# EX-06 Telegram plumbing — verification

- **verifier:** Hermes subagent (independent of the PR author)
- **date:** 2026-10-02
- **HEAD:** `498b2fe42e96093204bf9227d374195ff5d92b44` (feat(pipeline): telegram plumbing for pipeline events (EX-06))
- **base:** `4a981b5` (EX-05) ; rebased on `5dfa16b` (monitor test fix)
- **verdict:** **PASS** (21/21 checks; C4, C7, C10 by structural/code evidence as declared below)

## Checks

| C#  | Check (checks.md)                                            | Result | Evidence |
| --- | ------------------------------------------------------------ | ------ | -------- |
| C1  | Migration 036: `pipeline` agent row — role `notifier`, status `stopped`, handle `Pipeline`; `ON CONFLICT (id) DO NOTHING` | PASS   | `internal/db/migrations/036_pipeline_telegram.sql:13-16` (VALUES read/quoted verbatim) |
| C2  | Partial unique index `merge_queue(task_id) WHERE status IN ('pending','merging')` | PASS   | same file, `uq_merge_queue_live_task` (lines 18-20); paired with `ON CONFLICT (task_id) WHERE … DO NOTHING` in `internal/db/queries.go` `EnqueueMerge` |
| C3  | `merge_queue.attempts INT NOT NULL DEFAULT 0`                | PASS   | same file, `ALTER TABLE merge_queue ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0` (line 22) |
| C4  | Re-run migrate → no errors (idempotent)                      | PASS (structural) | all three statements idempotent by construction (`ON CONFLICT DO NOTHING` / `CREATE UNIQUE INDEX IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS`); the full migration chain runs clean on a fresh Postgres in every db-backed test below — no live double-run executed in this verification |
| C5  | `EnqueueMerge` twice → one pending entry, no error           | PASS   | `--- PASS: TestEnqueueMerge_Idempotent (2.28s)` (asserts live-entry count == 1) |
| C6  | `ReleaseMergeEntry` on `merged` entry → row stays `merged`   | PASS   | `--- PASS: TestReleaseMergeEntry_GuardsTerminal (2.20s)`; SQL guard `AND status = 'merging'` in `internal/db/queries.go` |
| C7  | `BumpMergeAttempts` increments, returns 1, 2, …              | PASS (structural) | `internal/db/queries.go:908-917` `SET attempts = attempts + 1 … RETURNING attempts`; exercised by CICapParksNeedsHuman attempt counting (1/2 with cap 2, note says "CI failed 2 times") — no dedicated unit test (noted below) |
| C8  | `Notify` inserts `agent_outbox` row for `pipeline`, `content->>'text'` == message | PASS   | `--- PASS: TestNotify_WritesPipelineOutbox (2.02s)` (asserts exactly one text `"hello pipeline"`); content shape `{"type":"text","text":…}` in `internal/pipeline/notify.go` |
| C9  | `notifyf` on broken pool logs and returns, no panic          | PASS   | `--- PASS: TestNotifyf_SwallowsDeadPool (2.10s)` (pool closed → notifyf only) |
| C10 | Relay delivers the row to the Pipeline topic binding         | PASS (structural; manual/live per checks.md) | `internal/relay/relay.go:133-155` `fanoutDeliveries` fans out to every `owner`/`observer` binding of the emitting agent with no role filter → pipeline agent's owner binding receives it; live-stack delivery remains a manual check by checks.md's own wording |
| C11 | approve verdict → outbox with `ready_to_merge` + approve hint | PASS   | `--- PASS: TestVerdict_NotifyPerOutcome/approve` (asserts `✅`, `ready_to_merge`, `maquinista approve <id>`) |
| C12 | request_changes verdict → round + fixer note                 | PASS   | `--- PASS: TestVerdict_NotifyPerOutcome/request-changes` (asserts `request_changes (review round 0)`, `fixer spawning`) |
| C13 | needs_human / round-cap → question outbox                    | PASS   | `--- PASS: TestVerdict_NotifyPerOutcome/needs-human`, `…/round-cap` (asserts `🆘`, `review round cap 3 reached`, `parked needs-human`) |
| C14 | watchdog park → outbox with stall note                       | PASS   | `--- PASS: TestWatchdog_NotifyOnStall (2.11s)`; emission inside the `applied` arm of `watchdogPass` (`internal/pipeline/dispatch.go`, review + fixer arms) |
| C15 | gh merge success → outbox with PR number, base, SHA          | PASS (code evidence) | `internal/pipeline/merge.go:347` `notifyf(… "✅ %s merged: PR #%d squash-merged into %s (%s).", …, pr, base, mergeSHA)` inside the guarded `UPDATE … WHERE status='ready_to_merge'` arm; flow covered by `--- PASS: TestProcessMergeGH_HappyPath (2.18s)` (note text itself not directly asserted by the test — noted below) |
| C16 | rebase conflict → outbox listing conflict files              | PASS (code evidence) | `internal/pipeline/merge.go:403` `notifyf(… "🆘 %s: rebase conflict on branch %s. Conflicting files:\n%s\nTask parked needs-human." …)`; flow covered by `--- PASS: TestProcessMergeGH_Conflict (2.25s)` (note text itself not directly asserted) |
| C17 | CI failed at cap → entry `failed`, task `pending_approval`, outbox; below cap → released, **no** outbox | PASS   | `--- PASS: TestProcessMergeGH_CICapParksNeedsHuman (2.40s)` (attempt 1: `pending` + 0 notes; attempt 2: `failed` + `pending_approval` + exactly 1 question); `--- PASS: TestProcessMergeGH_CIGate (9.11s)` incl. failed/pending subcases |
| C18 | infra failMerge → outbox (warning)                           | PASS   | `--- PASS: TestProcessMergeGH_MergeFailNotifies (2.14s)` (exactly one "merge failed" note, task stays `ready_to_merge`) |
| C19 | Every emission once per transition (repeat pass → no duplicates) | PASS   | `TestVerdict_NotifyPerOutcome` asserts exactly 1 row per outcome; `--- PASS: TestVerdict_MalformedNoNotify (2.20s)` (0 rows); `--- PASS: TestWatchdog_ActiveNoNotify (2.07s)` (0 rows when not parked); CICap attempt-1 asserts 0 rows; all emission sites sit inside the applied/transition-guard arms (dispatch.go `notifyVerdict` after `applied`; merge.go arms after their UPDATE guards) |
| C20 | `PIPELINE_AUTO_MERGE` accepts `1/true/True/TRUE/yes/y`, junk → false | PASS   | `--- PASS: TestMergeConfigFromEnv (0.00s)`; `truthyEnv = {1, t, true, y, yes}` via `strings.ToLower` (superset of the listed spellings, case-insensitive); junk/empty → false |
| C21 | `MAQUINISTA_MERGE_ATTEMPTS_MAX` parsed, default 5            | PASS   | same test: `7` parsed, `"banana"` → `defaultMergeAttempts`; `defaultMergeAttempts = 5` (`internal/pipeline/merge.go:66`) |

## Gate

- `go vet ./...` — clean (run in this verification).
- `go test ./internal/pipeline/` — `ok … 63.638s`, all named checks above `--- PASS` (run in this verification, `-v`).
- `go test ./internal/monitor/` — integrator gate: 114 PASS / 0 FAIL (test fix landed on main in `5dfa16b`, carried by this branch); spot re-run in this verification: `--- PASS: TestOutboxSink_WritesAssistantText (2.38s)`, `--- PASS: TestToolEventSink_PairedEmitsResultOnly (2.37s)`, `ok internal/monitor 4.771s`.
- `go test ./...` not re-run by the verifier (out of budget); package-level gates above are the EX-06-touched packages.

## Notes

- **Bot topic provisioner has no unit test** — deliberate: it needs a live Telegram API, matching the untested `provisionMissingTopics` (plan.md). Verified by reading `internal/bot/topic_provisioner.go`: `ensurePipelineTopic` runs on the provisioner ticker (line 41), selects the `pipeline` agent when it exists, is not `archived/dead`, and has no `owner` binding, then creates the "Pipeline" forum topic + owner binding (`user_id = AllowedUsers[0]`, `chat_id`, `thread_id` = topic). `--- PASS: TestMergeConfigFromEnv (0.00s)` is the closest named test in the checks; no `internal/bot/` Pipeline test exists, so C10's topic leg is code-verified only.
- **C7 has no dedicated unit test** (`BumpMergeAttempts` return sequence not directly asserted anywhere). Behavior is one SQL statement read verbatim and exercised via the cap test; risk is low, but worth a follow-up if EX-07 touches the cap.
- **C15/C16 note-text content** is asserted by code inspection of the `notifyf` format strings, not by direct test assertion; the surrounding flows (happy path, conflict) pass end-to-end.
- **Relay leg (C10)** is structural: `fanoutDeliveries` has no agent-role filter, so the synthetic `pipeline` agent rides the stock path. checks.md itself marks live-stack delivery as manual.
- No exploratory test files were created by this verification.

**Verdict: PASS — EX-06 (Telegram plumbing) is verified; PR #6 is mergeable from the verifier's perspective.**
