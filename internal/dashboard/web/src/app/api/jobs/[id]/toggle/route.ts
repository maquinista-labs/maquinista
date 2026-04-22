import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";

// PATCH /api/jobs/[id]/toggle — enable or disable a job
export async function PATCH(
  req: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  try {
    const { id } = await params;
    const { enabled } = (await req.json()) as { enabled: boolean };

    if (typeof enabled !== "boolean") {
      return NextResponse.json(
        { error: "enabled (boolean) required" },
        { status: 400 },
      );
    }

    const pool = getPool();
    const { rowCount } = await pool.query(
      `UPDATE scheduled_jobs SET enabled = $2 WHERE id = $1`,
      [id, enabled],
    );
    if (rowCount === 0) {
      return NextResponse.json({ error: "not found" }, { status: 404 });
    }
    return NextResponse.json({ ok: true, enabled });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
