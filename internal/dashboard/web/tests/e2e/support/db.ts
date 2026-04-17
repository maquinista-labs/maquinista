// Postgres helper used by specs to seed / clean / insert rows in
// the fixture DB that global-setup launched.

import { Client } from "pg";

/**
 * withDb(fn) opens a dedicated pg Client pointed at the fixture DB
 * for the duration of fn. The URL is passed via
 * MAQUINISTA_DASHBOARD_PG_URL by global-setup.
 */
export async function withDb<T>(fn: (client: Client) => Promise<T>): Promise<T> {
  const url = process.env.MAQUINISTA_DASHBOARD_PG_URL;
  if (!url) {
    throw new Error(
      "MAQUINISTA_DASHBOARD_PG_URL not set (global-setup failed to spin up Postgres?)",
    );
  }
  const client = new Client({ connectionString: url });
  await client.connect();
  try {
    return await fn(client);
  } finally {
    await client.end();
  }
}

/**
 * cleanTables wipes the mailbox tables + agents in an order safe for
 * FK cascades. Used before each spec that expects a pristine DB.
 */
export async function cleanTables(): Promise<void> {
  await withDb(async (c) => {
    await c.query("TRUNCATE TABLE agent_outbox CASCADE");
    await c.query("TRUNCATE TABLE agent_inbox CASCADE");
    await c.query("TRUNCATE TABLE agent_settings CASCADE");
    await c.query("TRUNCATE TABLE agents CASCADE");
  });
}

/**
 * insertAgent writes a minimal agents + agent_settings row pair.
 * Returns the agent id so specs can chain assertions.
 */
export async function insertAgent(params: {
  id: string;
  runnerType?: string;
  role?: string;
  status?: "idle" | "working" | "dead";
  lastSeen?: Date | null;
  tmuxWindow?: string | null;
  stopRequested?: boolean;
  persona?: string | null;
  handle?: string | null;
}): Promise<string> {
  const {
    id,
    runnerType = "claude",
    role = "executor",
    status = "working",
    lastSeen = new Date(),
    tmuxWindow = "0:1",
    stopRequested = false,
    persona = null,
    handle = null,
  } = params;
  await withDb(async (c) => {
    await c.query(
      `INSERT INTO agents
         (id, tmux_session, tmux_window, status, runner_type, role,
          last_seen, stop_requested, handle)
       VALUES ($1,'maquinista',$2,$3,$4,$5,$6,$7,$8)
       ON CONFLICT (id) DO UPDATE SET
         status=EXCLUDED.status, last_seen=EXCLUDED.last_seen,
         tmux_window=EXCLUDED.tmux_window,
         stop_requested=EXCLUDED.stop_requested`,
      [
        id,
        tmuxWindow ?? "",
        status,
        runnerType,
        role,
        lastSeen,
        stopRequested,
        handle,
      ],
    );
    await c.query(
      `INSERT INTO agent_settings (agent_id, persona, roster)
       VALUES ($1, $2, '[]'::jsonb)
       ON CONFLICT (agent_id) DO UPDATE SET persona=EXCLUDED.persona`,
      [id, persona],
    );
  });
  return id;
}

/** insertOutbox writes one agent_outbox row and returns the id. */
export async function insertOutbox(
  agentId: string,
  text: string,
): Promise<string> {
  let id = "";
  await withDb(async (c) => {
    const r = await c.query(
      `INSERT INTO agent_outbox (agent_id, content)
       VALUES ($1, $2)
       RETURNING id`,
      [agentId, JSON.stringify({ text })],
    );
    id = r.rows[0].id;
  });
  return id;
}

/** insertInbox writes one agent_inbox row in pending state. */
export async function insertInbox(
  agentId: string,
  text: string,
): Promise<string> {
  let id = "";
  await withDb(async (c) => {
    const r = await c.query(
      `INSERT INTO agent_inbox
         (agent_id, from_kind, content, status)
       VALUES ($1, 'user', $2, 'pending')
       RETURNING id`,
      [agentId, JSON.stringify({ text })],
    );
    id = r.rows[0].id;
  });
  return id;
}
