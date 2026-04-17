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
import { access, cp, mkdir, readFile, stat, writeFile } from "node:fs/promises";
import { constants } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import process from "node:process";
import { setTimeout as sleep } from "node:timers/promises";
import { execFile } from "node:child_process";
import { promisify } from "node:util";

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
  const configMTime = (await stat(path.join(WEB_DIR, "next.config.ts")))
    .mtimeMs;
  const stale = !(await exists(serverJS))
    ? true
    : (await stat(serverJS)).mtimeMs < configMTime;
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

async function globalSetup() {
  const bin = await ensureMaquinistaBinary();
  const standalone = await ensureNextStandalone();

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

  await waitForHealthz(`${url}/api/healthz`, 60_000);

  await writeFile(
    STATE_FILE,
    JSON.stringify({ pid: child.pid, homeDir, url, listen }),
    "utf8",
  );
}

export default globalSetup;
