import { NextResponse } from "next/server";

import { writeAudit } from "@/lib/audit";
import { getPool } from "@/lib/db";
import { requestRespawn } from "@/lib/actions";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// Label shape matches what agent_workspaces.id expects as a suffix
// after "<agent>@". Kept permissive (dots/underscores/dashes +
// alphanumerics) so repo basenames like "project-a_v2" fit.
const LABEL_RE = /^[A-Za-z0-9._-]+$/;

type WorkspaceRow = {
  id: string;
  agent_id: string;
  scope: "shared" | "agent" | "task";
  repo_root: string;
  worktree_dir: string | null;
  branch: string | null;
  created_at: string;
};

// GET /api/agents/:id/workspaces — non-archived workspaces plus the
// id of the currently-active one. The UI uses this to render the
// list with a ★ marker and to disable "archive" on the active row.
export async function GET(
  _req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  const pool = getPool();

  const agentRes = await pool.query<{ active_workspace_id: string | null }>(
    `SELECT active_workspace_id FROM agents WHERE id=$1`,
    [id],
  );
  if (agentRes.rowCount === 0) {
    return NextResponse.json({ error: "not_found" }, { status: 404 });
  }

  const wsRes = await pool.query<WorkspaceRow>(
    `SELECT id, agent_id, scope, repo_root, worktree_dir, branch, created_at
       FROM agent_workspaces
      WHERE agent_id=$1 AND archived_at IS NULL
      ORDER BY created_at ASC`,
    [id],
  );

  return NextResponse.json(
    {
      active_workspace_id: agentRes.rows[0].active_workspace_id,
      workspaces: wsRes.rows,
    },
    { headers: { "Cache-Control": "no-store" } },
  );
}

// POST /api/agents/:id/workspaces — create a workspace + activate it.
//   body: { label, scope?, repo_root? }
//   400 invalid body / missing repo for scope=agent|task / bad label
//   404 agent not found
//   409 label collision
export async function POST(
  req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;

  let body: { label?: unknown; scope?: unknown; repo_root?: unknown };
  try {
    body = await req.json();
  } catch {
    return NextResponse.json({ error: "invalid_json" }, { status: 400 });
  }

  const label = typeof body.label === "string" ? body.label.trim() : "";
  if (!label || !LABEL_RE.test(label)) {
    return NextResponse.json({ error: "invalid_label" }, { status: 400 });
  }
  // Workspace creation always uses agent scope — per-agent git isolation
  // is the only meaningful choice from the dashboard. shared is the
  // no-workspace default; task is only useful for automated /t_auto flows.
  const scope = "agent";
  const repoRoot =
    typeof body.repo_root === "string" ? body.repo_root.trim() : "";
  if (!repoRoot) {
    return NextResponse.json({ error: "repo_root_required" }, { status: 400 });
  }

  const pool = getPool();

  // Agent must exist.
  const agentRes = await pool.query<{ id: string }>(
    `SELECT id FROM agents WHERE id=$1`,
    [id],
  );
  if (agentRes.rowCount === 0) {
    return NextResponse.json({ error: "not_found" }, { status: 404 });
  }

  const wsId = `${id}@${label}`;
  // worktree_dir + branch must match ResolveLayout / migration-028
  // backfill SQL. Keep the three formulas in lockstep.
  const worktreeDir = `${repoRoot}/.maquinista/worktrees/${scope}/${id}`;
  const branch = `maquinista/${scope}/${id}`;

  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    await client.query(
      `INSERT INTO agent_workspaces (id, agent_id, scope, repo_root, worktree_dir, branch)
       VALUES ($1, $2, $3, $4, $5, $6)`,
      [wsId, id, scope, repoRoot, worktreeDir, branch],
    );
    await client.query(
      `UPDATE agents SET active_workspace_id=$1 WHERE id=$2`,
      [wsId, id],
    );
    await client.query("COMMIT");
  } catch (err) {
    await client.query("ROLLBACK");
    const msg = err instanceof Error ? err.message : String(err);
    if (msg.includes("duplicate key") || msg.includes("unique constraint")) {
      return NextResponse.json(
        { error: "label_taken", workspace_id: wsId },
        { status: 409 },
      );
    }
    if (msg.includes("agent_workspaces_worktree_scope_chk")) {
      return NextResponse.json({ error: "invalid_shape" }, { status: 400 });
    }
    return NextResponse.json({ error: msg }, { status: 500 });
  } finally {
    client.release();
  }

  await writeAudit(pool, {
    action: "agent.workspace.created",
    subject: { agent_id: id, workspace_id: wsId, scope, repo_root: repoRoot },
    ok: true,
    error: null,
  });

  // Trigger immediate respawn so the reconciler picks up the new workspace
  // and creates the git worktree on its next tick — no manual restart needed.
  await requestRespawn(pool, id);

  return NextResponse.json(
    {
      workspace: {
        id: wsId,
        agent_id: id,
        scope,
        repo_root: repoRoot,
        worktree_dir: worktreeDir,
        branch,
      },
      active_workspace_id: wsId,
    },
    { status: 201 },
  );
}
