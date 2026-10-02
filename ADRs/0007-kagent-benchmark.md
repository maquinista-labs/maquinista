# ADR-0007: kagent benchmark — stay the course, adopt tracing, checkpoint/fork, and temporal verdict guards

- **Status:** Proposto (pending Otavio's ok)
- **Date:** 2026-10-02
- **Deciders:** Otavio
- **Scope:** maquinista runner subsystem, pipeline guard predicates, observability; positions the platform against the Oct-2026 wave of declarative agent infrastructure

## Context

Otavio asked (02/10) for an ADR comparing kagent with maquinista + substrate and
extracting what we should improve. Four sources from the reading log this week:

- **kagent** (`github.com/kagent-dev/kagent`, Solo.io, "Cloud Native Agentic AI", Go,
  Apache-2.0, 3,916★ upstream; README surveyed 02/10 via gh CLI): Kubernetes-native
  framework where agents ARE custom resources — Agent CR (behavior + runtime config),
  AgentTemplate CR (shareable prompt/tools/subagents/LLM config), ModelConfig CR
  (OpenAI/Azure/Anthropic/Vertex/Ollama + gateways), RemoteMCPServer CR (shared MCP
  tools), Session CR with suspend/resume + checkpoint + fork, OTel tracing. Harnesses:
  Go/Python ADKs, Codex, Claude, custom. Bundled MCP servers: Kubernetes, Istio, Helm,
  Argo, Prometheus, Grafana, Cilium. **No isolation story**: agents run as pods on
  whatever cluster; no egress control, no per-tenant namespace design, no environment
  snapshot (README, 02/10).
- **celld** (Deno, 10-02): self-hosted Durable Objects; S3 conditional writes as the
  consensus layer; epoch-fenced single-writer cells with gated acks.
- **Tensorlake** (10-02): sandboxes where storage is the product — versioned FS,
  managed Git, snapshot/suspend/resume, fork-and-fan-out of live VMs.
- **Dogwood** (Brooker, 10-01): temporal policy over agent event logs — look-back
  predicates (`formerly`, `previous`, windows) compiled to Cedar; the request-vs-settled
  "one word is the bug" class.

### Layer map: kagent ↔ maquinista + substrate

| Concern | kagent | maquinista + substrate |
|---|---|---|
| State locus | etcd — agents are CRs, controller reconciles desired state | Postgres is the system of record; tmux holds zero durable state (ADR-0002) |
| Coordination | control-loop reconciliation | pull-based inbox/outbox + dispatcher (`agent_inbox` → sidecar → tmux → `agent_outbox` → relay) |
| Harness abstraction | Harnesses: Go/Python ADKs, Codex, Claude, custom | `AgentRunner` interface (`internal/runner/runner.go:27-58`): claude, openclaude, opencode, pi, custom |
| Role templating | AgentTemplate CRs | role souls seeded in migration 035 (ADR-0005) |
| Model config | ModelConfig CRs | runner extras `default_runner` + `reasoning_class` (migration 035) |
| Session model | Session CR: suspend/resume, checkpoint, fork | transcript files + `session_map` (migration 015), binding v2 (sticky/mtime/duplicate-claim re-pair, `458c6c6`+`cbc7ab4`+`3829740`) |
| Tools | MCP-first, shared RemoteMCPServer resources | the box + harness-native tools |
| Tracing | OTel out of the box | none (`rg -in "otel|opentelemetry" --type go` → 0 hits, 02/10) |
| Isolation | none (pods only) | substrate: microVM/gVisor on the NUC, per-tenant namespaces, default-deny egress, snapshot suspend/resume (ADR-0003/0004, E-02/E-04) |
| Human channel | web UI + API | Telegram-first in-the-loop |

The architecture validates rather than competes: every maquinista seam has a
k8s-native twin, and the two things kagent has that we lack (checkpoint/fork, tracing)
are adoptable without touching our differentiators.

## Options Considered

| Option | Description | Verdict | Effort |
|---|---|---|---|
| A | Migrate maquinista onto kagent | Rejected — loses Telegram-first channel and Postgres SoR; kagent has no isolation so substrate would still be required below it; rewrite cost buys nothing on our differentiators | weeks, negative ROI |
| B | Stay the course; adopt three targeted patterns (tracing, checkpoint/fork, temporal guards) | **Chosen** | 5.5–10 dev days |
| C | Stay the course; adopt nothing | Rejected — tracing and checkpoint/fork fix pain we have already paid for (the 42-loop, binding v2 leak, stale-binding repair were all debugged by journalctl archaeology; review rounds re-mint episodes instead of branching) | 0 d, recurring cost stays |

## Decision

Option B. Maquinista's architecture stands (Postgres SoR, pull-based pipeline, tmux
default executor, substrate isolation per ADR-0002/0003/0004). We adopt three patterns
the kagent/celld/Tensorlake/Dogwood wave proves out, in falsifier-first order:

### S-07a — Session-portability spike (falsifier, 0.5–1 d)

**The load-bearing assumption to falsify:** a copied harness session file, resumed in a
fresh pane, yields a coherent continuation (not a degenerate replay like the 42-loop).
Spike: copy a live pi session file mid-task → spawn a fresh pane resuming from the
copy → verify the model continues coherently. Repeat once for claude JSONL. If pi
resumes degenerately, CK-01 dies here and we keep re-minting.

### OT-01 — OTel spans at the relay seams (1–2 d)

OTel Go SDK with an OTLP-file/stdout exporter on barceloneta — no collector daemon day
one. Spans exactly where the sagas lived: monitor poll → binding resolve (per pass of
`resolvePiBindings`) → sink emit → relay fanout → dispatcher delivery. Span attrs:
window, session_id, offset, transcript bytes. A trace replaces the journalctl
archaeology that cost ~1h+ per incident on 30/09–01/10.

### CK-01 — Checkpoint/fork runner extension (3–5 d, gated on S-07a)

Optional `ForkSession` method on `AgentRunner` (runners without it report fork
unsupported — the port grows only when a second real impl demands, ADR-0002 rule).
TranscriptSource-side: pi session file copied to a fresh uuid + binding v2 epoch seed;
claude JSONL copied likewise. Pipeline verb: fork a task from a verdict episode → new
task with `task_context` copy and `review_rounds` preserved, fresh agent row. This
replaces the re-mint pattern (`recordReviewRound`, `internal/pipeline/dispatch.go:375-392`)
with branching where branching is what we mean.

### DG-01 — Temporal verdict guards (1–2 d)

Dogwood's approve-before-act and request-vs-settled distinction as predicates INSIDE
the existing guarded transitions (we already do guarded `UPDATE … WHERE status='review'`,
`internal/pipeline/dispatch.go:257`):

- Merge path requires a `VERDICT: approve` row for episode N that is (a) newer than any
  `request_changes`/`needs_human` for the same task AND (b) within
  `MAQUINISTA_REVIEW_STALE_MAX` (new env, default 2h — mirrors the review timeout at
  `dispatch.go:102`). Presence checks become recency + ordering checks.
- Fixer mint keeps the episode-key dedup (`external_msg_id = 'review:' || task ||
  ':' || review_rounds`, `dispatch.go:441`) — unchanged.

No new table: predicates query existing rows. `task_events` is a NOTIFY channel
(`migrations/004_notify_triggers.sql`), not a queryable log — we do not build one yet.

### Non-adoptions (each with the why)

- **Declarative CR/agent templates:** role souls in migration 035 already template
  roles; externalizing them is ceremony without a second consumer (YAGNI guard).
- **MCP tool catalog:** our tool surface is the box + harness-native tools; formal
  tool-sharing solves multi-agent reuse we don't have yet.
- **celld as a state layer:** Postgres SoR already eliminates the JSON-file bug class
  celld makes structurally impossible; noted as the escape hatch for DO-shaped
  multi-writer state on our k3s (reader note 10-02), not adopted.
- **Tensorlake fork-and-fan-out:** batch "fix X for all" scaling belongs to substrate
  scale-out (ADR-0004's Robot trigger), not the pipeline.
- **Full Dogwood policy language:** three temporal predicates cover the pipeline's
  actual incident surface; a policy DSL is overkill until guards multiply.

## Effort Estimate

| ID | Task | Effort |
|---|---|---|
| S-07a | Session-portability spike (falsifier first) | 0.5–1 d |
| OT-01 | OTel spans at relay seams | 1–2 d |
| CK-01 | Checkpoint/fork runner extension + pipeline verb | 3–5 d |
| DG-01 | Temporal verdict guards | 1–2 d |
| | **Total** | **5.5–10 dev days** |

Sequencing: S-07a → (CK-01) and OT-01/DG-01 independently. Worker sandboxing and the
claimed-task reaper stay EX-08 candidates under ADR-0003/0004 — the kagent comparison
(kagent pods at least lack sudo; our panes run as the service user) only reinforces
those, it does not reopen them.

## Revisit Triggers

- kagent ships an isolation/sandbox story or acquires a substrate-style executor →
  re-evaluate Option A seriously.
- kagent's harness layer matures faster than CK-01/OT-01 land (Solo.io strategic play,
  fast-moving) → adopt their checkpoint/fork + tracing instead of ours.
- Temporal guards multiply past ~5 predicates → revisit a Dogwood-style policy layer.
- Substrate CNCF momentum (donated 09-30, reader note) produces an upstream
  kagent↔substrate integration → the comparison collapses into "use theirs," re-open.

## References

- kagent README + core concepts — `github.com/kagent-dev/kagent`, fetched via gh CLI 02/10/2026
- Reader note: [kagent — maquinista's mirror, missing the substrate](https://github.com/otaviocarvalho/obsidian-otavio/blob/main/wiki/Readings/2026/2026-10%20October.md), 02/10/2026
- Reader notes: celld (10-02), Tensorlake (10-02), Dogwood (10-01) — same month file
- `internal/runner/runner.go:27-58` (AgentRunner), `internal/pipeline/dispatch.go:102,257,375-392,425,441`, `internal/db/migrations/004_notify_triggers.sql` (task_events NOTIFY), `internal/db/migrations/015_agents_session_fields.sql` (session_map) — rg'd 02/10/2026
- ADR-0002 (executor port), ADR-0003 (substrate multi-tenant), ADR-0004 (barceloneta NUC), ADR-0005 (pipeline), ADR-0006 (ticket abstraction)
