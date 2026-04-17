// Auth primitives for the dashboard. Phase 6 of
// plans/active/dashboard.md.
//
// Three modes controlled by env MAQUINISTA_DASHBOARD_AUTH:
//   "none"     — bind to loopback, no auth (default)
//   "password" — username + PBKDF2-SHA256; cookie session
//   "telegram" — magic-link via the bot (stub; returns 501 until
//                 the bot→dashboard wiring lands).
//
// Deliberately small: pbkdf2 from node:crypto, cookie-signed token,
// tables defined in migration 026.

import { pbkdf2Sync, randomBytes, timingSafeEqual, createHash } from "node:crypto";
import type { Pool } from "pg";

const DEFAULT_ITER = 600_000;

export type AuthMode = "none" | "password" | "telegram";

export function authMode(): AuthMode {
  const raw = (process.env.MAQUINISTA_DASHBOARD_AUTH ?? "none").toLowerCase();
  if (raw === "password" || raw === "telegram") return raw;
  return "none";
}

// --- password hashing ------------------------------------------------------

export function hashPassword(password: string, salt?: string, iter = DEFAULT_ITER) {
  const s = salt ?? randomBytes(16).toString("hex");
  const derived = pbkdf2Sync(password, s, iter, 32, "sha256").toString("hex");
  return { hash: derived, salt: s, iter };
}

export function verifyPassword(
  password: string,
  hash: string,
  salt: string,
  iter = DEFAULT_ITER,
): boolean {
  const derived = pbkdf2Sync(password, salt, iter, 32, "sha256");
  const expected = Buffer.from(hash, "hex");
  if (derived.length !== expected.length) return false;
  return timingSafeEqual(derived, expected);
}

// --- session tokens --------------------------------------------------------

// Token: a random opaque string; we store sha256(token) in the DB
// so a DB leak doesn't expose session tokens.
export function newSessionToken(): { token: string; hash: string } {
  const token = randomBytes(32).toString("hex");
  const hash = createHash("sha256").update(token).digest("hex");
  return { token, hash };
}

export function hashToken(token: string): string {
  return createHash("sha256").update(token).digest("hex");
}

export const SESSION_COOKIE = "maquinista_dash_session";
export const SESSION_TTL_MS = 12 * 60 * 60 * 1000; // 12 h

// --- DB helpers ------------------------------------------------------------

export type Operator = {
  id: string;
  username: string;
};

export async function findOperator(
  pool: Pool,
  username: string,
): Promise<null | {
  id: string;
  username: string;
  pbkdf2_hash: string;
  salt: string;
  iter: number;
  failed_attempts: number;
  locked_until: Date | null;
}> {
  const { rows } = await pool.query(
    `SELECT id, username, pbkdf2_hash, salt, iter, failed_attempts, locked_until
     FROM operator_credentials WHERE username = $1`,
    [username],
  );
  return rows[0] ?? null;
}

export async function createOperator(
  pool: Pool,
  username: string,
  password: string,
): Promise<Operator> {
  const { hash, salt, iter } = hashPassword(password);
  const { rows } = await pool.query(
    `INSERT INTO operator_credentials (username, pbkdf2_hash, salt, iter)
     VALUES ($1, $2, $3, $4)
     RETURNING id, username`,
    [username, hash, salt, iter],
  );
  return rows[0] as Operator;
}

export async function createSession(
  pool: Pool,
  operatorId: string,
  meta: { ua?: string; ip?: string } = {},
): Promise<string> {
  const { token, hash } = newSessionToken();
  const expiresAt = new Date(Date.now() + SESSION_TTL_MS);
  await pool.query(
    `INSERT INTO dashboard_sessions (operator_id, token_hash, ua, ip, expires_at)
     VALUES ($1, $2, $3, $4, $5)`,
    [operatorId, hash, meta.ua ?? null, meta.ip ?? null, expiresAt],
  );
  return token;
}

export async function sessionOperator(
  pool: Pool,
  token: string | null | undefined,
): Promise<Operator | null> {
  if (!token) return null;
  const hash = hashToken(token);
  const { rows } = await pool.query(
    `SELECT s.operator_id, o.username
     FROM dashboard_sessions s
     JOIN operator_credentials o ON o.id = s.operator_id
     WHERE s.token_hash = $1
       AND s.revoked_at IS NULL
       AND s.expires_at > NOW()
     LIMIT 1`,
    [hash],
  );
  if (rows.length === 0) return null;
  return { id: rows[0].operator_id, username: rows[0].username };
}

export async function revokeSession(
  pool: Pool,
  token: string | null | undefined,
): Promise<void> {
  if (!token) return;
  await pool.query(
    `UPDATE dashboard_sessions
     SET revoked_at = NOW()
     WHERE token_hash = $1 AND revoked_at IS NULL`,
    [hashToken(token)],
  );
}

// --- failed-login handling (lockout after 5 failures) ----------------------

export async function recordFailedLogin(
  pool: Pool,
  username: string,
): Promise<void> {
  await pool.query(
    `UPDATE operator_credentials
     SET failed_attempts = failed_attempts + 1,
         locked_until = CASE
           WHEN failed_attempts + 1 >= 5 THEN NOW() + INTERVAL '15 minutes'
           ELSE locked_until
         END
     WHERE username = $1`,
    [username],
  );
}

export async function resetFailedLogin(
  pool: Pool,
  operatorId: string,
): Promise<void> {
  await pool.query(
    `UPDATE operator_credentials
     SET failed_attempts = 0,
         locked_until = NULL,
         last_login_at = NOW()
     WHERE id = $1`,
    [operatorId],
  );
}

export function isLocked(locked_until: Date | null): boolean {
  return locked_until !== null && locked_until.getTime() > Date.now();
}
