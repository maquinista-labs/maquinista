// SQL query helpers for the dashboard. Keep query text here so tests
// can run them against a pg_tmp / testcontainers DB without spinning
// up a Next server.

import type { Pool } from "pg";

import type {
  AgentListItem,
  ConversationItem,
  ConversationRow,
  GlobalInboxRow,
  InboxRow,
  JobsList,
  KPIs,
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
      a.model         AS model,
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

export type GlobalInboxOpts = {
  limit?: number;
  status?: InboxRow["status"][]; // defaults to ['pending','processing']
};

// listGlobalInbox: cross-agent feed of non-human signals entering the
// system — webhooks, scheduled jobs. Excludes:
//   • all user messages (from_kind='user') — operator's own messages
//     whether sent via Telegram or dashboard; those belong in Chats
//   • agent-to-agent messages (from_kind='agent') — internal plumbing
//   • system control messages (from_kind='system') — internal plumbing
export async function listGlobalInbox(
  pool: Pool,
  opts: GlobalInboxOpts = {},
): Promise<GlobalInboxRow[]> {
  const limit = Math.min(opts.limit ?? 100, 200);
  const statuses =
    opts.status && opts.status.length > 0
      ? opts.status
      : ["pending", "processing", "failed", "dead"];
  const { rows } = await pool.query(
    `
    SELECT i.id, i.agent_id, a.handle AS agent_handle,
           i.from_kind, i.from_id, i.status,
           i.origin_channel, i.origin_user_id,
           i.content, i.enqueued_at
    FROM agent_inbox i
    JOIN agents a ON a.id = i.agent_id
    WHERE i.status = ANY($1::text[])
      AND i.from_kind NOT IN ('user', 'agent', 'system')
    ORDER BY i.enqueued_at DESC
    LIMIT ${limit}
    `,
    [statuses],
  );
  return rows.map((r) => ({
    id: r.id,
    agent_id: r.agent_id,
    agent_handle: r.agent_handle,
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

// listConversations: one row per chat thread, merging inbox + outbox.
// Threads are keyed by conversation_id when set (multi-agent a2a
// handoffs) and fall back to agent_id when it's null (single-agent
// Telegram topic chats — the common case). Without that fallback
// every Telegram-originated message would be silently dropped from
// the feed.
//
// The `conversation_id` column on the row stays null for the fallback
// case so the UI can deep-link into the agent's cross-conversation
// timeline (no specific conversation filter).
export async function listConversations(
  pool: Pool,
  limit = 50,
): Promise<ConversationRow[]> {
  const lim = Math.min(limit, 200);
  const { rows } = await pool.query(
    `
    WITH last_msg AS (
      SELECT
        COALESCE(conversation_id::text, agent_id)    AS thread_key,
        conversation_id,
        agent_id,
        MAX(at)                                      AS last_at,
        (ARRAY_AGG(content ORDER BY at DESC))[1]     AS preview,
        COUNT(*)::int                                AS msg_count,
        COALESCE(SUM(pending),0)::int                AS pending_count
      FROM (
        SELECT conversation_id, agent_id, content,
               enqueued_at AS at,
               CASE WHEN status IN ('pending','processing')
                    THEN 1 ELSE 0 END AS pending
        FROM agent_inbox
        UNION ALL
        SELECT conversation_id, agent_id, content,
               created_at AS at,
               0 AS pending
        FROM agent_outbox
      ) m
      GROUP BY thread_key, conversation_id, agent_id
    )
    SELECT lm.thread_key, lm.conversation_id, lm.agent_id,
           a.handle AS agent_handle,
           lm.last_at, lm.preview,
           lm.msg_count, lm.pending_count
    FROM last_msg lm
    JOIN agents a ON a.id = lm.agent_id
    ORDER BY lm.last_at DESC
    LIMIT ${lim}
    `,
  );
  return rows.map((r) => ({
    // thread_key is what the UI keys on; conversation_id remains
    // null for single-agent threads so the row links to the agent's
    // cross-conversation timeline rather than a non-existent
    // conversation filter.
    conversation_id: r.conversation_id,
    thread_key: r.thread_key,
    agent_id: r.agent_id,
    agent_handle: r.agent_handle,
    last_at: r.last_at.toISOString
      ? r.last_at.toISOString()
      : String(r.last_at),
    preview: excerptFromContent(r.preview),
    msg_count: Number(r.msg_count) || 0,
    pending_count: Number(r.pending_count) || 0,
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
    excerpt: excerptFromContent(r.content, 20000),
    at: r.at.toISOString ? r.at.toISOString() : String(r.at),
  }));
}

// computeKPIs aggregates fleet-wide counters for the KPI strip.
// Single round-trip via CTEs; safe to call from a Server Component.
export async function computeKPIs(pool: Pool): Promise<KPIs> {
  const { rows } = await pool.query(`
    WITH
      a AS (
        SELECT COUNT(*) FILTER (WHERE status = 'working') AS active,
               COUNT(*)                                   AS total
        FROM agents
        WHERE status <> 'archived' OR status IS NULL
      ),
      inbox AS (
        SELECT COUNT(*) AS in_flight
        FROM agent_inbox
        WHERE status IN ('pending','processing')
      ),
      outbox AS (
        SELECT COUNT(*) AS pending
        FROM agent_outbox
        WHERE status = 'pending'
      ),
      tc AS (
        SELECT
          COALESCE(SUM(input_tokens), 0)::bigint  AS input_today,
          COALESCE(SUM(output_tokens), 0)::bigint AS output_today,
          COALESCE(SUM(input_usd_cents + output_usd_cents), 0)::int AS cents_today
        FROM agent_turn_costs
        WHERE finished_at >= date_trunc('day', NOW())
      ),
      tc_month AS (
        SELECT COALESCE(SUM(input_usd_cents + output_usd_cents), 0)::int AS cents_month
        FROM agent_turn_costs
        WHERE finished_at >= date_trunc('month', NOW())
      ),
      donut AS (
        SELECT model,
               COALESCE(SUM(input_usd_cents + output_usd_cents), 0)::int AS cents
        FROM agent_turn_costs
        WHERE finished_at >= date_trunc('day', NOW())
        GROUP BY model
      )
    SELECT
      a.active, a.total, inbox.in_flight, outbox.pending,
      tc.input_today, tc.output_today, tc.cents_today,
      tc_month.cents_month,
      COALESCE((SELECT json_agg(row_to_json(donut)) FROM donut), '[]') AS by_model
    FROM a, inbox, outbox, tc, tc_month
  `);
  const r = rows[0];
  const now = new Date();
  // Linear projection for the rest of the month based on cents_today.
  const startOfMonth = new Date(now.getFullYear(), now.getMonth(), 1);
  const daysElapsed = Math.max(
    1,
    Math.ceil((now.getTime() - startOfMonth.getTime()) / 86_400_000),
  );
  const daysInMonth = new Date(
    now.getFullYear(),
    now.getMonth() + 1,
    0,
  ).getDate();
  const projection = Math.round((r.cents_month / daysElapsed) * daysInMonth);

  return {
    active_agents: Number(r.active) || 0,
    total_agents: Number(r.total) || 0,
    inbox_in_flight: Number(r.in_flight) || 0,
    outbox_pending: Number(r.pending) || 0,
    tokens_today: {
      input: Number(r.input_today) || 0,
      output: Number(r.output_today) || 0,
    },
    cost_today_cents: Number(r.cents_today) || 0,
    cost_month_projected_cents: projection,
    cost_by_model: Array.isArray(r.by_model)
      ? r.by_model.map((m: { model: string; cents: number }) => ({
          model: m.model,
          cents: Number(m.cents) || 0,
        }))
      : [],
  };
}

export async function listJobs(pool: Pool): Promise<JobsList> {
  const [{ rows: scheduled }, { rows: webhooks }] = await Promise.all([
    pool.query(`
      SELECT id, name, cron_expr, timezone, agent_id, enabled,
             next_run_at, last_run_at
      FROM scheduled_jobs
      ORDER BY enabled DESC, next_run_at ASC
    `),
    pool.query(`
      SELECT id, name, path, agent_id, enabled, rate_limit_per_min
      FROM webhook_handlers
      ORDER BY enabled DESC, name ASC
    `),
  ]);
  return {
    scheduled: scheduled.map((r) => ({
      id: r.id,
      name: r.name,
      kind: "scheduled" as const,
      cron_expr: r.cron_expr,
      timezone: r.timezone,
      agent_id: r.agent_id,
      enabled: r.enabled,
      next_run_at: r.next_run_at.toISOString
        ? r.next_run_at.toISOString()
        : String(r.next_run_at),
      last_run_at: r.last_run_at
        ? r.last_run_at.toISOString
          ? r.last_run_at.toISOString()
          : String(r.last_run_at)
        : null,
    })),
    webhooks: webhooks.map((r) => ({
      id: r.id,
      name: r.name,
      kind: "webhook" as const,
      path: r.path,
      agent_id: r.agent_id,
      enabled: r.enabled,
      rate_limit_per_min: r.rate_limit_per_min,
    })),
  };
}

// listAgentTimeline: cross-conversation flat merge of inbox + outbox
// for a single agent, newest-last. Used by the agent-detail page's
// Conversation tab when no specific conversation is selected.
export async function listAgentTimeline(
  pool: Pool,
  agentId: string,
  limit = 100,
): Promise<ConversationItem[]> {
  const { rows } = await pool.query(
    `
    (
      SELECT 'inbox' AS kind, id, agent_id, from_kind, content,
             enqueued_at AS at
      FROM agent_inbox
      WHERE agent_id = $1
      ORDER BY enqueued_at DESC LIMIT ${Math.min(limit, 500)}
    )
    UNION ALL
    (
      SELECT 'outbox' AS kind, id, agent_id, NULL AS from_kind, content,
             created_at AS at
      FROM agent_outbox
      WHERE agent_id = $1
      ORDER BY created_at DESC LIMIT ${Math.min(limit, 500)}
    )
    ORDER BY at ASC
    `,
    [agentId],
  );
  return rows.map((r) => ({
    kind: r.kind,
    id: r.id,
    agent_id: r.agent_id,
    from_kind: r.from_kind,
    excerpt: excerptFromContent(r.content, 20000),
    at: r.at.toISOString ? r.at.toISOString() : String(r.at),
  }));
}
