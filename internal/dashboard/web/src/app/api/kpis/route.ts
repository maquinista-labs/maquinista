import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { computeKPIs } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export async function GET() {
  try {
    const kpis = await computeKPIs(getPool());
    return NextResponse.json(kpis, {
      headers: { "Cache-Control": "no-store" },
    });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500, headers: { "Cache-Control": "no-store" } },
    );
  }
}
