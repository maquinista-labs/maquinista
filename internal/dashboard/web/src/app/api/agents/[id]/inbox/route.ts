import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { listInbox } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// GET /api/agents/:id/inbox?limit=&before=
// `before` is an ISO timestamp cursor — the next page follows the
// enqueued_at < cursor predicate.
export async function GET(
  req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  const url = new URL(req.url);
  const limit = Number(url.searchParams.get("limit") ?? "50");
  const before = url.searchParams.get("before") ?? undefined;
  try {
    const rows = await listInbox(getPool(), id, { limit, before });
    const nextCursor =
      rows.length === limit ? rows[rows.length - 1].enqueued_at : null;
    return NextResponse.json(
      { rows, nextCursor },
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
