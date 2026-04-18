import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { listGlobalInbox } from "@/lib/queries";
import type { InboxRow } from "@/lib/types";

// GET /api/inbox — cross-agent inbox feed. Default shows recent
// activity across every status so operators see what came in, not
// only what's still queued (empty "nothing pending" was
// confusing when the agents had already consumed everything). The
// `pending` status is always ordered first via listGlobalInbox so
// action-needed rows still stand out.
//
// Callers can narrow via `?status=pending,processing`.
export const dynamic = "force-dynamic";
export const revalidate = 0;

const VALID: Set<InboxRow["status"]> = new Set([
  "pending",
  "processing",
  "processed",
  "failed",
  "dead",
]);

export async function GET(req: Request) {
  const url = new URL(req.url);
  const limitParam = url.searchParams.get("limit");
  const limit = limitParam ? Math.max(1, Number(limitParam)) : 100;
  const statusParam = url.searchParams.get("status");
  const status: InboxRow["status"][] = statusParam
    ? statusParam
        .split(",")
        .map((s) => s.trim())
        .filter((s): s is InboxRow["status"] =>
          VALID.has(s as InboxRow["status"]),
        )
    : (Array.from(VALID) as InboxRow["status"][]);
  try {
    const items = await listGlobalInbox(getPool(), { limit, status });
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
