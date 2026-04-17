// Module-level Postgres pool for the dashboard. The Go daemon and
// the dashboard share the same DATABASE_URL; they coordinate only
// through the DB per §0 of plans/reference/maquinista-v2.md.
//
// The pool is lazy: the first call to getPool() creates it and
// caches the instance on the Node global. Subsequent calls return
// the same pool. Hot-reload-safe because the global is keyed on a
// Symbol that survives React refresh cycles but not full restarts.

import { Pool, type PoolConfig } from "pg";

const POOL_KEY = Symbol.for("maquinista.dashboard.pgPool");

type GlobalWithPool = typeof globalThis & {
  [POOL_KEY]?: Pool;
};

// poolOverrides is primarily for tests: they call setPoolOverride()
// with a pool pointing at an ephemeral container, then reset to
// undefined in afterEach. Production always uses DATABASE_URL.
let poolOverride: Pool | undefined;

export function setPoolOverride(p: Pool | undefined) {
  poolOverride = p;
}

// poolConfig builds connection options from env. Default size is
// modest — the dashboard has single-digit queries per request and
// should not monopolise DB connections the main daemon needs.
function poolConfig(): PoolConfig {
  const url = process.env.DATABASE_URL;
  if (!url) {
    throw new Error(
      "dashboard: DATABASE_URL not set (pass it to `maquinista dashboard start` or set the env)",
    );
  }
  return {
    connectionString: url,
    max: Number(process.env.MAQUINISTA_DASHBOARD_DB_MAX ?? 5),
    idleTimeoutMillis: 30_000,
    connectionTimeoutMillis: 5_000,
  };
}

export function getPool(): Pool {
  if (poolOverride) return poolOverride;
  const g = globalThis as GlobalWithPool;
  if (!g[POOL_KEY]) {
    const pool = new Pool(poolConfig());
    pool.on("error", (err) => {
      // Idle-client errors should not crash the process; log and
      // let pg's internal reconnect handle it.
      console.error("[dashboard:db] idle client error", err);
    });
    g[POOL_KEY] = pool;

    // Clean shutdown hook — the supervisor's SIGTERM closes the
    // pool so in-flight queries finish rather than dropping on
    // the floor.
    const shutdown = async () => {
      try {
        await pool.end();
      } catch {
        /* already closed */
      }
    };
    process.once("SIGTERM", shutdown);
    process.once("SIGINT", shutdown);
  }
  return g[POOL_KEY]!;
}

// closePool is exposed for tests; in production the SIGTERM hook
// above is the closure path.
export async function closePool(): Promise<void> {
  const g = globalThis as GlobalWithPool;
  if (g[POOL_KEY]) {
    try {
      await g[POOL_KEY]!.end();
    } finally {
      delete g[POOL_KEY];
    }
  }
}
