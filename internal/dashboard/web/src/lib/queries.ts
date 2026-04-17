// SQL query helpers for the dashboard. Keep query text here so tests
// can run them against a pg_tmp / testcontainers DB without spinning
// up a Next server.

import type { Pool } from "pg";

import type {
  AgentListItem,
  ConversationItem,
  InboxRow,
  OutboxRow,
} from "@/lib/types";

// excerptFromContent extracts a compact preview from an agent_inbox
// or agent_outbox content JSONB. The content shape varies — we
// tolerate {text}, {body}, {message}, and fall back to JSON.stringify.
export function excerptFromContent(
  content: unknown,
  max = 120,
): string | null {
  if (content == null) return null;
  if (typeof content === "string") {
    return truncate(content, max);
  }
  if (typeof content === "object") {
    const obj = content as Record<string, unknown>;
    for (const key of ["text", "body", "message", "summary"]) {
      const v = obj[key];
      if (typeof v === "string" && v.length > 0) {
        return truncate(v, max);
      }
    }
    try {
      return truncate(JSON.stringify(content), max);
    } catch {
      return null;
    }
  }
  return null;
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 1) + "…";
}

// listAgents returns one row per agent with the latest outbox
// excerpt and unread inbox count joined in. Read-only, safe to call
// from a Server Component.
export async function listAgents(pool: Pool): Promise<AgentListItem[]> {
  const { rows } = await pool.query(
    `
    WITH latest_outbox AS (
      SELECT DISTINCT ON (agent_id)
             agent_id, content, created_at
      FROM agent_outbox
      ORDER BY agent_id, created_at DESC
    ),
    inbox_counts AS (
      SELECT agent_id, COUNT(*)::int AS unread_count
      FROM agent_inbox
      WHERE status IN ('pending','processing')
      GROUP BY agent_id
    )
    SELECT
      a.id,
      a.handle,
      a.runner_type   AS runner,
      NULL::text      AS model,
      a.role,
      a.status,
      a.stop_requested,
      a.tmux_window,
      a.started_at,
      a.last_seen,
      lo.content      AS last_outbox_content,
      lo.created_at   AS last_outbox_at,
      COALESCE(ic.unread_count, 0) AS unread_inbox_count,
      s.persona
    FROM agents a
    LEFT JOIN agent_settings s ON s.agent_id = a.id
    LEFT JOIN latest_outbox  lo ON lo.agent_id = a.id
    LEFT JOIN inbox_counts   ic ON ic.agent_id = a.id
    WHERE a.status <> 'archived' OR a.status IS NULL
    ORDER BY a.last_seen DESC NULLS LAST, a.started_at DESC
    `,
  );

  return rows.map((r) => ({
    id: r.id,
    handle: r.handle,
    runner: r.runner,
    model: r.model,
    role: r.role,
    status: r.status,
    stop_requested: r.stop_requested,
    tmux_window: r.tmux_window,
    started_at: r.started_at.toISOString
      ? r.started_at.toISOString()
      : String(r.started_at),
    last_seen: r.last_seen
      ? r.last_seen.toISOString
        ? r.last_seen.toISOString()
        : String(r.last_seen)
      : null,
    last_outbox_excerpt: excerptFromContent(r.last_outbox_content),
    last_outbox_at: r.last_outbox_at
      ? r.last_outbox_at.toISOString
        ? r.last_outbox_at.toISOString()
        : String(r.last_outbox_at)
      : null,
    unread_inbox_count: Number(r.unread_inbox_count) || 0,
    persona: r.persona,
  }));
}

export async function getAgent(
  pool: Pool,
  id: string,
): Promise<AgentListItem | null> {
  const all = await listAgents(pool);
  return all.find((a) => a.id === id) ?? null;
}

export type InboxListOpts = {
  limit?: number;
  before?: string; // ISO timestamp cursor
};

export async function listInbox(
  pool: Pool,
  agentId: string,
  opts: InboxListOpts = {},
): Promise<InboxRow[]> {
  const limit = Math.min(opts.limit ?? 50, 200);
  const params: unknown[] = [agentId];
  let where = `agent_id = $1`;
  if (opts.before) {
    params.push(opts.before);
    where += ` AND enqueued_at < $${params.length}`;
  }
  const { rows } = await pool.query(
    `
    SELECT id, agent_id, from_kind, from_id, status,
           origin_channel, origin_user_id, content, enqueued_at
    FROM agent_inbox
    WHERE ${where}
    ORDER BY enqueued_at DESC
    LIMIT ${limit}
    `,
    params,
  );
  return rows.map((r) => ({
    id: r.id,
    agent_id: r.agent_id,
    from_kind: r.from_kind,
    from_id: r.from_id,
    status: r.status,
    origin_channel: r.origin_channel,
    origin_user_id: r.origin_user_id,
    excerpt: excerptFromContent(r.content),
    enqueued_at: r.enqueued_at.toISOString
      ? r.enqueued_at.toISOString()
      : String(r.enqueued_at),
  }));
}

export async function listOutbox(
  pool: Pool,
  agentId: string,
  opts: InboxListOpts = {},
): Promise<OutboxRow[]> {
  const limit = Math.min(opts.limit ?? 50, 200);
  const params: unknown[] = [agentId];
  let where = `agent_id = $1`;
  if (opts.before) {
    params.push(opts.before);
    where += ` AND created_at < $${params.length}`;
  }
  const { rows } = await pool.query(
    `
    SELECT id, agent_id, in_reply_to, status, content, created_at
    FROM agent_outbox
    WHERE ${where}
    ORDER BY created_at DESC
    LIMIT ${limit}
    `,
    params,
  );
  return rows.map((r) => ({
    id: r.id,
    agent_id: r.agent_id,
    in_reply_to: r.in_reply_to,
    status: r.status,
    excerpt: excerptFromContent(r.content),
    created_at: r.created_at.toISOString
      ? r.created_at.toISOString()
      : String(r.created_at),
  }));
}

export async function listConversation(
  pool: Pool,
  conversationId: string,
  limit = 100,
): Promise<ConversationItem[]> {
  const { rows } = await pool.query(
    `
    (
      SELECT 'inbox' AS kind, id, agent_id, from_kind, content,
             enqueued_at AS at
      FROM agent_inbox
      WHERE conversation_id = $1
    )
    UNION ALL
    (
      SELECT 'outbox' AS kind, id, agent_id, NULL AS from_kind, content,
             created_at AS at
      FROM agent_outbox
      WHERE conversation_id = $1
    )
    ORDER BY at ASC
    LIMIT ${Math.min(limit, 500)}
    `,
    [conversationId],
  );
  return rows.map((r) => ({
    kind: r.kind,
    id: r.id,
    agent_id: r.agent_id,
    from_kind: r.from_kind,
    excerpt: excerptFromContent(r.content),
    at: r.at.toISOString ? r.at.toISOString() : String(r.at),
  }));
}
