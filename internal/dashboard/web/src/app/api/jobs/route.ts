import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import { listJobs } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export async function GET() {
  try {
    const jobs = await listJobs(getPool());
    return NextResponse.json(jobs, {
      headers: { "Cache-Control": "no-store" },
    });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500, headers: { "Cache-Control": "no-store" } },
    );
  }
}
