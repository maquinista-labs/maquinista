// Postgres helper used by specs to seed / clean / insert rows in
// the fixture DB that global-setup launched.

import { readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { Client } from "pg";

const STATE_FILE = path.join(tmpdir(), "maquinista-e2e-state.json");

// Worker processes don't inherit env vars mutated by globalSetup
// after they've spawned. Read pgUrl from the state file instead —
// globalSetup writes it there synchronously before any spec runs.
function resolvePgUrl(): string | undefined {
  if (process.env.MAQUINISTA_DASHBOARD_PG_URL) {
    return process.env.MAQUINISTA_DASHBOARD_PG_URL;
  }
  try {
    const state = JSON.parse(readFileSync(STATE_FILE, "utf8"));
    return state.pgUrl;
  } catch {
    return undefined;
  }
}

export function pgUrlFromState(): string | undefined {
  return resolvePgUrl();
}

/**
 * withDb(fn) opens a dedicated pg Client pointed at the fixture DB
 * for the duration of fn. The URL comes from MAQUINISTA_DASHBOARD_PG_URL
 * (set by global-setup for the main process) or, for spawned
 * workers that don't inherit the mutation, from the state file
 * global-setup wrote.
 */
export async function withDb<T>(fn: (client: Client) => Promise<T>): Promise<T> {
  const url = resolvePgUrl();
  if (!url) {
    throw new Error(
      "Postgres fixture URL unavailable (state file + env both unset)",
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
    await c.query("TRUNCATE TABLE agent_turn_costs CASCADE");
    await c.query("TRUNCATE TABLE agent_outbox CASCADE");
    await c.query("TRUNCATE TABLE agent_inbox CASCADE");
    await c.query("TRUNCATE TABLE scheduled_jobs CASCADE");
    await c.query("TRUNCATE TABLE webhook_handlers CASCADE");
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

/** insertTurnCost writes one agent_turn_costs row. */
export async function insertTurnCost(args: {
  agentId: string;
  model?: string;
  inputTokens?: number;
  outputTokens?: number;
  inputUsdCents?: number;
  outputUsdCents?: number;
  finishedAt?: Date;
}): Promise<number> {
  const {
    agentId,
    model = "test-model",
    inputTokens = 0,
    outputTokens = 0,
    inputUsdCents = 0,
    outputUsdCents = 0,
    finishedAt = new Date(),
  } = args;
  let id = 0;
  await withDb(async (c) => {
    const r = await c.query(
      `INSERT INTO agent_turn_costs
         (agent_id, model, input_tokens, output_tokens,
          input_usd_cents, output_usd_cents, started_at, finished_at)
       VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
       RETURNING id`,
      [
        agentId,
        model,
        inputTokens,
        outputTokens,
        inputUsdCents,
        outputUsdCents,
        finishedAt,
      ],
    );
    id = r.rows[0].id;
  });
  return id;
}

/** insertScheduledJob writes one scheduled_jobs row. */
export async function insertScheduledJob(args: {
  name: string;
  agentId: string;
  cronExpr?: string;
  enabled?: boolean;
}): Promise<string> {
  const {
    name,
    agentId,
    cronExpr = "0 * * * *",
    enabled = true,
  } = args;
  let id = "";
  await withDb(async (c) => {
    const r = await c.query(
      `INSERT INTO scheduled_jobs
         (name, cron_expr, agent_id, prompt, enabled, next_run_at)
       VALUES ($1,$2,$3,$4,$5,$6)
       RETURNING id`,
      [
        name,
        cronExpr,
        agentId,
        JSON.stringify({ text: "placeholder" }),
        enabled,
        new Date(Date.now() + 3_600_000),
      ],
    );
    id = r.rows[0].id;
  });
  return id;
}

/** insertWebhookHandler writes one webhook_handlers row. */
export async function insertWebhookHandler(args: {
  name: string;
  agentId: string;
  path?: string;
  enabled?: boolean;
}): Promise<string> {
  const { name, agentId, path = `/hook/${name}`, enabled = true } = args;
  let id = "";
  await withDb(async (c) => {
    const r = await c.query(
      `INSERT INTO webhook_handlers
         (name, path, secret, agent_id, prompt_template, enabled)
       VALUES ($1,$2,'seed-secret',$3,'{{ payload }}',$4)
       RETURNING id`,
      [name, path, agentId, enabled],
    );
    id = r.rows[0].id;
  });
  return id;
}
