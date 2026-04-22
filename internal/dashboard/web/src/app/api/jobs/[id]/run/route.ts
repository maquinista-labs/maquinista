import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";

// POST /api/jobs/[id]/run — trigger immediate execution by making next_run_at
// just in the past. The scheduler (polling every 30s) will pick it up.
export async function POST(
  _req: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  try {
    const { id } = await params;
    const pool = getPool();
    const { rowCount } = await pool.query(
      `UPDATE scheduled_jobs
       SET next_run_at = NOW() - INTERVAL '1 second'
       WHERE id = $1`,
      [id],
    );
    if (rowCount === 0) {
      return NextResponse.json({ error: "not found" }, { status: 404 });
    }
    return NextResponse.json({ ok: true, queued: true });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
