-- Migration 032: Shared memory archives (Phase 5 of agent-memory-db.md).
-- Letta-style: an archive owns a set of passages; junction table grants
-- agents read/write access. Multiple agents share an archive without
-- data duplication.

CREATE TABLE IF NOT EXISTS agent_archives (
    id             BIGSERIAL    PRIMARY KEY,
    name           TEXT         NOT NULL,
    description    TEXT,
    owner_agent_id TEXT         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (owner_agent_id, name)
);

CREATE TABLE IF NOT EXISTS archive_members (
    archive_id  BIGINT  NOT NULL REFERENCES agent_archives(id) ON DELETE CASCADE,
    agent_id    TEXT    NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    role        TEXT    NOT NULL CHECK (role IN ('owner', 'writer', 'reader')),
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (archive_id, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_archive_members_agent ON archive_members (agent_id);

-- Attach an optional archive to each archival passage.
-- NULL archive_id = private to agent_id (existing behavior unchanged).
ALTER TABLE agent_memories
    ADD COLUMN IF NOT EXISTS archive_id BIGINT REFERENCES agent_archives(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_agent_memories_archive
    ON agent_memories (archive_id) WHERE archive_id IS NOT NULL;
