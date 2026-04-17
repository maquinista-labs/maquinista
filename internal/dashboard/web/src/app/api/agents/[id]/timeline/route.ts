import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { listAgentTimeline } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// GET /api/agents/:id/timeline — cross-conversation flat merge of
// inbox + outbox for the agent's Conversation tab.
export async function GET(
  _req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  try {
    const items = await listAgentTimeline(getPool(), id);
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
