import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// POST /api/agents/:id/archive — set status='archived' so agent
// won't respawn on restart. Keeps history and bindings intact.
export async function POST(
  _req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  try {
    const pool = getPool();
    const result = await pool.query(
      `UPDATE agents
       SET status = 'archived', stop_requested = TRUE, last_seen = NOW()
       WHERE id = $1 AND role = 'user' AND task_id IS NULL
       RETURNING id`,
      [id],
    );
    if (result.rows.length === 0) {
      return NextResponse.json(
        { error: "agent not found or not a persistent user agent", id },
        { status: 404 },
      );
    }
    return NextResponse.json({ ok: true, id: result.rows[0].id });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
