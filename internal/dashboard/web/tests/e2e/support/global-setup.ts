/**
 * Playwright global setup: build the maquinista binary (if missing
 * or stale), build the Next.js standalone bundle (if missing or
 * stale), and launch `maquinista dashboard start --no-embed ...`
 * on an ephemeral port. The PID is written to a file the
 * global-teardown hook reads on shutdown.
 *
 * Phase 1 Commit 1.7 ships this in DB-less form: the dashboard has
 * no Postgres dependencies yet, so Playwright just drives the
 * static shell. Phase 2 adds a dbtest-like Postgres fixture and
 * DATABASE_URL injection alongside it.
 */

import { spawn } from "node:child_process";
import { createServer } from "node:net";
import { access, cp, mkdir, readFile, readdir, stat, writeFile } from "node:fs/promises";
import { constants } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import process from "node:process";
import { setTimeout as sleep } from "node:timers/promises";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { randomBytes } from "node:crypto";

const execFileP = promisify(execFile);

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..", "..", "..");
const WEB_DIR = path.join(REPO_ROOT, "internal", "dashboard", "web");
const STANDALONE = path.join(WEB_DIR, ".next", "standalone");
const BIN_DIR = path.join(tmpdir(), "maquinista-e2e-bin");
const BIN = path.join(BIN_DIR, "maquinista");
const STATE_FILE = path.join(tmpdir(), "maquinista-e2e-state.json");

async function exists(p: string): Promise<boolean> {
  try {
    await access(p, constants.F_OK);
    return true;
  } catch {
    return false;
  }
}

// anySourceNewerThan walks src/ + next.config.ts + package.json and
// returns true if any file's mtime exceeds the reference file's.
// Used by the Playwright global-setup to decide when to rebuild
// the standalone bundle.
async function anySourceNewerThan(refPath: string): Promise<boolean> {
  const { readdir } = await import("node:fs/promises");
  const refMtime = (await stat(refPath)).mtimeMs;
  const queue: string[] = [
    path.join(WEB_DIR, "src"),
    path.join(WEB_DIR, "next.config.ts"),
    path.join(WEB_DIR, "package.json"),
  ];
  for (const p of queue) {
    if (!(await exists(p))) continue;
    const s = await stat(p);
    if (s.isDirectory()) {
      const entries = await readdir(p);
      for (const e of entries) queue.push(path.join(p, e));
      continue;
    }
    if (s.mtimeMs > refMtime) return true;
  }
  return false;
}

async function ensureMaquinistaBinary(): Promise<string> {
  if (await exists(BIN)) return BIN;
  await mkdir(BIN_DIR, { recursive: true });
  await execFileP(
    "go",
    ["build", "-o", BIN, "./cmd/maquinista"],
    { cwd: REPO_ROOT },
  );
  return BIN;
}

async function ensureNextStandalone(): Promise<string> {
  const serverJS = path.join(STANDALONE, "server.js");
  const stale = !(await exists(serverJS))
    ? true
    : await anySourceNewerThan(serverJS);
  if (!stale) return STANDALONE;

  console.log("[playwright setup] building Next.js standalone bundle…");
  await execFileP("npm", ["run", "build"], { cwd: WEB_DIR });

  // Per Next docs, standalone mode does not copy public/ or
  // .next/static/ — do it here.
  if (await exists(path.join(WEB_DIR, "public"))) {
    await cp(
      path.join(WEB_DIR, "public"),
      path.join(STANDALONE, "public"),
      { recursive: true, force: true },
    );
  }
  await cp(
    path.join(WEB_DIR, ".next", "static"),
    path.join(STANDALONE, ".next", "static"),
    { recursive: true, force: true },
  );
  return STANDALONE;
}

async function pickFreePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.unref();
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        reject(new Error("unexpected server address"));
        return;
      }
      const port = addr.port;
      server.close(() => resolve(port));
    });
  });
}

async function waitForHealthz(url: string, timeoutMs: number) {
  const deadline = Date.now() + timeoutMs;
  let lastErr: unknown;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
      lastErr = new Error(`status ${res.status}`);
    } catch (err) {
      lastErr = err;
    }
    await sleep(100);
  }
  throw new Error(
    `healthz never became ready at ${url} after ${timeoutMs} ms: ${String(
      lastErr,
    )}`,
  );
}

async function hasDocker(): Promise<boolean> {
  try {
    await execFileP("docker", ["info"], { timeout: 5000 });
    return true;
  } catch {
    return false;
  }
}

// startPostgres spins up a disposable postgres:16-alpine container
// on an ephemeral port, applies maquinista migrations against it,
// and returns the connection URL + container name. The container
// is torn down in global-teardown via the state file.
async function startPostgres(): Promise<{ url: string; container: string }> {
  if (!(await hasDocker())) {
    throw new Error(
      "docker not available; Playwright e2e needs Docker for the Postgres fixture",
    );
  }
  const container = `maquinista-e2e-${randomBytes(4).toString("hex")}`;
  const pgPort = await pickFreePort();

  await execFileP("docker", [
    "run",
    "-d",
    "--rm",
    "--name",
    container,
    "-e",
    "POSTGRES_DB=maquinistadb",
    "-e",
    "POSTGRES_USER=maquinista",
    "-e",
    "POSTGRES_PASSWORD=maquinista",
    "-p",
    `${pgPort}:5432`,
    "postgres:16-alpine",
  ]);

  const url = `postgres://maquinista:maquinista@127.0.0.1:${pgPort}/maquinistadb?sslmode=disable`;

  // Wait for Postgres to accept TCP connections. pg_isready over
  // the unix socket returns success before the TCP listener is up,
  // so we probe -h localhost explicitly.
  const deadline = Date.now() + 60_000;
  let ready = false;
  while (Date.now() < deadline) {
    try {
      await execFileP("docker", [
        "exec",
        container,
        "pg_isready",
        "-h",
        "localhost",
        "-U",
        "maquinista",
        "-d",
        "maquinistadb",
      ]);
      ready = true;
      break;
    } catch {
      await sleep(250);
    }
  }
  if (!ready) {
    throw new Error("Postgres did not become ready within 60 s");
  }

  // Apply every .sql migration in order using the container's psql.
  const migrationsDir = path.join(
    REPO_ROOT,
    "internal",
    "db",
    "migrations",
  );
  const files = (await readdir(migrationsDir)).filter((f) =>
    f.endsWith(".sql"),
  );
  files.sort();
  for (const f of files) {
    const p = path.join(migrationsDir, f);
    const content = await readFile(p, "utf8");
    // Pipe via stdin to the container's psql.
    await new Promise<void>((resolve, reject) => {
      const psql = spawn(
        "docker",
        [
          "exec",
          "-i",
          container,
          "psql",
          "-h",
          "localhost",
          "-U",
          "maquinista",
          "-d",
          "maquinistadb",
          "-v",
          "ON_ERROR_STOP=1",
          "-q",
        ],
        { stdio: ["pipe", "ignore", "inherit"] },
      );
      psql.on("exit", (code) =>
        code === 0
          ? resolve()
          : reject(new Error(`migration ${f} failed (exit ${code})`)),
      );
      psql.stdin!.end(content);
    });
  }

  return { url, container };
}

async function globalSetup() {
  const bin = await ensureMaquinistaBinary();
  const standalone = await ensureNextStandalone();

  let pg: { url: string; container: string } | undefined;
  try {
    pg = await startPostgres();
  } catch (err) {
    console.error("[playwright setup] Postgres fixture unavailable:", err);
    console.error(
      "[playwright setup] specs that rely on the DB will be skipped",
    );
  }

  const port = await pickFreePort();
  const listen = `127.0.0.1:${port}`;
  const homeDir = await import("node:fs/promises").then((fs) =>
    fs.mkdtemp(path.join(tmpdir(), "maquinista-e2e-home-")),
  );

  const child = spawn(
    bin,
    [
      "dashboard",
      "start",
      "--foreground",
      "--listen",
      listen,
      "--no-embed",
      standalone,
    ],
    {
      env: {
        ...process.env,
        HOME: homeDir,
        MAQUINISTA_DASHBOARD_LISTEN: listen,
        ...(pg ? { DATABASE_URL: pg.url } : {}),
      },
      stdio: ["ignore", "inherit", "inherit"],
      detached: false,
    },
  );

  child.on("exit", (code, signal) => {
    if (code !== null && code !== 0) {
      console.error(`[playwright setup] dashboard exited with code ${code}`);
    } else if (signal) {
      console.log(`[playwright setup] dashboard received ${signal}`);
    }
  });

  const url = `http://${listen}`;
  process.env.MAQUINISTA_DASHBOARD_URL = url;
  if (pg) process.env.MAQUINISTA_DASHBOARD_PG_URL = pg.url;

  await waitForHealthz(`${url}/api/healthz`, 60_000);

  await writeFile(
    STATE_FILE,
    JSON.stringify({
      pid: child.pid,
      homeDir,
      url,
      listen,
      pgContainer: pg?.container,
      pgUrl: pg?.url,
    }),
    "utf8",
  );
}

export default globalSetup;
