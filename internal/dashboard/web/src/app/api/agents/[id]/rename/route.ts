import { NextResponse } from "next/server";

import { renameAgent } from "@/lib/actions";
import { writeAudit } from "@/lib/audit";
import { getPool } from "@/lib/db";
import { isValidHandle } from "@/lib/utils";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// POST /api/agents/:id/rename — set (or clear) `agents.handle`.
//   body: { handle: string | null }
//   409  if the handle collides with another agent (lower-case).
//   400  if the handle fails the regex / reserved-prefix check.
//   404  if the id does not exist.
export async function POST(
  req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  let body: { handle?: string | null };
  try {
    body = (await req.json()) as { handle?: string | null };
  } catch {
    return NextResponse.json({ error: "invalid_json" }, { status: 400 });
  }

  const raw = body.handle;
  let handle: string | null = null;
  if (raw !== null && raw !== undefined) {
    if (typeof raw !== "string") {
      return NextResponse.json({ error: "invalid_type" }, { status: 400 });
    }
    const trimmed = raw.trim();
    if (trimmed.length > 0) {
      if (!isValidHandle(trimmed)) {
        return NextResponse.json(
          { error: "invalid_handle", handle: trimmed },
          { status: 400 },
        );
      }
      handle = trimmed;
    }
  }

  const pool = getPool();
  const result = await renameAgent(pool, id, handle);

  // Audit the attempt regardless of outcome.
  await writeAudit(pool, {
    action: "agent.renamed",
    subject: { id, handle },
    ok: result === "updated",
    error: result === "updated" ? null : result,
  });

  if (result === "not_found") {
    return NextResponse.json({ error: "not_found" }, { status: 404 });
  }
  if (result === "conflict") {
    return NextResponse.json(
      { error: "handle_taken", handle },
      { status: 409 },
    );
  }
  return NextResponse.json({ id, handle }, { status: 200 });
}
