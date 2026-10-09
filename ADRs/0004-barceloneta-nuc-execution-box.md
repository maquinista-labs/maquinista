# ADR-0004: Barceloneta NUC as the Execution Box — Owned Silicon First, Robot as Scale-Out

- **Status:** Aceito (2026-10-09, Otavio — de facto live: maquinista runs and deploys on barceloneta since early Oct 2026)
- **Date:** 2026-10-01
- **Amended:** 2026-10-02 — Phase 1/2: sandbox technology becomes a config knob
  (template indirection), not a promotion event (Otavio, live discussion)
- **Deciders:** Otavio
- **Scope:** execution infrastructure locus (existing home NUC vs. new rental), Substrate sandbox technology choice (gVisor vs. microVM), ate-env integration surface
- **Depends on:** ADR-0002 (`Executor` port), ADR-0003 (Substrate/AX integration plan — this ADR revises its hardware decision, not its adapter/tenant design)
- **Partially supersedes:** ADR-0003 §Decision (the "dedicated Hetzner Robot box" choice); everything else in 0003 carries over

## Context

ADR-0003 (2026-09-21) chose a rented Hetzner Robot bare-metal box as the
Substrate execution host, on the verified premise that neither playa nor
Hetzner Cloud expose `/dev/kvm`, locking out Substrate's microVM mode. That
premise never covered barceloneta — Otavio's always-on home server — which
was verified **2026-10-01, by direct inspection**:

- **Bare metal**: `systemd-detect-virt` → `none`; `/dev/kvm` present
  (`crw-rw----+ root:kvm`); VT-x flags on every core. KVM is real here.
- **Capacity**: Intel Core 3 100U (6C/8T, 15 W U-series), 16 GB RAM
  (~12 GB available), 931 GB NVMe as plain ext4 partitions (no LVM):
  `/` 57 GB (43 GB free), `/var` 22 GB, swap 16 GB, `/home` 834 GB
  (**808 GB available** — Proxmox `home` dir storage, holds the
  ct100–102 disks). Earlier draft quoted the root partition only
  ("56 GB NVMe with 43 GB free"); corrected by direct inspection
  2026-10-01 (`lsblk` + `pvesm status`).
- **Existing load**: maquinista daemon + tmux agents (the workload this ADR
  would isolate) and Proxmox LXC containers ct100–ct102 (pihole, navidrome,
  jellyfin) rooted on the same host.
- **Network**: home LAN + Tailscale (100.74.121.4); playa reachable over TS.

Two upstream facts change the software picture since 0003 was written:

1. **`agent-substrate/env` (ate-env) now exists** — an environment service on
   top of Substrate: each environment is a Substrate *actor* running a guest
   daemon (`ate-env-guest`) exposing **command execution, filesystem
   operations, and built-in MCP tools**, driven by `ate-env` CLI or Go/Python
   clients through `ate-env-api`. Verified by shallow clone 2026-10-01.
   Marked **alpha API**. This is directly relevant to ADR-0003's biggest
   open unknown **S-00b (transcript egress)**: fs operations over the env
   API are a candidate mechanism for shipping JSONL transcripts out of the
   sandbox without host mounts — it converts S-00b from "unknown" to
   "must be measured" (latency, auth, stream-vs-poll).
2. **Substrate's microVM runtime** (`cmd/ateom-microvm`, cloud-hypervisor) is
   still not the default isolation (gVisor is), but on barceloneta it is
   *runnable today* — no nesting, no rental needed. Substrate remains
   pre-1.0, x86_64-centric.

Capacity honesty: 0003 sized the Robot at 8c/64 GB for ≤20 concurrent
sandboxes. The NUC is 6C/8T at 15 W with 16 GB — Substrate's own pitch
(30× oversubscription of *idle* actors, sub-500 ms suspend/resume) is
precisely the mechanism that makes 16 GB viable for Otavio's actual scale
(1 tenant, 4–6 live agents, bursty activity). It is **not** viable as a
multi-tenant scale-out host. The NUC is a pilot/single-tenant box; the
Robot remains the scale-out path.

Why move the pilot to the NUC:

- **€0 marginal cost** vs. ≈ €50–60/mo, for a workload that today (30/09–
  01/10 binding incident) demonstrably needs isolation *now*, at 1-user
  scale, before any tenant exists.
- **The isolation argument got stronger, not weaker**: the 01/10 incident
  (two pi panes sharing one transcript; one reply relayed into the wrong
  topic) is exactly the failure class shared-UID tmux cannot structurally
  prevent. maquinista already runs *on* barceloneta — isolating it where it
  lives removes the playa↔box hop for the drive loop entirely.
- **microVM mode becomes a real option**, not roadmap: KVM is present. For a
  home box the snapshot/suspend story also survives power cuts better than
  pane respawn does (though snapshots live on the same NVMe — this is
  availability sugar, not backup).

## Options Considered

| | A. NUC now (k3s + Substrate + ate-env), Robot later | B. Stay the course: rent Robot (ADR-0003 as written) | C. LXC-per-agent on Proxmox, no Substrate | D. Defer (tmux-only) |
|---|---|---|---|---|
| Marginal cost | €0 | ≈ €50–60/mo from day 1 | €0 | €0 |
| Isolation quality | gVisor today, microVM *available* | same as A | container-grade only | none |
| KVM/microVM path | native (bare metal) | native (bare metal) | no (LXC is not a VM) | n/a |
| ate-env surface | local, no cross-host auth | remote over TS/WG | n/a | n/a |
| Capacity for scale-out | capped ~3–6 active sandboxes | ≤20 sandboxes | medium | n/a |
| Ops risk on existing homelab | k3s coexists with Proxmox/LXC on 16 GB — the real risk | new box, zero coexistence risk | low (Proxmox-native) | none |
| Day-1 churn exposure | gated by G-00* (below) | gated by S-00* (0003) | low | none |

## Decision

**Option A, as a Phase-0-gated pilot on the NUC; Option B (Robot rental)
demoted to the explicit scale-out trigger.** tmux stays the default executor
throughout — ADR-0003's adapter phasing (shadow mode, opt-in agents,
never a flag day) carries over unchanged. ADR-0003's S-00 gates are
re-scoped into NUC-specific G-00 gates; nothing in the adapter or tenant
design changes.

### Phase 0 — Pilot gates (go/no-go, ~1 day, all on barceloneta)

- **G-00a Coexistence budget**: k3s (single node) installs *alongside*
  Proxmox + LXC on 16 GB without disturbing ct100–ct102 or the maquinista
  daemon. Measured: free RAM after k3s idle ≤ 4 GB, no OOM events in 24 h.
  **Disk locus (decided 2026-10-01)**: k3s runs with `--data-dir /home/k3s`
  so containerd images, cluster state and local-path PVCs land on `/home`
  (808 GB free) instead of the default `/var/lib/rancher` — `/var` is its
  own 22 GB partition shared with the maquinista Postgres and would fill
  first under sandbox image churn.
  Fallback shape if the host is too tight: k3s inside one dedicated LXC
  (gVisor-only; microVM mode then needs KVM passthrough — likely no-go, so
  this fallback forfeits microVM, and is recorded as such).
- **G-00b PTY fidelity (re-scope of S-00a)**: console stream is TUI-grade
  through both gVisor and microVM sandboxes, via `ax ssh` / ate-env exec.
  MonitorProfile-style scraping must survive.
- **G-00c Transcript egress (re-scope of S-00b, now concretely shaped by
  ate-env)**: a maquinista `TranscriptSource` can tail a JSONL transcript
  out of the sandbox with p95 latency < 5 s per append, via ate-env fs API
  or a guest-side push shim. Measure auth overhead per call too — the
  monitor polls per cycle.
- **G-00d MicroVM reality**: cloud-hypervisor boots a guest on the Core 3
  100U, and snapshot/resume of an idle actor completes < 5 s on this NVMe.
  If slow or broken, the pilot proceeds gVisor-only and the microVM claim is
  parked with a revisit trigger — gVisor already satisfies the isolation
  requirement; microVM is the upgrade, not the gate.

All four gates falsifiable in one day; no-go on G-00a or G-00c parks this
ADR and re-opens Robot rental (Option B) as written in 0003.

Evidence status 2026-10-02 (substrate fork ADR-0001 outcome log): G-00a
effectively evidenced — 4.7 GB RAM floor under a 7 G synthetic hog, zero
OOM, ct101/102 + maquinista daemon intact through a real cold reboot.
G-00c GO for per-runner egress with named exclusions (push ≈ 7 ms/line;
NO-GO for whole-file polling and multi-tenant as-is). G-00d PASS at ~13×
under target (suspend 0.45 s / resume 0.38 s), upgraded by reboot
durability: snapshot bytes survive a cold host boot, recovery =
`kubectl ate revert` → resume, proven on mc-e04 with continuous counters.
One gate open: **G-00b streaming relay** — E-02's caveat (request/response
`/process` exercised; the live follow-stream maquinista would wire to
MonitorProfile scraping is not yet).

### Phase 1 — ate-env executor adapter (3–5 days, unchanged from 0003 Phase 1 in shape)

`internal/executor/env.go` against the ate-env **Go client** (not raw REST):
Create → new environment, Write → fs ops, Read → fs ops (transcript tail),
Kill → environment delete; suspend/resume mapped when maquinista's reconcile
moves from pane-respawn to snapshot-resume. Same seam, same shadow-mode
rollout as 0003 planned.

Config surface (amended 2026-10-02): the adapter's ONLY sandbox-aware knobs
are `ATESPACE` + `ACTOR_TEMPLATE` (e.g. `sandbox-gvisor` / `sandbox-microvm`).
Substrate routes sandbox technology at the template level (template → worker
pool → SandboxConfig/RuntimeClass), so `Create(actor, template)` is identical
either way — executor code never branches on sandbox tech. Both classes
already coexist on the same node (e02-pty-1 gVisor, mc-e05 microvm).
Switching or shadowing = a config edit, not a port; on a KVM-less host
(Hetzner fallback) the microvm template simply doesn't exist and the
adapter stays unchanged.

### Phase 2 — Sandbox technology as a permanent config knob (amended 2026-10-02)

Start position: `ACTOR_TEMPLATE=sandbox-gvisor` (mature, no KVM dependency,
and it keeps the Hetzner fallback alive — microVM can never run there). The
microvm template stays warm behind the same knob; **there is no promotion
event**. Evidence accrues by shadow A/B: the same agent task run against
both templates, diffing transcript integrity, PTY stream quality and
lifecycle latency — the G-00b open item collapses into "flip the knob, run
the probe".

Named prerequisite for the knob's second position: a microvm actor template
whose guest image embeds `ate-env-guest` — ate-env fs/process ops are the
transcript-egress path (E-03-proven), and the counter-microvm demo guest
serves HTTP only. Small job: the demo's guest-build pipeline already exists
in-repo. Per-tenant egress allowlists follow 0003's Gateway model verbatim.

Evidence base already banked (substrate fork ADR-0001, 02/10): microvm
suspend 0.45 s / resume 0.38 s; snapshot bytes survive cold host reboot
(revert → resume, mc-e04 counters continuous); gVisor checkpoint/restore
same-pids with the live PTY surviving the worker move (E-02).

### Scale-out trigger (when the NUC is no longer enough)

Provision the Robot box per ADR-0003 §Phase 0 when **any** of: sustained
RAM pressure with < 1 GB host headroom at idle-agent loads; a second tenant
onboards; snapshot churn makes the NVMe the bottleneck. The adapter makes
this a config move, not a port.

## Consequences

- **Positive:** pilot starts at €0 on hardware that already hosts the
  workload; microVM mode is *available* from day 1 (0003 could only promise
  gVisor); the playa↔box hop disappears for the drive loop; ate-env gives
  S-00b a concrete, measurable surface instead of an open question.
- **Negative:** 16 GB shared between k3s, Substrate workers, LXC services
  and the maquinista daemon — coexistence is the top failure mode (hence
  G-00a gates it); home-host realities (power cuts, residential ISP,
  NVMe-only snapshots) mean this box is a pilot locus, never the durability
  story; ate-env is alpha on top of pre-1.0 Substrate — day-1 API churn risk
  is now double-layered and stays gated behind Phase 0.
- **Neutral:** ADR-0003's tenant/adapter design, Robot sizing and egress
  model all survive intact; only the *first* hardware locus changes. The
  16 GB envelope also caps how many of 0003's "≤20 sandboxes" ever run
  here — by design.

## References

- ADR-0002 — `Executor` port; ADR-0003 — Substrate/AX plan this ADR revises
  (its S-00a/S-00b gates re-scope into G-00b/G-00c here)
- Barceloneta inspection 2026-10-01 (disk layout re-verified same day):
  `systemd-detect-virt`=none, `/dev/kvm` present, VT-x ×16,
  Core 3 100U / 16 GB / 931 GB NVMe — `/` 43 GB free, `/home` 808 GB free
- maquinista incident 30/09–01/10: two pi panes shared one transcript;
  fixed in `458c6c6` + `cbc7ab4` (resume-safe bindings, duplicate-session
  resume-guard) — the isolation motivation, on record
- `github.com/agent-substrate/env` — ate-env CLI/API/guest, Go+Python
  clients, alpha (shallow clone 2026-10-01)
- `github.com/agent-substrate/substrate` — actor/worker multiplexing on
  Kubernetes; gVisor default, microVM via cloud-hypervisor, pre-1.0
  (shallow clone 2026-10-01)
