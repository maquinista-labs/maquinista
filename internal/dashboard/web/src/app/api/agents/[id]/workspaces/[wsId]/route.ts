import { NextResponse } from "next/server";

import { writeAudit } from "@/lib/audit";
import { requestRespawn } from "@/lib/actions";
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

  // Respawn so the agent immediately starts in the new worktree.
  await requestRespawn(pool, id);

  return NextResponse.json(
    { id, active_workspace_id: wsId },
    { status: 200 },
  );
}

// DELETE /api/agents/:id/workspaces/:wsId — archive a workspace.
// When the workspace is currently active, also clears active_workspace_id
// and resets the agent to shared (no git isolation), then triggers a
// respawn so the agent comes back without a worktree.
//
//   404  workspace not found / already archived / owned by another agent
export async function DELETE(
  _req: Request,
  ctx: { params: Promise<{ id: string; wsId: string }> },
) {
  const { id, wsId } = await ctx.params;
  const pool = getPool();

  const client = await pool.connect();
  let wasActive = false;
  try {
    await client.query("BEGIN");

    // Archive the workspace — fail fast if not found / wrong agent.
    const archRes = await client.query<{ agent_id: string }>(
      `UPDATE agent_workspaces
          SET archived_at = NOW()
        WHERE id=$1 AND agent_id=$2 AND archived_at IS NULL
        RETURNING agent_id`,
      [wsId, id],
    );
    if (archRes.rowCount === 0) {
      await client.query("ROLLBACK");
      return NextResponse.json(
        { error: "workspace_not_found_or_archived" },
        { status: 404 },
      );
    }

    // If this was the active workspace, detach and revert to shared scope.
    // The sync_active_workspace trigger only fires on non-NULL values, so
    // we reset workspace_scope + workspace_repo_root explicitly here.
    const detachRes = await client.query(
      `UPDATE agents
          SET active_workspace_id = NULL,
              workspace_scope      = 'shared',
              workspace_repo_root  = NULL
        WHERE id=$1 AND active_workspace_id=$2`,
      [id, wsId],
    );
    wasActive = (detachRes.rowCount ?? 0) > 0;

    await client.query("COMMIT");
  } catch (err) {
    await client.query("ROLLBACK");
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  } finally {
    client.release();
  }

  await writeAudit(pool, {
    action: "agent.workspace.archived",
    subject: { agent_id: id, workspace_id: wsId, was_active: wasActive },
    ok: true,
    error: null,
  });

  // If we detached the active workspace, respawn into the default cwd
  // (no worktree — shared scope).
  if (wasActive) {
    await requestRespawn(pool, id);
  }

  return NextResponse.json({ id, workspace_id: wsId, was_active: wasActive }, { status: 200 });
}
