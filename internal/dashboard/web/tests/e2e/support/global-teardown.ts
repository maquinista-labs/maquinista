import { rm, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import process from "node:process";

const STATE_FILE = path.join(tmpdir(), "maquinista-e2e-state.json");

async function globalTeardown() {
  let state: { pid: number; homeDir: string } | undefined;
  try {
    state = JSON.parse(await readFile(STATE_FILE, "utf8"));
  } catch {
    return; // setup never completed
  }
  if (!state) return;

  try {
    process.kill(state.pid, "SIGTERM");
  } catch (err) {
    // Already dead — fine.
  }

  // Poll for exit; SIGKILL after 10 s.
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    try {
      process.kill(state.pid, 0);
    } catch {
      break;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  try {
    process.kill(state.pid, "SIGKILL");
  } catch {
    /* already gone */
  }

  try {
    await rm(state.homeDir, { recursive: true, force: true });
  } catch {
    /* best-effort */
  }
  try {
    await rm(STATE_FILE);
  } catch {
    /* ditto */
  }
}

export default globalTeardown;
