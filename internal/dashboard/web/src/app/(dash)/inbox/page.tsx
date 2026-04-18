// Inbox page — cross-agent flat feed of recent inbox activity.
// Shows all statuses so operators see what came in, not just
// what's still queued (empty-when-caught-up was confusing). The
// pending/processing rows still stand out via per-row badges.
//
// G.1 of plans/active/dashboard-gaps.md.

import { GlobalInboxList } from "@/components/dash/global-inbox-list";
import { getPool } from "@/lib/db";
import { listGlobalInbox } from "@/lib/queries";
import type { GlobalInboxRow, InboxRow } from "@/lib/types";

export const dynamic = "force-dynamic";
export const revalidate = 0;

const ALL_STATUSES: InboxRow["status"][] = [
  "pending",
  "processing",
  "processed",
  "failed",
  "dead",
];

export default async function InboxPage() {
  let rows: GlobalInboxRow[] = [];
  let error: string | null = null;
  try {
    rows = await listGlobalInbox(getPool(), { status: ALL_STATUSES });
  } catch (err) {
    error = err instanceof Error ? err.message : String(err);
  }

  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-3 text-xl font-semibold">Inbox</h2>
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
