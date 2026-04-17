import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import type { SystemHealth } from "@/lib/types";

export const dynamic = "force-dynamic";
export const revalidate = 0;

const startedAt = Date.now();

export async function GET() {
  try {
    const pool = getPool();
    const stat: SystemHealth = {
      pg: {
        total: pool.totalCount,
        idle: pool.idleCount,
        waiting: pool.waitingCount,
      },
      uptime_ms: Date.now() - startedAt,
      pid: process.pid,
      node_version: process.version,
      platform: `${process.platform}/${process.arch}`,
    };
    return NextResponse.json(stat, {
      headers: { "Cache-Control": "no-store" },
    });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500, headers: { "Cache-Control": "no-store" } },
    );
  }
}
