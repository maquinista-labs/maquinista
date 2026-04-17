import { NextResponse } from "next/server";

import { enqueueInterrupt } from "@/lib/actions";
import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// POST /api/agents/:id/interrupt — enqueue a control='interrupt'
// row in agent_inbox. The supervisor sidecar (plan:
// plans/active/per-agent-sidecar.md) is the consumer and will
// issue a Ctrl+C into the tmux window.
export async function POST(
  req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  let operator: string | null | undefined;
  const ct = req.headers.get("content-type") ?? "";
  if (ct.includes("application/json")) {
    try {
      const body = (await req.json()) as { operator?: string | null };
      operator = body.operator;
    } catch {
      /* optional body */
    }
  }
  try {
    const rowId = await enqueueInterrupt(getPool(), {
      agentId: id,
      operator,
    });
    return NextResponse.json({ id: rowId }, { status: 201 });
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return NextResponse.json({ error: msg }, { status: 500 });
  }
}
