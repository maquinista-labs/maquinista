import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

// DELETE /api/agents/:id/delete — fully delete agent and all related data
// (agent_souls, agent_settings, agent_inbox, agent_outbox, etc.)
// via ON DELETE CASCADE. This is irreversible.
export async function DELETE(
  _req: Request,
  ctx: { params: Promise<{ id: string }> },
) {
  const { id } = await ctx.params;
  try {
    const pool = getPool();

    // First, kill the tmux window if it exists to stop the runner process.
    const { tmux_session, tmux_window } = (
      await pool.query(
        `SELECT tmux_session, COALESCE(tmux_window,'') AS tmux_window
         FROM agents WHERE id = $1`,
        [id],
      )
    ).rows[0];

    if (tmux_window && tmux_session) {
      // Use tmux to kill the window. Since we're in Node.js, we can't
      // import the Go tmux package. We'll shell out to tmux directly.
      const { exec } = await import("child_process");
      try {
        exec(
          `tmux kill-window -t ${tmux_session}:${tmux_window}`,
          (error: any) => {
            // Ignore "window not found" errors — already dead.
            if (error && !error.message.includes("not found")) {
              console.error(`tmux kill-window failed: ${error.message}`);
            }
          },
        );
      } catch (e) {
        console.error(`Failed to kill tmux window: ${e}`);
      }
    }

    // Delete the agent row — ON DELETE CASCADE cleans up related tables.
    const result = await pool.query(
      `DELETE FROM agents WHERE id = $1 AND role = 'user' AND task_id IS NULL RETURNING id`,
      [id],
    );

    if (result.rows.length === 0) {
      return NextResponse.json(
        { error: "agent not found or not a persistent user agent", id },
        { status: 404 },
      );
    }

    return NextResponse.json({ ok: true, id: result.rows[0].id });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
