import { NextResponse } from "next/server";

import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";

// GET /api/soul-templates — list all soul templates (for job form selects)
export async function GET() {
  try {
    const pool = getPool();
    const { rows } = await pool.query(
      `SELECT id, name, role, tagline FROM soul_templates ORDER BY name`,
    );
    return NextResponse.json(rows);
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}

// POST /api/soul-templates — create a new soul template
export async function POST(request: Request) {
  try {
    const pool = getPool();
    const body = await request.json();

    const {
      id,
      name,
      tagline,
      role,
      goal,
      core_truths = "",
      boundaries = "",
      vibe = "",
      continuity = "",
      allow_delegation = false,
      max_iter = 25,
    } = body;

    // Validation
    if (!id || typeof id !== "string") {
      return NextResponse.json(
        { error: "missing_or_invalid_id" },
        { status: 400 },
      );
    }
    if (!name || typeof name !== "string") {
      return NextResponse.json(
        { error: "missing_or_invalid_name" },
        { status: 400 },
      );
    }
    if (!role || typeof role !== "string") {
      return NextResponse.json(
        { error: "missing_or_invalid_role" },
        { status: 400 },
      );
    }
    if (!goal || typeof goal !== "string") {
      return NextResponse.json(
        { error: "missing_or_invalid_goal" },
        { status: 400 },
      );
    }

    // Check for duplicate
    const existing = await pool.query(
      `SELECT 1 FROM soul_templates WHERE id = $1`,
      [id],
    );
    if (existing.rows.length > 0) {
      return NextResponse.json(
        { error: "template_already_exists", existing_id: id },
        { status: 409 },
      );
    }

    // Insert
    await pool.query(
      `INSERT INTO soul_templates
        (id, name, tagline, role, goal, core_truths, boundaries, vibe, continuity, allow_delegation, max_iter)
      VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
      [id, name, tagline || null, role, goal, core_truths, boundaries, vibe, continuity, allow_delegation, max_iter],
    );

    return NextResponse.json(
      { id, name, tagline, role, goal },
      { status: 201 },
    );
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return NextResponse.json({ error: msg }, { status: 500 });
  }
}