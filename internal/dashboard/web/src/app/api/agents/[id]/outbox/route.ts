import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { listOutbox } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export async function GET(
  req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  const url = new URL(req.url);
  const limit = Number(url.searchParams.get("limit") ?? "50");
  const before = url.searchParams.get("before") ?? undefined;
  try {
    const rows = await listOutbox(getPool(), id, { limit, before });
    const nextCursor =
      rows.length === limit ? rows[rows.length - 1].created_at : null;
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
