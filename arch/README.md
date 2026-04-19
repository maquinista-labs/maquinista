# Architecture

Durable truths about how maquinista works. Each file covers one concern.
These are not plans (see `plans/`) and not user docs (see `docs/`) — they
describe invariants that cut across the codebase and should stay accurate
as the system evolves.

| File | What it covers |
|------|----------------|
| [messaging.md](messaging.md) | How agents send and receive messages — inbox/outbox, channels, relay, dispatcher |
