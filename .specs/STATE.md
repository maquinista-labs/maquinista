# STATE

Handoff snapshot and project-level decisions for maquinista `.specs/`.

## Decisions

| ID | Decision | Status |
| --- | --- | --- |
| AD-0001 | The runner registry key is the canonical runner_type string shared by `runner.Register`, `TranscriptSource` registration in `cmd_start.go`, and `agents.runner_type` — one name, three places, always equal | active |
| AD-0002 | maquinista runner env overrides for pi live under the `MAQUINISTA_PI_*` namespace; bare `PI_MODEL`/`PI_PROVIDER`/`PI_SESSION_ID`/`PI_SESSION_FILE` are pi's own injected outputs (verified 0.73.1) and are never runner inputs | active |
