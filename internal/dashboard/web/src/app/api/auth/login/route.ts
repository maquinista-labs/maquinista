import { NextResponse } from "next/server";

import { writeAudit, extractClientIp, rateLimit } from "@/lib/audit";
import {
  SESSION_COOKIE,
  SESSION_TTL_MS,
  createSession,
  findOperator,
  isLocked,
  recordFailedLogin,
  resetFailedLogin,
  verifyPassword,
} from "@/lib/auth";
import { getPool } from "@/lib/db";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export async function POST(req: Request) {
  const ip = extractClientIp(req);
  // 60/min is generous for human login; protects against automated
  // brute-force without tripping on CI or Playwright runs that
  // exercise the endpoint dozens of times in a minute.
  const limit = rateLimit(`login:${ip ?? "anon"}`, 60);
  if (!limit.allowed) {
    return NextResponse.json(
      { error: "too many attempts; try again in a minute" },
      { status: 429 },
    );
  }

  let body: { username?: string; password?: string } = {};
  try {
    body = (await req.json()) as typeof body;
  } catch {
    return NextResponse.json({ error: "bad body" }, { status: 400 });
  }
  const username = (body.username ?? "").trim();
  const password = body.password ?? "";
  if (!username || !password) {
    return NextResponse.json(
      { error: "username + password required" },
      { status: 400 },
    );
  }

  const pool = getPool();
  const op = await findOperator(pool, username);

  if (!op) {
    await writeAudit(pool, {
      action: "auth.login",
      subject: { username },
      ip,
      ua: req.headers.get("user-agent"),
      ok: false,
      error: "no such user",
    });
    return NextResponse.json({ error: "invalid credentials" }, { status: 401 });
  }

  if (isLocked(op.locked_until)) {
    await writeAudit(pool, {
      operatorId: op.id,
      action: "auth.login",
      subject: { username },
      ip,
      ua: req.headers.get("user-agent"),
      ok: false,
      error: "locked",
    });
    return NextResponse.json(
      { error: "account locked; try again later" },
      { status: 423 },
    );
  }

  const valid = verifyPassword(password, op.pbkdf2_hash, op.salt, op.iter);
  if (!valid) {
    await recordFailedLogin(pool, username);
    await writeAudit(pool, {
      operatorId: op.id,
      action: "auth.login",
      subject: { username },
      ip,
      ua: req.headers.get("user-agent"),
      ok: false,
      error: "bad password",
    });
    return NextResponse.json({ error: "invalid credentials" }, { status: 401 });
  }

  await resetFailedLogin(pool, op.id);
  const token = await createSession(pool, op.id, {
    ua: req.headers.get("user-agent") ?? undefined,
    ip: ip ?? undefined,
  });

  await writeAudit(pool, {
    operatorId: op.id,
    action: "auth.login",
    subject: { username },
    ip,
    ua: req.headers.get("user-agent"),
    ok: true,
  });

  const res = NextResponse.json({ ok: true });
  res.cookies.set(SESSION_COOKIE, token, {
    httpOnly: true,
    sameSite: "lax",
    secure: process.env.NODE_ENV === "production",
    path: "/",
    maxAge: Math.floor(SESSION_TTL_MS / 1000),
  });
  return res;
}
