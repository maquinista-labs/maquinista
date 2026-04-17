// Server-side helpers for Phase 5 action endpoints. Each helper
// is small (one INSERT/UPDATE) and pure DB-level so the Route
// Handlers stay thin. Action rows go to agent_inbox with
// origin_channel='dashboard' so the sidecar consumes them the same
// way as Telegram messages.

import type { Pool } from "pg";

export async function enqueueInboxFromDashboard(
  pool: Pool,
  args: {
    agentId: string;
    text: string;
    operator?: string | null;
  },
): Promise<string> {
  const { agentId, text, operator } = args;
  const { rows } = await pool.query(
    `INSERT INTO agent_inbox
       (agent_id, from_kind, origin_channel, origin_user_id, content, status)
     VALUES ($1, 'user', 'dashboard', $2, $3, 'pending')
     RETURNING id`,
    [agentId, operator ?? null, JSON.stringify({ text })],
  );
  return rows[0].id as string;
}

export async function enqueueInterrupt(
  pool: Pool,
  args: { agentId: string; operator?: string | null },
): Promise<string> {
  const { rows } = await pool.query(
    `INSERT INTO agent_inbox
       (agent_id, from_kind, origin_channel, origin_user_id, content, status)
     VALUES ($1, 'system', 'dashboard', $2, $3, 'pending')
     RETURNING id`,
    [
      args.agentId,
      args.operator ?? null,
      JSON.stringify({ control: "interrupt" }),
    ],
  );
  return rows[0].id as string;
}

// requestKill marks the agent for graceful shutdown. The supervisor
// reconcile loop is the reader; this is fire-and-forget.
export async function requestKill(
  pool: Pool,
  agentId: string,
): Promise<boolean> {
  const { rowCount } = await pool.query(
    `UPDATE agents SET stop_requested = TRUE WHERE id = $1`,
    [agentId],
  );
  return (rowCount ?? 0) > 0;
}

// requestRespawn clears tmux_window and stop_requested so the
// reconcile loop starts a fresh pane on its next tick. agents.tmux_window
// is NOT NULL, so we write empty string (convention for "no active pane").
export async function requestRespawn(
  pool: Pool,
  agentId: string,
): Promise<boolean> {
  const { rowCount } = await pool.query(
    `UPDATE agents
     SET tmux_window = '', stop_requested = FALSE
     WHERE id = $1`,
    [agentId],
  );
  return (rowCount ?? 0) > 0;
}
