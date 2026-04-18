import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { listConversations } from "@/lib/queries";

// GET /api/conversations — cross-agent chat feed, one row per
// (conversation_id, agent_id). Replaces the separate /inbox + old
// /conversations placeholders with a single surface per operator
// feedback.
export const dynamic = "force-dynamic";
export const revalidate = 0;

export async function GET(req: Request) {
  const url = new URL(req.url);
  const limitParam = url.searchParams.get("limit");
  const limit = limitParam ? Math.max(1, Number(limitParam)) : 50;
  try {
    const items = await listConversations(getPool(), limit);
    return NextResponse.json(
      { items },
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
