-- 017_agent_memory.sql
--
-- Phase 0 + Phase 1 of plans/active/agent-memory-db.md:
-- agent_blocks (core, in-context memory injected every turn) +
-- agent_memories (archival passages retrieved by search tool).
--
-- Recall memory (inbox/outbox) already exists in migration 009.

-- ------------------------------------------------------------------
-- Core blocks (Letta shape). Small, always in-context, curated by the
-- agent via core_memory_append / core_memory_replace tools. The
-- (agent_id, label) pair is unique so the default block seeding is
-- idempotent.
-- ------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_blocks (
    id          BIGSERIAL   PRIMARY KEY,
    agent_id    TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    label       TEXT        NOT NULL,
    value       TEXT        NOT NULL DEFAULT '',
    char_limit  INTEGER     NOT NULL DEFAULT 2200,
    read_only   BOOLEAN     NOT NULL DEFAULT FALSE,
    description TEXT,
    version     INTEGER     NOT NULL DEFAULT 1,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (agent_id, label)
);
CREATE INDEX IF NOT EXISTS idx_agent_blocks_agent ON agent_blocks (agent_id);

-- ------------------------------------------------------------------
-- Archival passages. Unbounded per agent; Postgres FTS over title+body
-- via tsvector + GIN index. Pinned rows always float to the top and
-- are candidates for spawn-time injection.
-- ------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_memories (
    id         BIGSERIAL   PRIMARY KEY,
    agent_id   TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    dimension  TEXT        NOT NULL
               CHECK (dimension IN ('agent','user')),
    tier       TEXT        NOT NULL
               CHECK (tier IN ('long_term','daily','signal')),
    category   TEXT        NOT NULL
               CHECK (category IN ('feedback','project','reference','fact','preference','other')),
    title      TEXT        NOT NULL,
    body       TEXT        NOT NULL,
    source     TEXT        NOT NULL,
    source_ref TEXT,
    tags       TEXT[]      NOT NULL DEFAULT '{}',
    pinned     BOOLEAN     NOT NULL DEFAULT FALSE,
    score      REAL        NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    tsv        tsvector    GENERATED ALWAYS AS
               (to_tsvector('simple',
                    COALESCE(title,'') || ' ' || COALESCE(body,'')
                )) STORED
);

CREATE INDEX IF NOT EXISTS agent_memories_agent_dim_tier_idx
    ON agent_memories (agent_id, dimension, tier, created_at DESC);
CREATE INDEX IF NOT EXISTS agent_memories_tsv_idx
    ON agent_memories USING GIN (tsv);
CREATE INDEX IF NOT EXISTS agent_memories_tags_idx
    ON agent_memories USING GIN (tags);
CREATE INDEX IF NOT EXISTS agent_memories_pinned_idx
    ON agent_memories (agent_id, dimension) WHERE pinned;
