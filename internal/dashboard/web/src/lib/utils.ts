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

// displayName returns the operator-facing label for an agent —
// handle when present, otherwise the stable id.
export function displayName(a: {
  handle: string | null;
  id: string;
}): string {
  return a.handle ?? a.id;
}
