import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// Handle format contract — migration 014. The reserved prefix
// `t-` is forbidden so handles can't shadow the auto-generated
// `t-<chat>-<thread>` ids.
export const HANDLE_REGEX = /^[a-z0-9_-]{2,32}$/;

export function isValidHandle(h: string): boolean {
  if (!HANDLE_REGEX.test(h)) return false;
  if (h.startsWith("t-")) return false;
  return true;
}

// ---------------------------------------------------------------------------
// Fun agent name generator — adjective + noun + noun, e.g. "blazing-robot-cloud"
// ~10 000 unique combos; all slots are lowercase [a-z], output passes HANDLE_REGEX.
// ---------------------------------------------------------------------------

const _ADJ = [
  "amber", "blazing", "bright", "calm", "cobalt",
  "cosmic", "crisp", "drifting", "fancy", "frozen",
  "fuzzy", "golden", "hollow", "jade", "lunar",
  "nimble", "quiet", "rusty", "silent", "sleek",
  "solar", "swift", "turbo", "velvet", "wild",
] as const;

const _NOUN_A = [
  "beacon", "cipher", "circuit", "cloud", "comet",
  "core", "drone", "ember", "engine", "forge",
  "ghost", "orbit", "pixel", "prism", "relay",
  "robot", "signal", "spark", "storm", "vector",
] as const;

const _NOUN_B = [
  "bloom", "cloud", "drift", "field", "forge",
  "gate", "grid", "layer", "light", "mesh",
  "node", "patch", "peak", "ridge", "ring",
  "shore", "sphere", "stream", "vault", "wave",
] as const;

function _pick<T>(arr: readonly T[]): T {
  return arr[Math.floor(Math.random() * arr.length)];
}

/** Returns a random name like "blazing-robot-cloud". Always passes isValidHandle(). */
export function generateAgentName(): string {
  return `${_pick(_ADJ)}-${_pick(_NOUN_A)}-${_pick(_NOUN_B)}`;
}

// displayName returns the operator-facing label for an agent —
// handle when present, otherwise the stable id.
export function displayName(a: {
  handle: string | null;
  id: string;
}): string {
  return a.handle ?? a.id;
}
