import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { getAgent } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// GET /api/agents/:id — single agent detail. 404 if absent.
export async function GET(
  _req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  try {
    const agent = await getAgent(getPool(), id);
    if (!agent) {
      return NextResponse.json(
        { error: "agent not found", id },
        { status: 404, headers: { "Cache-Control": "no-store" } },
      );
    }
    return NextResponse.json(
      { agent },
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
