// Tiny helpers for encoding SSE frames. Extracted from the route
// handler so Vitest can exercise them without spinning up a server.

export type SSEFrame = {
  event?: string;
  data: unknown;
  id?: string;
  retry?: number;
};

const NL = "\n";

export function encodeSSE(frame: SSEFrame): string {
  const parts: string[] = [];
  if (frame.event) parts.push(`event: ${frame.event}`);
  if (frame.id) parts.push(`id: ${frame.id}`);
  if (typeof frame.retry === "number") parts.push(`retry: ${frame.retry}`);
  const data =
    typeof frame.data === "string" ? frame.data : JSON.stringify(frame.data);
  // Data lines split on newlines per the SSE spec.
  for (const line of data.split(/\r?\n/)) {
    parts.push(`data: ${line}`);
  }
  return parts.join(NL) + NL + NL;
}
