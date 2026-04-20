// Inbox page — external signals entering the system that are pending,
// in-flight, or failed. Shows Telegram messages, webhook payloads, and
// scheduled jobs. Excludes operator-sent dashboard messages (those live
// in Chats) and agent-to-agent / system control messages (plumbing).
//
// G.1 of plans/active/dashboard-gaps.md.

import { GlobalInboxList } from "@/components/dash/global-inbox-list";
import { getPool } from "@/lib/db";
import { listGlobalInbox } from "@/lib/queries";
import type { GlobalInboxRow } from "@/lib/types";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export default async function InboxPage() {
  let rows: GlobalInboxRow[] = [];
  let error: string | null = null;
  try {
    // No explicit status filter — listGlobalInbox defaults to
    // pending + processing + failed + dead (action-relevant only).
    rows = await listGlobalInbox(getPool());
  } catch (err) {
    error = err instanceof Error ? err.message : String(err);
  }

  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-1 text-xl font-semibold">Inbox</h2>
      <p className="mb-4 text-sm text-muted-foreground">
        External messages arriving from Telegram, webhooks, and scheduled
        jobs — pending, in-flight, or failed.
      </p>
      {error ? (
        <p
          data-testid="inbox-error"
          className="rounded border border-destructive/60 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {error}
        </p>
      ) : (
        <GlobalInboxList initial={rows} />
      )}
    </section>
  );
}
