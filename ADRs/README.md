# ADRs

Architecture Decision Records for maquinista. Each ADR captures one decision
with its context, options, and consequences at the time it was made. ADRs are
never deleted; superseded ones link to their replacement.

Status lifecycle: Proposto → Aceito → Depreciado/Substituído.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-deepseek-harness-integration.md) | DeepSeek harness integration (thin claude-compatible runner) | Proposto |
| [0002](0002-executor-port.md) | Executor port — decouple agent session I/O from tmux (ports & adapters) | Proposto |
| [0003](0003-substrate-hetzner-multitenant.md) | Substrate/AX executor on dedicated Hetzner box, multi-tenant | Proposto |
| [0004](0004-barceloneta-nuc-execution-box.md) | Barceloneta NUC as execution box — owned silicon first, Robot as scale-out | Proposto |
| [0005](0005-linear-pr-iteration-pipeline.md) | Linear-driven PR iteration pipeline — tlc-spec-lean execution via maquinista agent sessions | Proposto |
| [0006](0006-ticket-provider-abstraction.md) | Ticket-provider abstraction — vendor-neutral pipeline core behind a TicketProvider seam | Proposto |
| [0007](0007-kagent-benchmark.md) | kagent benchmark — stay the course; adopt OTel tracing, checkpoint/fork, temporal verdict guards | Proposto |
