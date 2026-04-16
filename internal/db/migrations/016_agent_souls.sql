-- 016_agent_souls.sql
--
-- Phase 1 of plans/active/agent-soul-db-state.md: first-class DB entity
-- for agent identity (name, role, goal, boundaries, vibe). Replaces the
-- opaque agent_settings.system_prompt blob with structured fields that
-- spawn-time prompt composition can render.
--
-- soul_templates carries the catalog of reusable identities (default,
-- helper, planner, …). agent_souls is the per-agent instantiation —
-- created by the agent-creation code path, edited individually after.

CREATE TABLE IF NOT EXISTS soul_templates (
    id               TEXT        PRIMARY KEY,
    name             TEXT        NOT NULL,
    tagline          TEXT,
    role             TEXT        NOT NULL,
    goal             TEXT        NOT NULL,
    core_truths      TEXT        NOT NULL DEFAULT '',
    boundaries       TEXT        NOT NULL DEFAULT '',
    vibe             TEXT        NOT NULL DEFAULT '',
    continuity       TEXT        NOT NULL DEFAULT '',
    extras           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    allow_delegation BOOLEAN     NOT NULL DEFAULT FALSE,
    max_iter         INTEGER     NOT NULL DEFAULT 25,
    is_default       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one default template system-wide (tier-0 fallback at agent
-- creation when no --soul-template is passed).
CREATE UNIQUE INDEX IF NOT EXISTS uq_soul_templates_one_default
    ON soul_templates ((is_default))
    WHERE is_default;

CREATE TABLE IF NOT EXISTS agent_souls (
    agent_id         TEXT        PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    template_id      TEXT        REFERENCES soul_templates(id),
    name             TEXT        NOT NULL,
    tagline          TEXT,
    role             TEXT        NOT NULL,
    goal             TEXT        NOT NULL,
    core_truths      TEXT        NOT NULL DEFAULT '',
    boundaries       TEXT        NOT NULL DEFAULT '',
    vibe             TEXT        NOT NULL DEFAULT '',
    continuity       TEXT        NOT NULL DEFAULT '',
    extras           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    allow_delegation BOOLEAN     NOT NULL DEFAULT FALSE,
    max_iter         INTEGER     NOT NULL DEFAULT 25,
    respect_context  BOOLEAN     NOT NULL DEFAULT TRUE,
    version          INTEGER     NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Seed the default template so bootstrap works on an empty install.
INSERT INTO soul_templates
    (id, name, tagline, role, goal,
     core_truths, boundaries, vibe, continuity,
     allow_delegation, max_iter, is_default)
VALUES (
    'default',
    'Maquinista',
    'Operator-in-the-loop engineering agent',
    'Engineering collaborator',
    'Ship small, correct changes that move the repo forward without surprising the operator.',
    '- Be genuinely helpful. Ship small, correct changes.' || E'\n' ||
    '- Have opinions, earn trust by being specific.' || E'\n' ||
    '- Never delegate understanding.',
    '- Do not run destructive git commands without confirmation.' || E'\n' ||
    '- Validate only at system boundaries.' || E'\n' ||
    '- Refuse to bypass CI/hooks.',
    'Calm, concise, terminal-native. Short sentences, no filler.',
    'State persists in Postgres between sessions; memory continuity lives in agent_memory.',
    FALSE, 25, TRUE
)
ON CONFLICT (id) DO NOTHING;
