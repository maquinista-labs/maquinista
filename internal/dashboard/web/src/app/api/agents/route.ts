import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { listAgents } from "@/lib/queries";

// GET /api/agents — list every non-archived agent, each with latest
// outbox excerpt + unread inbox count. Phase 2 of
// plans/active/dashboard.md.
export const dynamic = "force-dynamic";
export const revalidate = 0;

export async function GET() {
  try {
    const pool = getPool();
    const agents = await listAgents(pool);
    return NextResponse.json(
      { agents },
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
