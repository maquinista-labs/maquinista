// Audit + rate-limit helpers. Wraps Route Handlers to record a
// dashboard_audit row on every write and throttle via an in-memory
// sliding-window limiter keyed on operator_id || ip.

import type { Pool } from "pg";

export type AuditEntry = {
  operatorId?: string | null;
  action: string;
  subject: unknown;
  ua?: string | null;
  ip?: string | null;
  ok: boolean;
  error?: string | null;
};

export async function writeAudit(pool: Pool, e: AuditEntry): Promise<void> {
  try {
    await pool.query(
      `INSERT INTO dashboard_audit
         (operator_id, action, subject, ua, ip, ok, error)
       VALUES ($1,$2,$3,$4,$5,$6,$7)`,
      [
        e.operatorId ?? null,
        e.action,
        JSON.stringify(e.subject ?? {}),
        e.ua ?? null,
        e.ip ?? null,
        e.ok,
        e.error ?? null,
      ],
    );
  } catch (err) {
    // Audit failure must not break the request. Log to stderr.
    console.error("[dashboard:audit] insert failed", err);
  }
}

// --- sliding-window rate limiter ------------------------------------------

// Simple in-memory ring per key. Good enough for single-process
// maquinista; a multi-replica deploy would want Postgres or Redis.
const buckets = new Map<string, number[]>();

export function rateLimit(
  key: string,
  maxPerMinute = 60,
  now = Date.now(),
): { allowed: boolean; remaining: number; resetMs: number } {
  const windowMs = 60_000;
  const bucket = buckets.get(key) ?? [];
  const cutoff = now - windowMs;
  const fresh = bucket.filter((t) => t > cutoff);
  if (fresh.length >= maxPerMinute) {
    return {
      allowed: false,
      remaining: 0,
      resetMs: Math.max(0, fresh[0] + windowMs - now),
    };
  }
  fresh.push(now);
  buckets.set(key, fresh);
  return {
    allowed: true,
    remaining: maxPerMinute - fresh.length,
    resetMs: windowMs,
  };
}

// Visible for tests only.
export function _resetRateLimiter() {
  buckets.clear();
}

export function extractClientIp(req: Request): string | null {
  return (
    req.headers.get("x-forwarded-for")?.split(",")[0].trim() ??
    req.headers.get("x-real-ip") ??
    null
  );
}
