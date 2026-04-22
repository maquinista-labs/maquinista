import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";
import type { JobExecution } from "@/lib/types";

export const dynamic = "force-dynamic";

// GET /api/jobs/[id]/executions — last 25 executions for a job
export async function GET(
  _req: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  try {
    const { id } = await params;
    const pool = getPool();
    const { rows } = await pool.query(
      `SELECT je.id, je.agent_id, je.started_at, je.ended_at,
              a.status AS agent_status, a.tmux_window
       FROM job_executions je
       LEFT JOIN agents a ON a.id = je.agent_id
       WHERE je.job_id = $1
       ORDER BY je.started_at DESC
       LIMIT 25`,
      [id],
    );

    const executions: JobExecution[] = rows.map((r) => ({
      id: r.id as string,
      agent_id: r.agent_id as string | null,
      started_at: r.started_at.toISOString
        ? r.started_at.toISOString()
        : String(r.started_at),
      ended_at: r.ended_at
        ? r.ended_at.toISOString
          ? r.ended_at.toISOString()
          : String(r.ended_at)
        : null,
      agent_status: r.agent_status as string | null,
      tmux_window: r.tmux_window as string | null,
    }));

    return NextResponse.json(executions);
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
