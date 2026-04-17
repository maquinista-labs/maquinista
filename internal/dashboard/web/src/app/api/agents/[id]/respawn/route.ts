import { NextResponse } from "next/server";

import { requestRespawn } from "@/lib/actions";
import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// POST /api/agents/:id/respawn — clear tmux_window + stop_requested.
// The reconcile loop creates a fresh pane on the next tick.
export async function POST(
  _req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  try {
    const ok = await requestRespawn(getPool(), id);
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
