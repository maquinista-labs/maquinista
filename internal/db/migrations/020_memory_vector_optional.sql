-- 020_memory_vector_optional.sql
--
-- Phase 2 of plans/active/agent-memory-db.md: optional vector search
-- over archival passages. Requires the pgvector extension; if the
-- cluster doesn't have it, this migration is a no-op (wrapped in a
-- DO-block so the rest of the stack keeps working).
--
-- When pgvector is present:
--   - an `embedding vector(384)` column is added to agent_memories
--     (sized for bge-small-en / all-MiniLM-L6-v2 — the popular 384-d
--     CPU-friendly embeddings)
--   - an ivfflat index is built for approximate nearest-neighbor
--     queries. Pull this into maintenance when the row count grows
--     (REINDEX … CONCURRENTLY).
--
-- Embedding ingestion is feature-flagged — maquinista ships with no
-- embedding provider wired, so this column stays NULL on every write
-- unless operators configure one. Search helpers degrade to
-- Postgres FTS when embeddings are missing.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
        CREATE EXTENSION IF NOT EXISTS vector;

        BEGIN
            ALTER TABLE agent_memories
                ADD COLUMN IF NOT EXISTS embedding vector(384);
        EXCEPTION WHEN others THEN
            RAISE NOTICE 'memory vector: ALTER failed: %', SQLERRM;
        END;

        BEGIN
            CREATE INDEX IF NOT EXISTS agent_memories_embedding_ivf
                ON agent_memories USING ivfflat (embedding vector_cosine_ops)
                WITH (lists = 100);
        EXCEPTION WHEN others THEN
            RAISE NOTICE 'memory vector: ivfflat index skipped: %', SQLERRM;
        END;

        RAISE NOTICE 'memory vector: pgvector enabled; embedding column + ivfflat index ready';
    ELSE
        RAISE NOTICE 'memory vector: pgvector extension not available; skipping';
    END IF;
END;
$$;
