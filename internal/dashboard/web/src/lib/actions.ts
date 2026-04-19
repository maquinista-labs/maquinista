// Server-side helpers for Phase 5 action endpoints. Each helper
// is small (one INSERT/UPDATE) and pure DB-level so the Route
// Handlers stay thin. Action rows go to agent_inbox with
// origin_channel='dashboard' so the sidecar consumes them the same
// way as Telegram messages.

import type { Pool } from "pg";

import { isValidHandle } from "@/lib/utils";

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

// renameAgent sets (or clears) the agent's `handle` — the operator-
// facing display name. `id` stays untouched (referenced by mailbox
// rows, soul rows, tmux names). Callers validate the regex; this
// helper just runs the UPDATE and surfaces the outcome.
//
// Return values:
//   "updated" — row exists, handle written.
//   "not_found" — no row matched the id.
//   "conflict" — unique index on lower(handle) rejected the value.
export type RenameAgentResult = "updated" | "not_found" | "conflict";

export async function renameAgent(
  pool: Pool,
  id: string,
  handle: string | null,
): Promise<RenameAgentResult> {
  try {
    const { rowCount } = await pool.query(
      `UPDATE agents SET handle = $2 WHERE id = $1`,
      [id, handle],
    );
    return (rowCount ?? 0) > 0 ? "updated" : "not_found";
  } catch (err) {
    // Postgres unique violation is 23505.
    if (
      typeof err === "object" &&
      err !== null &&
      "code" in err &&
      (err as { code: string }).code === "23505"
    ) {
      return "conflict";
    }
    throw err;
  }
}

// spawnAgentFromDashboard creates a fresh agents row + cloned soul.
// Uses the operator-entered handle as the stable id (agents.id is a
// free-form TEXT pk; handle already matches the required regex).
//
// Phase-1 of G.5 — Telegram topic creation is deferred (expose
// bot API across process boundary is out of scope for v1). The
// agent comes up in the fleet and is reachable from the dashboard;
// @mention routing from Telegram lands on the agent via the
// existing handle-lookup path.
//
// Outcomes mirror renameAgent: returns either {kind:"created", id}
// or a discriminated error for UX copy.
export type SpawnAgentResult =
  | { kind: "created"; id: string }
  | { kind: "invalid_handle" }
  | { kind: "invalid_runner" }
  | { kind: "invalid_model" }
  | { kind: "invalid_soul_template" }
  | { kind: "handle_taken"; handle: string };

export type SpawnAgentSpec = {
  handle: string;
  runner: string;
  model: string | null;
  soulTemplateID: string;
  cwd: string;
};

export async function spawnAgentFromDashboard(
  pool: Pool,
  spec: SpawnAgentSpec,
): Promise<SpawnAgentResult> {
  if (!isValidHandle(spec.handle)) {
    return { kind: "invalid_handle" };
  }

  const soulCheck = await pool.query(
    `SELECT 1 FROM soul_templates WHERE id = $1 LIMIT 1`,
    [spec.soulTemplateID],
  );
  if (soulCheck.rows.length === 0) {
    return { kind: "invalid_soul_template" };
  }

  const client = await pool.connect();
  try {
    await client.query("BEGIN");

    const taken = await client.query(
      `SELECT 1 FROM agents WHERE lower(handle) = lower($1) LIMIT 1`,
      [spec.handle],
    );
    if (taken.rows.length > 0) {
      await client.query("ROLLBACK");
      return { kind: "handle_taken", handle: spec.handle };
    }

    try {
      await client.query(
        `INSERT INTO agents
           (id, handle, tmux_session, tmux_window, role, status,
            runner_type, model, cwd, window_name,
            started_at, last_seen, stop_requested, workspace_scope)
         VALUES
           ($1, $1, 'maquinista', '', 'user', 'stopped',
            $2, $3, $4, $1,
            NOW(), NOW(), FALSE, 'shared')`,
        [spec.handle, spec.runner, spec.model, spec.cwd],
      );
    } catch (err) {
      await client.query("ROLLBACK");
      if (
        typeof err === "object" &&
        err !== null &&
        "code" in err &&
        (err as { code: string }).code === "23505"
      ) {
        return { kind: "handle_taken", handle: spec.handle };
      }
      throw err;
    }

    // Clone the soul template into agent_souls. The SQL mirrors
    // soul.CreateFromTemplate in Go — keep in sync if that signature
    // changes.
    await client.query(
      `INSERT INTO agent_souls
         (agent_id, template_id, name, tagline, role, goal,
          core_truths, boundaries, vibe, continuity, extras,
          allow_delegation, max_iter)
       SELECT $1, id, name, tagline, role, goal,
              core_truths, boundaries, vibe, continuity, extras,
              allow_delegation, max_iter
       FROM soul_templates WHERE id = $2`,
      [spec.handle, spec.soulTemplateID],
    );

    await client.query("COMMIT");
    return { kind: "created", id: spec.handle };
  } catch (err) {
    try {
      await client.query("ROLLBACK");
    } catch {
      /* best effort */
    }
    throw err;
  } finally {
    client.release();
  }
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
     SET tmux_window = '', stop_requested = FALSE, session_id = ''
     WHERE id = $1`,
    [agentId],
  );
  return (rowCount ?? 0) > 0;
}
