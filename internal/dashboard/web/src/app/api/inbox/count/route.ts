import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// GET /api/inbox/count — returns total pending+processing inbox rows
// across all agents. Used by the bottom nav badge.
export async function GET() {
  try {
    const { rows } = await getPool().query(`
      SELECT COUNT(*)::int AS count
      FROM agent_inbox
      WHERE status IN ('pending', 'processing')
    `);
    return NextResponse.json(
      { count: Number(rows[0]?.count) || 0 },
      { headers: { "Cache-Control": "no-store" } },
    );
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return NextResponse.json(
      { error: msg },
      { status: 500, headers: { "Cache-Control": "no-store" } },
    );
  }
}
