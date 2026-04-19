# Soul and Identity

## What a soul is

A soul is the persistent identity of an agent: its name, role, goal,
core truths, boundaries, and vibe. It is rendered into the system prompt
that bootstraps the runner on every fresh start.

The soul is DB-only. No prompt files on disk. The runner receives it via
shell substitution at launch time:

```sh
claude --system-prompt "$(maquinista soul render <agent-id>)"
```

This means editing the soul in the DB takes effect on the next spawn or
restart — no file sync required.

## Tables

```
soul_templates     — reusable blueprints; shown in dashboard "New Agent" picker
agent_souls        — one row per agent, cloned from a template at spawn time
```

`agent_souls` fields mirror `soul_templates` plus `agent_id` FK. After
cloning, the agent's soul is independent of the template — template
changes do not propagate to existing agents.

## Soul rendering

`maquinista soul render <agent-id>` queries `agent_souls`, interpolates
the fields into a system prompt string, and prints to stdout. The shell
captures this at launch time.

Rendering also injects **memory blocks** (see below).

## Memory blocks

`agent_memory` rows are short key/value entries attached to an agent:
persona notes, project context, recurring preferences. They are appended
to the rendered soul so the agent has them in its system prompt on every
startup.

`memory.SeedDefaultBlocks` populates a standard set of blocks at spawn
time (e.g. "you are agent X", project cwd, etc.).

## Identity across restarts

On resume (`--resume <session_id>`), the runner reloads its own
conversation history. Soul injection is **skipped** — the history already
contains the original system prompt. This avoids a doubled or
contradictory identity.

On fresh start (no session_id), soul is injected fresh. If the soul was
edited since the last run, the new identity takes effect.

## Templates

`soul_templates` are operator-managed. The dashboard "New Agent" form
shows the list. A template has the same fields as `agent_souls` plus an
`id` and `name` for display.

Built-in templates (seeded by `seedDefaultAgents`): coordinator, planner,
coder. Custom templates can be added via `maquinista soul template` CLI
or directly in the DB.

## TODO

- [ ] Document advanced template fields (plans/active/soul-template-advanced.md)
- [ ] Document soul edit flow (maquinista soul edit)
- [ ] Memory block types and priority ordering
- [ ] Soul versioning / history
