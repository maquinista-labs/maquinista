# Maquinista Execution Plan

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

Derived from `maquinista_plan.md`. 35 tasks across 5 phases, organized into execution waves that maximize parallelism while respecting dependencies.

---

## Wave 1 — Foundation (no dependencies)

| Task | Description | Priority | Est. Complexity |
|------|------------|----------|-----------------|
| **P1-01** | Initialize Go module, directory skeleton, Makefile, empty cobra root | 10 | Small |

**Deliverable:** `go build ./cmd/maquinista && ./maquinista version`

---

## Wave 2 — Independent Package Ports (depends: P1-01)

All tasks in this wave can run **in parallel**.

| Task | Description | Priority | Est. Complexity |
|------|------------|----------|-----------------|
| **P1-02** | Port merged config package (tramuntana base + DB/Maquinista fields) | 9 | Small |
| **P1-03** | Port merged tmux package (tramuntana base + minuano extras) | 9 | Medium |
| **P1-04** | Port merged git package (tramuntana base + minuano extras) | 9 | Small |
| **P1-05** | Port state package from tramuntana (wholesale copy) | 8 | Small |
| **P1-06** | Port database package from minuano (wholesale copy) | 8 | Medium |
| **P1-12** | Port and rename scripts (minuano-* -> maquinista-*) | 6 | Small |
| **P1-13** | Port docker and claude directories | 5 | Small |

**Deliverable:** All foundational packages compile independently.

---

## Wave 3 — Dependent Package Ports (depends: Wave 2 subsets)

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P1-07** | Port agent package from minuano | 7 | P1-03, P1-04, P1-06 |
| **P1-08** | Port render, queue, monitor, listener from tramuntana | 7 | P1-02, P1-05 |
| **P1-10** | Port TUI package from minuano | 6 | P1-06 |
| **P1-11** | Port hook package from tramuntana | 6 | P1-03, P1-05 |

P1-07, P1-08, P1-10, P1-11 can run **in parallel** (different dependency subsets).

**Deliverable:** All internal packages compile.

---

## Wave 4 — Bot and Bridge (depends: Wave 3 subsets)

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P1-09** | Port bot package and bridge from tramuntana | 6 | P1-08, P1-03 |

**Deliverable:** Bot package compiles with all handler files.

---

## Wave 5 — CLI Wiring and Verification (depends: all P1 packages)

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P1-14** | Wire up unified CLI with all subcommands | 10 | P1-02 through P1-13 |
| **P1-15** | End-to-end build verification | 10 | P1-14 |

**Deliverable:** `maquinista version`, `maquinista serve --help`, `maquinista status --help`, `go test ./... && go vet ./...`

---

## Wave 6 — Spec Parser + Runner Interface (depends: P1-15)

These two tracks can run **in parallel** since they are independent.

### Track A: Spec System

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P2-01** | Implement spec file parser (YAML frontmatter + markdown body) | 8 | P1-15 |
| **P2-02** | Implement spec sync logic (diff specs vs DB, create/update) | 8 | P2-01, P1-06 |
| **P2-03** | Create `maquinista spec sync` CLI command | 7 | P2-02 |
| **P2-04** | Example specs and documentation | 5 | P2-03 |

### Track B: Agent Runner

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P3-01** | Define AgentRunner interface and registry | 8 | P1-15 |
| **P3-02** | Implement Claude Code runner | 8 | P3-01 |
| **P3-03** | Implement OpenCode runner | 7 | P3-01 |
| **P3-04** | Implement Custom/Generic runner | 6 | P3-01 |
| **P3-06** | Add DB migration for runner metadata | 7 | P1-06 |

P3-02, P3-03, P3-04 can run **in parallel** after P3-01.

**Deliverable (Track A):** `maquinista spec sync --dir .specs/ --project test` works.
**Deliverable (Track B):** Runner interface with 3 implementations + DB migration.

---

## Wave 7 — Agent Refactor (depends: P3-02, P1-07)

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P3-05** | Refactor agent package to use AgentRunner | 8 | P3-02, P1-07 |

**Deliverable:** `maquinista spawn my-agent --runner claude` and `--runner opencode` both work.

---

## Wave 8 — Orchestrator (depends: P3-05, P1-06, P1-07, P1-08)

Sequential chain:

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P4-01** | Implement core orchestrator loop (poll-dispatch-reconcile) | 8 | P3-05, P1-06, P1-07 |
| **P4-02** | Add NOTIFY-driven wake-up (channel-based select) | 7 | P4-01, P1-08 |
| **P4-03** | Create `maquinista orchestrate` command | 8 | P4-02 |

Then in parallel after P4-03:

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P4-04** | Extract shared prompt builders | 7 | P4-03 |
| **P4-05** | Orchestrator status reporting | 5 | P4-03 |

**Deliverable:** `maquinista orchestrate --project test --max-agents 2` polls and dispatches.

---

## Wave 9 — Telegram Decoupling (partially parallel with Wave 8)

P5-01 can start as early as Wave 6 (only depends on P1-06):

| Task | Description | Priority | Depends on |
|------|------------|----------|------------|
| **P5-01** | Add topic-agent observation model to DB (migration 007) | 7 | P1-06 |
| **P5-02** | Update monitor to route by agent observation | 6 | P5-01, P1-08 |
| **P5-03** | Add /observe and /unobserve Telegram commands | 6 | P5-02 |
| **P5-04** | Orchestrator Telegram notifications | 5 | P5-01, P4-03 |
| **P5-05** | Combined serve+orchestrate mode | 5 | P5-04 |

**Deliverable:** Topics can observe any agent. `maquinista serve --orchestrate` runs both.

---

## Critical Path

The longest dependency chain determines minimum sequential effort:

```
P1-01 -> P1-03 -> P1-07 -> P3-05 -> P4-01 -> P4-02 -> P4-03 -> P5-04 -> P5-05
  |        |                  ^
  +-> P1-06 ----------------+
  |        |
  +-> P1-04 -+
  |
  +-> P3-01 -> P3-02 -------+
```

**Critical path length: 10 tasks** (P1-01 -> P1-03/P1-04/P1-06 -> P1-07 -> P3-01 -> P3-02 -> P3-05 -> P4-01 -> P4-02 -> P4-03 -> P5-05)

---

## Execution Summary

| Wave | Tasks | Parallelism | Cumulative |
|------|-------|-------------|------------|
| 1 | 1 | 1 | 1 |
| 2 | 7 | 7 parallel | 8 |
| 3 | 4 | 4 parallel | 12 |
| 4 | 1 | 1 | 13 |
| 5 | 2 | sequential | 15 |
| 6 | 9 (2 tracks) | up to 5 parallel | 24 |
| 7 | 1 | 1 | 25 |
| 8 | 5 | 3 seq + 2 parallel | 30 |
| 9 | 5 | mixed | 35 |

**Total: 35 tasks. With max parallelism, ~10 sequential steps minimum.**

---

## Task Checklist

- [ ] P1-01: Initialize maquinista Go module and directory skeleton
- [ ] P1-02: Port merged config package
- [ ] P1-03: Port merged tmux package
- [ ] P1-04: Port merged git package
- [ ] P1-05: Port state package from tramuntana
- [ ] P1-06: Port database package from minuano
- [ ] P1-07: Port agent package from minuano
- [ ] P1-08: Port render, queue, monitor, listener packages
- [ ] P1-09: Port bot package and bridge from tramuntana
- [ ] P1-10: Port TUI package from minuano
- [ ] P1-11: Port hook package from tramuntana
- [ ] P1-12: Port and rename scripts
- [ ] P1-13: Port docker and claude directories
- [ ] P1-14: Wire up unified CLI with all subcommands
- [ ] P1-15: End-to-end build verification
- [ ] P2-01: Implement spec file parser
- [ ] P2-02: Implement spec sync logic
- [ ] P2-03: Create `maquinista spec sync` CLI command
- [ ] P2-04: Example specs and documentation
- [ ] P3-01: Define AgentRunner interface and registry
- [ ] P3-02: Implement Claude Code runner
- [ ] P3-03: Implement OpenCode runner
- [ ] P3-04: Implement Custom/Generic runner
- [ ] P3-05: Refactor agent package to use AgentRunner
- [ ] P3-06: Add DB migration for runner metadata
- [ ] P4-01: Implement core orchestrator loop
- [ ] P4-02: Add NOTIFY-driven wake-up
- [ ] P4-03: Create `maquinista orchestrate` command
- [ ] P4-04: Extract shared prompt builders
- [ ] P4-05: Orchestrator status reporting
- [ ] P5-01: Add topic-agent observation model to DB
- [ ] P5-02: Update monitor to route by agent observation
- [ ] P5-03: Add /observe and /unobserve Telegram commands
- [ ] P5-04: Orchestrator Telegram notifications
- [ ] P5-05: Combined serve+orchestrate mode
