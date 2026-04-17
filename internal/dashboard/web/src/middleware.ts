import { NextResponse, type NextRequest } from "next/server";

// Dashboard auth middleware. MAQUINISTA_DASHBOARD_AUTH controls the
// posture:
//   "none"     — pass through (default; loopback-only deployments).
//   "password" — session cookie required for /api/* and /(dash)/*.
//                Redirects UI routes to /auth; rejects API with 401.
//   "telegram" — same gate shape; /auth redirects to /auth/telegram
//                (stub until bot wiring lands).
//
// Note: we check only the cookie presence here for speed; the
// expiry + revocation check lives in each Route Handler (DB round
// trip is cheap and avoids edge-runtime DB access).

const SESSION_COOKIE = "maquinista_dash_session";

export const config = {
  matcher: [
    "/((?!_next/|favicon|robots|sitemap|manifest|auth|api/auth).*)",
  ],
};

export function middleware(req: NextRequest) {
  const mode = (process.env.MAQUINISTA_DASHBOARD_AUTH ?? "none").toLowerCase();
  if (mode !== "password" && mode !== "telegram") {
    return NextResponse.next();
  }

  // /api/healthz always passes — used by the Go supervisor's
  // readiness probe and stays cheap.
  const { pathname } = req.nextUrl;
  if (pathname === "/api/healthz") return NextResponse.next();

  const token = req.cookies.get(SESSION_COOKIE)?.value;
  if (token) return NextResponse.next();

  // No cookie. Redirect UI routes to /auth; reject API routes with
  // 401 (so fetch()-based clients can read the status).
  if (pathname.startsWith("/api/")) {
    return NextResponse.json(
      { error: "unauthenticated" },
      { status: 401 },
    );
  }
  const url = req.nextUrl.clone();
  url.pathname = "/auth";
  url.searchParams.set("next", pathname);
  return NextResponse.redirect(url);
}
