import { NextResponse } from "next/server";

import { enqueueInboxFromDashboard } from "@/lib/actions";
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

// POST /api/agents/:id/inbox — composer path. Accepts either JSON
// { text: "..." } or form-encoded "text=...". Writes to agent_inbox
// with origin_channel='dashboard'. Returns the new row id.
export async function POST(
  req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  let text: string | undefined;
  let operator: string | null | undefined;

  const ct = req.headers.get("content-type") ?? "";
  try {
    if (ct.includes("application/json")) {
      const body = (await req.json()) as {
        text?: string;
        operator?: string | null;
      };
      text = body.text;
      operator = body.operator;
    } else {
      const fd = await req.formData();
      const t = fd.get("text");
      if (typeof t === "string") text = t;
      const op = fd.get("operator");
      if (typeof op === "string") operator = op;
    }
  } catch (err) {
    return NextResponse.json(
      { error: `bad body: ${err instanceof Error ? err.message : String(err)}` },
      { status: 400 },
    );
  }

  if (!text || !text.trim()) {
    return NextResponse.json(
      { error: "text is required" },
      { status: 400 },
    );
  }

  try {
    const rowId = await enqueueInboxFromDashboard(getPool(), {
      agentId: id,
      text: text.trim(),
      operator,
    });
    return NextResponse.json({ id: rowId }, { status: 201 });
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return NextResponse.json({ error: msg }, { status: 500 });
  }
}
