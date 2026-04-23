# Plan: Object Storage (R2/S3) Integration via `firetiger-oss/storage`

> This is an **exploration document** — not a committed implementation plan.  
> It surveys ideas, evolution paths, and trade-offs. Sections marked ⚡ are
> high-leverage; sections marked 🔭 are speculative or longer-horizon.

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres is
> the system of record.** Object storage is a complement, not a replacement —
> metadata, indexing, and task state stay in DB; blobs, logs, and large
> artifacts move to buckets.

---

## Context

Maquinista today runs agents on a single host machine using tmux, with all
state split between PostgreSQL and the local `~/.maquinista/` directory.  
The `sandbox-deployment.md` plan introduces Kubernetes + Kata Containers as
the execution backend. Once agents run in ephemeral pods on multiple nodes,
the local filesystem assumptions break:

- `monitor_state.json` (transcript byte offsets) is per-host
- `worktrees/agent/<id>/` git checkouts live on the control-plane host
- dashboard `standalone.tgz` is extracted into a local path
- agent logs and LLM turn transcripts have no off-host home

The [`firetiger-oss/storage`](https://github.com/otaviocarvalho/storage) Go
library provides a unified `Bucket` interface across S3, R2, GCS, local
filesystem, in-memory, and HTTP backends — with presigned URLs, LRU caching,
OpenTelemetry instrumentation, and an adapter/middleware system. This makes it
a natural fit for replacing the ad-hoc local-file patterns throughout the
codebase.

Cloudflare R2 is the primary target for self-hosted deployment (S3-compatible,
no egress fees, zero-config with Cloudflare Workers if needed). AWS S3 drops
in via the same interface.

---

## The Storage Library at a Glance

```go
// import side-effect registers the r2:// scheme
import _ "github.com/firetiger-oss/storage/r2"

bucket, err := storage.LoadBucket(ctx, "r2://maquinista-agents")

// read
rc, info, err := bucket.GetObject(ctx, "transcripts/agent-42/turn-7.jsonl")

// write (streaming)
w := storage.PutObjectWriter(ctx, "r2://maquinista-agents/transcripts/agent-42/turn-7.jsonl")
io.Copy(w, source)
w.Close()

// list (Go 1.23 iter.Seq2)
for obj, err := range bucket.ListObjects(ctx, storage.KeyPrefix("transcripts/agent-42/")) { ... }

// presigned URL valid for 15 min
url, err := bucket.PresignGetObject(ctx, key, 15*time.Minute)
```

Key adapters composable via `AdaptBucket()`:
- `WithPrefix(p)` — namespace a bucket at a key prefix
- `NewCache()` — in-process LRU with TTL
- `WithInstrumentation()` — OpenTelemetry spans
- `ReadOnly()` — reject writes on a sub-bucket view

The `file://` backend lets local dev use the real filesystem with the same
interface, so no mocking is needed in tests.

---

## Ideas & Evolution Paths

### 1. ⚡ Transcript & Log Storage

**Current state**: The monitor polls tmux output, keeps byte-offset state in
`monitor_state.json`, and stores turn summaries in the DB. Full raw transcripts
are not persisted anywhere — they live in the tmux scrollback buffer until the
window closes.

**With object storage**:
- The per-agent tailer (sidecar) streams raw PTY output to
  `transcripts/agent-<id>/session-<ts>.jsonl` in real time.
- `monitor_state.json` becomes a DB column `transcript_sources.byte_offset`
  (already planned), but the transcript file itself lives in the bucket.
- The dashboard can fetch the last N turns by listing the prefix, reading only
  the tail of the latest file, without pulling the whole log into memory.
- Logs older than N days are automatically transitioned to cheaper storage
  tiers (R2 has no lifecycle rules yet, but S3/GCS do).

**Key prefix layout**:
```
transcripts/
  agent-<id>/
    session-<unix-ts>.jsonl     # raw PTY events
    turns/
      <turn-id>.json            # structured turn metadata
logs/
  orchestrator-<date>.log
  dashboard-<date>.log
```

**Evolution path**:
- Phase A: write tailer output to bucket in parallel with existing DB inserts
- Phase B: read transcript replays from bucket instead of tmux scrollback
- Phase C: deprecate `agent_outbox` body column; store only a bucket key + byte range

---

### 2. ⚡ Worktree Snapshotting (Stateless Agent Pods)

**Current state**: Agent worktrees live at
`~/.maquinista/worktrees/agent/<id>/` as real git checkouts. In Kubernetes,
the orchestrator pod would need a shared PVC accessible by all agent pods — a
read-write-many volume, which Hetzner CSI doesn't support natively.

**With object storage**:
Each agent pod can be fully stateless by treating the bucket as the canonical
source of worktree state:

- On task claim, the orchestrator writes a `worktrees/agent-<id>/init.tar.gz`
  snapshot of the baseline repo state to the bucket.
- The agent pod init container pulls and extracts that snapshot.
- On agent completion or periodic checkpoint, the pod writes a diff archive
  back: `worktrees/agent-<id>/checkpoints/<ts>.patch`.
- On resume, a new pod reconstructs state by applying patches in order.

This keeps every agent pod stateless and restartable, at the cost of
snapshot/restore latency (~1–2 s for a 50 MB checkout). Large binary assets
can be excluded via `.gitattributes`.

**Alternative (lighter)**: skip the snapshot and just `git clone` the repo
inside the pod from the origin remote. The bucket only stores the working
changes as a patch. This works when agents always start from HEAD.

**Evolution path**:
- Phase A: `checkpoint-rollback.md` + `sandbox-deployment.md` provide the
  triggering events; wire `storage.PutObject` for checkpoint writes
- Phase B: introduce pod init container that pulls + extracts snapshots
- Phase C: evaluate latency vs NFS PVC approach; choose winner

---

### 3. ⚡ Dashboard Binary Distribution

**Current state**: The dashboard `standalone.tgz` (19 MB) is embedded in the
Go binary via `go:embed` and extracted to `~/.maquinista/dashboard/<version>`
on start. This bloats the binary and forces a full restart to upgrade the
dashboard independently of the orchestrator.

**With object storage**:
- Publish `dashboard-<version>.tgz` to `r2://maquinista-releases/dashboard/`
  as part of CI.
- On start, the supervisor checks the current version marker, downloads and
  extracts the matching artifact if absent, then launches Next.js.
- Version pinning stays trivial (read a `latest` pointer object), but hotfixes
  can be deployed without rebuilding the Go binary.
- The `go:embed` blob disappears from the binary, shaving ~20 MB from the
  container image.

**Evolution path**:
- Phase A: publish CI artifacts to bucket; supervisor fetches on demand
- Phase B: split dashboard into its own Kubernetes Deployment with an init
  container that pulls the bundle, so it can be rolled independently
- Phase C: serve the bundle directly from R2 via Cloudflare Workers (no Next.js
  process needed for static assets)

---

### 4. ⚡ Agent Memory & Soul Persistence

**Current state**: `agent_souls` in Postgres stores agent personality templates.
`internal/memory/` stores agent memories in DB rows. Both are text-heavy and
can grow unboundedly as agents accumulate context.

**With object storage**:
- Large memory blocks (e.g., full session summaries, code snapshots referenced
  in memory) move to `memory/agent-<id>/<block-id>.md` in the bucket.
- The DB row keeps only the key, creation time, and a short excerpt for
  searching/listing.
- Presigned GET URLs serve memory blobs to the dashboard without proxying
  through the API server.
- Soul templates (system prompts, personas) versioned in
  `souls/<name>/v<n>.md`; agents always resolve `souls/<name>/latest`.

**Evolution path**:
- Phase A: add a `bucket_key` nullable column to `agent_memories`; new memories
  above a threshold size write to bucket instead of inline
- Phase B: migrate existing large rows to bucket; inline storage becomes the
  exception for small entries
- Phase C: memory search uses bucket listing + metadata filtering rather than
  `LIKE` queries

---

### 5. 🔭 Task Artifacts & Output Files

Agents often produce files: code, reports, diagrams, test results. Today these
sit in the worktree on disk and are effectively invisible to the dashboard or
other agents unless they read the filesystem.

**With object storage**:
- When an agent calls `maquinista-done`, a post-completion hook walks a
  configurable output glob (e.g., `dist/**`, `*.report.md`) and uploads
  matching files to `artifacts/task-<id>/`.
- The `tasks` table gains an `artifact_prefix` column pointing to the bucket
  path.
- Dashboard shows a file tree for each completed task; files served via
  presigned URLs (no API proxy needed).
- Agent-to-agent handoff: a downstream agent receives the artifact prefix in
  its task context and can read files without accessing the upstream agent's
  worktree.

**Key presigned URL pattern**:
```go
url, _ := bucket.PresignGetObject(ctx, "artifacts/task-42/report.md", 1*time.Hour)
// embed in Telegram message or dashboard card
```

---

### 6. 🔭 Structured LLM Cost & Token Records

**Current state**: `agent_turn_costs` in Postgres stores per-turn token usage.
The rows are small but accumulate fast under heavy use and are rarely queried
beyond aggregates.

**With object storage**:
- Raw turn payloads (full request + response JSON) written to
  `turns/agent-<id>/<turn-id>.json` for auditing and replay.
- The DB row retains only the summary (token counts, model, cost, timestamp).
- Batch export: a nightly job lists all turn objects for the day and writes a
  `billing/<date>/summary.jsonl` for external ingestion.

---

### 7. 🔭 Agent Image Caching (Container Layers)

In the Kubernetes path, each agent pod pulls a base container image (~500 MB
for a full Claude Code environment). On cold start, image pull time dominates
boot latency.

**With object storage + Kata**:
- Pre-pull base images and store squashfs snapshots at
  `images/claude-code/<digest>.squashfs` in the bucket.
- Kata Containers can boot directly from a squashfs layer fetched from an
  OCI-compatible registry or custom HTTP source.
- Per-agent COW layers are sparse diffs stored as
  `images/agent-<id>/overlay.ext4`.
- This mirrors what E2B does internally with overlayfs on Firecracker.

This is deep infrastructure work, only worth pursuing after the basic Kata
integration is stable.

---

### 8. 🔭 Multi-Region & Edge Distribution

The `storage` library's `Merge` and `Overlay` adapters enable geo-distribution
patterns:

```go
primary   := r2.NewBucket(...)  // us-east-1 bucket
secondary := r2.NewBucket(...)  // eu-central-1 bucket
merged    := storage.AdaptBucket(primary, storage.WithOverlay(secondary))
```

- Read from whichever region responds fastest (merge adapter does k-way merge
  on list; get from primary, fall back to secondary).
- Write always to primary; secondary replication handled by R2 or a background
  sync job.
- Relevant once maquinista serves teams in multiple geographic regions or needs
  data residency compliance.

---

## Integration Design

### Go Module Addition

```
go get github.com/firetiger-oss/storage@latest
go get github.com/firetiger-oss/storage/r2@latest
go get github.com/firetiger-oss/storage/file@latest
```

### Configuration

```
# .env additions
MAQUINISTA_STORAGE_URI=r2://maquinista-agents      # production
# MAQUINISTA_STORAGE_URI=file://~/.maquinista/blobs # local dev
CF_ACCOUNT_ID=<cloudflare-account-id>               # R2 only
```

A single `storage.LoadBucket(ctx, uri)` call at startup produces the bucket
used everywhere. Adapters stack on top:

```go
bucket, err := storage.LoadBucket(ctx, cfg.StorageURI)
bucket = storage.AdaptBucket(bucket,
    storage.WithPrefix("maquinista/"),
    storage.NewCache(storage.CachePageSize(512*1024)),
    storage.WithInstrumentation(),   // OTel spans for all ops
)
```

The `file://` backend in local dev means no R2 account is needed to run
maquinista, and tests use `:memory:` with no filesystem side effects.

### Key Naming Conventions

| Prefix | Contents |
|--------|----------|
| `transcripts/agent-<id>/` | Raw PTY session files, turn JSONL |
| `logs/<component>/<date>/` | Structured daemon logs |
| `worktrees/agent-<id>/` | Checkout snapshots, checkpoint patches |
| `artifacts/task-<id>/` | Agent output files |
| `memory/agent-<id>/` | Large memory blobs |
| `souls/<name>/` | Soul template versions |
| `releases/dashboard/` | Versioned Next.js bundles |
| `turns/agent-<id>/` | Full LLM request/response payloads |
| `billing/<date>/` | Daily cost summaries |

---

## Dependency on Other Active Plans

| Plan | Relationship |
|------|--------------|
| `sandbox-deployment.md` | Storage integration is a prerequisite for stateless agent pods; phases should align |
| `retire-legacy-tmux-paths.md` | Completing the sidecar migration enables the tailer to write transcripts to a bucket |
| `checkpoint-rollback.md` | Checkpoint snapshots are the primary write target; bucket is the natural store |
| `dashboard-cost-sse.md` | Turn cost NOTIFY triggers could emit bucket keys instead of inline payloads in Phase C |
| `agent-memory-db.md` | Memory persistence in DB is Phase A; bucket offload is Phase B of idea #4 above |

---

## Open Questions

1. **R2 or S3?** R2 has no egress fees (good for transcript replays in the
   dashboard), but S3 has lifecycle policies (auto-tier old logs to Glacier).
   Can start with R2 and migrate if archival cost becomes a concern.

2. **Bucket layout: one bucket or many?** A single `maquinista-agents` bucket
   with key prefixes is simpler to manage. Separate buckets per concern
   (transcripts, artifacts, releases) allow per-bucket ACLs and lifecycle
   policies independently. Lean toward one bucket + prefix adapter during early
   phases; split later if ACL requirements emerge.

3. **Presigned URL TTL and security**: presigned URLs served to dashboard users
   bypass the API server auth. A short TTL (15 min) mitigates leakage but
   requires the dashboard to refresh them. Consider whether a thin proxy is
   needed for sensitive artifacts (e.g., soul templates, memory).

4. **Write-ahead vs. write-on-completion for transcripts**: streaming writes
   during agent turns give the best observability but require the tailer to
   handle partial-write recovery. A simpler first pass writes complete turns
   after the turn boundary is detected.

5. **Go version constraint**: the `storage` library uses `iter.Seq2` (Go 1.23+)
   for `ListObjects` and `DeleteObjects`. Verify `go.mod` `go` directive before
   adding the dependency.
