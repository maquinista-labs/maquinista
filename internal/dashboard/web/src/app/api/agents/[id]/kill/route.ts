import { NextResponse } from "next/server";

import { requestKill } from "@/lib/actions";
import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// POST /api/agents/:id/kill — flip stop_requested. The reconcile
// loop tears the tmux pane down on its next tick.
export async function POST(
  _req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  try {
    const ok = await requestKill(getPool(), id);
    if (!ok) {
      return NextResponse.json(
        { error: "agent not found", id },
        { status: 404 },
      );
    }
    return NextResponse.json({ ok: true });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
