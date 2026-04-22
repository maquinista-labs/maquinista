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

export async function POST(req: Request) {
  try {
    const b = (await req.json()) as {
      name: string;
      cron_expr: string;
      timezone?: string;
      soul_template_id?: string;
      agent_id?: string;
      prompt: string;
      context_markdown?: string;
      agent_cwd?: string;
    };

    if (!b.name || typeof b.name !== "string") {
      return NextResponse.json({ error: "name required" }, { status: 400 });
    }
    if (!b.cron_expr || typeof b.cron_expr !== "string") {
      return NextResponse.json(
        { error: "cron_expr required" },
        { status: 400 },
      );
    }
    if (!b.soul_template_id && !b.agent_id) {
      return NextResponse.json(
        { error: "soul_template_id or agent_id required" },
        { status: 400 },
      );
    }
    if (!b.prompt || typeof b.prompt !== "string") {
      return NextResponse.json({ error: "prompt required" }, { status: 400 });
    }

    const pool = getPool();
    const { rows } = await pool.query(
      `
      INSERT INTO scheduled_jobs
        (name, cron_expr, timezone, soul_template_id, agent_id,
         prompt, context_markdown, agent_cwd, enabled, next_run_at)
      VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, true, NOW())
      RETURNING id
      `,
      [
        b.name,
        b.cron_expr,
        b.timezone ?? "UTC",
        b.soul_template_id ?? null,
        b.agent_id ?? null,
        JSON.stringify({ type: "text", text: b.prompt }),
        b.context_markdown ?? "",
        b.agent_cwd ?? "",
      ],
    );
    return NextResponse.json({ id: rows[0].id as string }, { status: 201 });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
