import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { isValidHandle } from "@/lib/utils";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// GET /api/agents/check-handle?h=<value> — returns the UX hint for
// the spawn modal's live availability check.
//
// Response shape:
//   {available: true}                       — handle is free
//   {available: false, reason: "taken"}     — another agent owns it
//   {available: false, reason: "invalid"}   — regex/prefix failure
export async function GET(req: Request) {
  const url = new URL(req.url);
  const raw = url.searchParams.get("h");
  if (raw === null) {
    return NextResponse.json(
      { error: "missing_h" },
      { status: 400, headers: { "Cache-Control": "no-store" } },
    );
  }
  const handle = raw.trim();
  if (handle.length === 0 || !isValidHandle(handle)) {
    return NextResponse.json(
      { available: false, reason: "invalid" },
      { headers: { "Cache-Control": "no-store" } },
    );
  }
  try {
    const { rows } = await getPool().query(
      `SELECT 1 FROM agents WHERE lower(handle) = lower($1) LIMIT 1`,
      [handle],
    );
    if (rows.length > 0) {
      return NextResponse.json(
        { available: false, reason: "taken" },
        { headers: { "Cache-Control": "no-store" } },
      );
    }
    return NextResponse.json(
      { available: true },
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
