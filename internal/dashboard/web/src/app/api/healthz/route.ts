import { NextResponse } from "next/server";

// /api/healthz — readiness probe for the Go supervisor and CI.
//
// Returns a small JSON payload. `stub: false` distinguishes the
// real Next.js-served healthz from the Phase 0 Node one-liner stub
// (which reports `stub: true`), so an integration test can assert
// "we are on the real server now" when Phase 1 Commit 1.6 wires the
// supervisor to spawn `node .next/standalone/server.js`.

// Cache policy: no-store so the Go supervisor's ready probe always
// hits a fresh handler rather than a cached one. The handler is
// trivial; force-dynamic keeps Next from attempting to statically
// optimize it.
export const dynamic = "force-dynamic";
export const revalidate = 0;

const startedAt = Date.now();

export function GET() {
  return NextResponse.json(
    {
      ok: true,
      stub: false,
      version: process.env.MAQUINISTA_VERSION ?? "dev",
      uptime_ms: Date.now() - startedAt,
      pid: process.pid,
    },
    { headers: { "Cache-Control": "no-store" } },
  );
}
