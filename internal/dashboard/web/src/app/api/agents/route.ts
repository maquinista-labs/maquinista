import { NextResponse } from "next/server";

import { spawnAgentFromDashboard } from "@/lib/actions";
import { writeAudit } from "@/lib/audit";
import { defaultModelFor, isKnownModel, isKnownRunner } from "@/lib/catalog";
import { getPool } from "@/lib/db";
import { listAgents } from "@/lib/queries";

// GET /api/agents — list every non-archived agent.
// POST /api/agents — spawn a new agent (G.5 of dashboard-gaps).
export const dynamic = "force-dynamic";
export const revalidate = 0;

export async function GET() {
  try {
    const pool = getPool();
    const agents = await listAgents(pool);
    return NextResponse.json(
      { agents },
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

type SpawnBody = {
  handle?: unknown;
  runner?: unknown;
  model?: unknown;
  soul_template?: unknown;
};

export async function POST(req: Request) {
  let body: SpawnBody;
  try {
    body = (await req.json()) as SpawnBody;
  } catch {
    return NextResponse.json({ error: "invalid_json" }, { status: 400 });
  }
  if (
    typeof body.handle !== "string" ||
    typeof body.runner !== "string" ||
    typeof body.soul_template !== "string"
  ) {
    return NextResponse.json(
      { error: "missing_fields" },
      { status: 400 },
    );
  }
  if (!isKnownRunner(body.runner)) {
    return NextResponse.json({ error: "invalid_runner" }, { status: 400 });
  }

  let model: string | null = null;
  if (typeof body.model === "string" && body.model.length > 0) {
    if (!isKnownModel(body.runner, body.model)) {
      return NextResponse.json({ error: "invalid_model" }, { status: 400 });
    }
    model = body.model;
  } else {
    model = defaultModelFor(body.runner);
  }

  const pool = getPool();
  // cwd defaults to the daemon's process cwd — spawning via the UI
  // inherits wherever the orchestrator was launched. Operators can
  // later edit via `maquinista agent edit --cwd`.
  const cwd = process.cwd();

  try {
    const result = await spawnAgentFromDashboard(pool, {
      handle: body.handle,
      runner: body.runner,
      model,
      soulTemplateID: body.soul_template,
      cwd,
    });

    const ok = result.kind === "created";
    await writeAudit(pool, {
      action: "agent.spawned",
      subject: {
        handle: body.handle,
        runner: body.runner,
        model,
        soul_template: body.soul_template,
      },
      ok,
      error: ok ? null : result.kind,
    });

    switch (result.kind) {
      case "created":
        return NextResponse.json(
          { id: result.id, handle: body.handle, runner: body.runner, model },
          { status: 201 },
        );
      case "invalid_handle":
        return NextResponse.json(
          { error: "invalid_handle", handle: body.handle },
          { status: 400 },
        );
      case "invalid_soul_template":
        return NextResponse.json(
          { error: "invalid_soul_template", soul_template: body.soul_template },
          { status: 400 },
        );
      case "handle_taken":
        return NextResponse.json(
          { error: "handle_taken", handle: result.handle },
          { status: 409 },
        );
      default:
        return NextResponse.json({ error: "unknown_result" }, { status: 500 });
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return NextResponse.json({ error: msg }, { status: 500 });
  }
}
