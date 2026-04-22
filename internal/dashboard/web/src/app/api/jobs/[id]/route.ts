import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";

// PATCH /api/jobs/[id] — update job fields
export async function PATCH(
  req: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  try {
    const { id } = await params;
    const b = (await req.json()) as {
      name?: string;
      cron_expr?: string;
      timezone?: string;
      soul_template_id?: string | null;
      agent_id?: string | null;
      prompt?: string;
      context_markdown?: string;
      agent_cwd?: string;
      enabled?: boolean;
    };

    const pool = getPool();
    const updates: string[] = [];
    const values: unknown[] = [];
    let i = 1;

    const addField = (col: string, val: unknown) => {
      updates.push(`${col} = $${i++}`);
      values.push(val);
    };

    if (b.name !== undefined) addField("name", b.name);
    if (b.cron_expr !== undefined) addField("cron_expr", b.cron_expr);
    if (b.timezone !== undefined) addField("timezone", b.timezone);
    if (b.soul_template_id !== undefined)
      addField("soul_template_id", b.soul_template_id);
    if (b.agent_id !== undefined) addField("agent_id", b.agent_id);
    if (b.prompt !== undefined)
      addField("prompt", JSON.stringify({ type: "text", text: b.prompt }));
    if (b.context_markdown !== undefined)
      addField("context_markdown", b.context_markdown);
    if (b.agent_cwd !== undefined) addField("agent_cwd", b.agent_cwd);
    if (b.enabled !== undefined) addField("enabled", b.enabled);

    if (updates.length === 0) {
      return NextResponse.json({ error: "no fields to update" }, { status: 400 });
    }

    values.push(id);
    const { rowCount } = await pool.query(
      `UPDATE scheduled_jobs SET ${updates.join(", ")} WHERE id = $${i}`,
      values,
    );
    if (rowCount === 0) {
      return NextResponse.json({ error: "not found" }, { status: 404 });
    }
    return NextResponse.json({ ok: true });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}

// DELETE /api/jobs/[id] — delete a job
export async function DELETE(
  _req: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  try {
    const { id } = await params;
    const pool = getPool();
    const { rowCount } = await pool.query(
      `DELETE FROM scheduled_jobs WHERE id = $1`,
      [id],
    );
    if (rowCount === 0) {
      return NextResponse.json({ error: "not found" }, { status: 404 });
    }
    return NextResponse.json({ ok: true });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
