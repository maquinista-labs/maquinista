import { NextResponse } from "next/server";

import { MODELS, RUNNERS } from "@/lib/catalog";
import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// GET /api/agents/new-catalog — one payload powering the G.5 spawn
// modal: runtimes (TS-side catalog), models per runtime (TS), and
// soul templates (DB). Bundled so the modal populates without
// chained fetches.
export async function GET() {
  try {
    const pool = getPool();
    const { rows } = await pool.query(`
      SELECT id, name, tagline
      FROM soul_templates
      ORDER BY id ASC
    `);
    return NextResponse.json(
      {
        runners: RUNNERS,
        models: MODELS,
        souls: rows.map((r) => ({
          id: r.id,
          name: r.name,
          tagline: r.tagline,
        })),
      },
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
