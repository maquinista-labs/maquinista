import { NextResponse } from "next/server";

import { writeAudit } from "@/lib/audit";
import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// PATCH /api/agents/:id/workspaces/:wsId — activate an existing
// workspace. The wsId in the URL is the full "<agent>@<label>"
// primary key. No body required.
//
//   404  agent or workspace not found
//   409  workspace is archived / doesn't belong to this agent
export async function PATCH(
  _req: Request,
  ctx: { params: Promise<{ id: string; wsId: string }> },
) {
  const { id, wsId } = await ctx.params;
  const pool = getPool();

  const wsRes = await pool.query<{
    agent_id: string;
    archived_at: string | null;
  }>(
    `SELECT agent_id, archived_at FROM agent_workspaces WHERE id=$1`,
    [wsId],
  );
  if (wsRes.rowCount === 0) {
    return NextResponse.json({ error: "workspace_not_found" }, { status: 404 });
  }
  const row = wsRes.rows[0];
  if (row.agent_id !== id) {
    return NextResponse.json(
      { error: "workspace_owned_by_other_agent", owner: row.agent_id },
      { status: 409 },
    );
  }
  if (row.archived_at) {
    return NextResponse.json({ error: "workspace_archived" }, { status: 409 });
  }

  const upd = await pool.query(
    `UPDATE agents SET active_workspace_id=$1 WHERE id=$2`,
    [wsId, id],
  );
  if (upd.rowCount === 0) {
    return NextResponse.json({ error: "agent_not_found" }, { status: 404 });
  }

  await writeAudit(pool, {
    action: "agent.workspace.switched",
    subject: { agent_id: id, workspace_id: wsId },
    ok: true,
    error: null,
  });

  return NextResponse.json(
    { id, active_workspace_id: wsId },
    { status: 200 },
  );
}

// DELETE /api/agents/:id/workspaces/:wsId — soft-delete (set
// archived_at). Refuses when the target is the currently-active
// workspace — the operator must switch away first.
//
//   404  workspace not found
//   409  workspace is active / owned by a different agent
export async function DELETE(
  _req: Request,
  ctx: { params: Promise<{ id: string; wsId: string }> },
) {
  const { id, wsId } = await ctx.params;
  const pool = getPool();

  const agentRes = await pool.query<{ active_workspace_id: string | null }>(
    `SELECT active_workspace_id FROM agents WHERE id=$1`,
    [id],
  );
  if (agentRes.rowCount === 0) {
    return NextResponse.json({ error: "agent_not_found" }, { status: 404 });
  }
  if (agentRes.rows[0].active_workspace_id === wsId) {
    return NextResponse.json(
      { error: "workspace_active", hint: "switch to a different workspace first" },
      { status: 409 },
    );
  }

  const res = await pool.query<{ agent_id: string }>(
    `UPDATE agent_workspaces
        SET archived_at = NOW()
      WHERE id=$1 AND agent_id=$2 AND archived_at IS NULL
      RETURNING agent_id`,
    [wsId, id],
  );
  if (res.rowCount === 0) {
    return NextResponse.json(
      { error: "workspace_not_found_or_archived" },
      { status: 404 },
    );
  }

  await writeAudit(pool, {
    action: "agent.workspace.archived",
    subject: { agent_id: id, workspace_id: wsId },
    ok: true,
    error: null,
  });

  return NextResponse.json({ id, workspace_id: wsId }, { status: 200 });
}
