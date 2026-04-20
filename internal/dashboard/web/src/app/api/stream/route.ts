import { Client } from "pg";

import { encodeSSE } from "@/lib/sse-encoder";

// GET /api/stream — SSE multiplexer over pg LISTEN channels.
//
// Each client connection opens its own pg Client (NOT a pool
// client — LISTEN is session-scoped and holding a pool client
// indefinitely would starve the pool). The Go daemon already emits
// these channels from its migrations:
//
//   agent_inbox_new       payload = agent_id
//   agent_outbox_new      payload = outbox_id
//   channel_delivery_new  payload = delivery_id
//   agent_stop            payload = agent_id
//
// The SSE stream emits one frame per NOTIFY, plus a periodic
// keepalive comment every 15 s so aggressive proxies (Cloudflare
// etc.) don't reap the connection.

export const dynamic = "force-dynamic";
export const revalidate = 0;

const CHANNELS = [
  "agent_inbox_new",
  "agent_outbox_new",
  "channel_delivery_new",
  "agent_stop",
  "tool_event",
  "agent_status",
] as const;

export async function GET(req: Request) {
  const databaseUrl = process.env.DATABASE_URL;
  if (!databaseUrl) {
    return new Response(
      JSON.stringify({ error: "DATABASE_URL not set" }),
      {
        status: 500,
        headers: { "content-type": "application/json" },
      },
    );
  }

  const encoder = new TextEncoder();

  // Dedicated client per connection.
  const client = new Client({ connectionString: databaseUrl });

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      // Helper closures — keep all state in locals.
      let closed = false;
      const safeEnqueue = (bytes: Uint8Array) => {
        if (closed) return;
        try {
          controller.enqueue(bytes);
        } catch {
          // controller already torn down
          closed = true;
        }
      };
      const push = (event: string, data: unknown) => {
        safeEnqueue(
          encoder.encode(
            encodeSSE({ event, data, id: String(Date.now()) }),
          ),
        );
      };

      try {
        await client.connect();
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err);
        safeEnqueue(
          encoder.encode(
            encodeSSE({ event: "error", data: { error: msg } }),
          ),
        );
        controller.close();
        return;
      }

      // Kick off a ready frame so the client knows the pipe is open.
      safeEnqueue(
        encoder.encode(
          encodeSSE({
            event: "ready",
            data: { channels: CHANNELS },
            retry: 5000,
          }),
        ),
      );

      client.on("notification", (msg) => {
        console.log(`[sse] notify channel=${msg.channel} payload=${msg.payload?.slice(0, 80)}`);
        push(msg.channel, { payload: msg.payload });
      });
      client.on("error", (err) => {
        push("error", { error: err.message });
      });

      for (const ch of CHANNELS) {
        await client.query(`LISTEN ${ch}`);
      }

      // Keepalive every 15 s.
      const keepalive = setInterval(() => {
        safeEnqueue(encoder.encode(`: keepalive\n\n`));
      }, 15_000);

      const cleanup = async () => {
        if (closed) return;
        closed = true;
        clearInterval(keepalive);
        try {
          for (const ch of CHANNELS) {
            await client.query(`UNLISTEN ${ch}`);
          }
        } catch {
          /* best-effort */
        }
        try {
          await client.end();
        } catch {
          /* best-effort */
        }
        try {
          controller.close();
        } catch {
          /* already closed */
        }
      };

      // AbortSignal fires when the browser disconnects.
      req.signal.addEventListener("abort", () => {
        void cleanup();
      });
    },
    async cancel() {
      try {
        await client.end();
      } catch {
        /* ignore */
      }
    },
  });

  return new Response(stream, {
    status: 200,
    headers: {
      "content-type": "text/event-stream",
      "cache-control": "no-store, no-transform",
      connection: "keep-alive",
      // Disable Nginx buffering if ever fronted by it.
      "x-accel-buffering": "no",
    },
  });
}
