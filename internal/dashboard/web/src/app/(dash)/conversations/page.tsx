// Chats page — the single cross-agent mailbox surface. Merges the
// old Phase-1 /inbox + /conversations placeholders into one feed per
// operator feedback: a dedicated "pending messages" feed duplicated
// what this view already shows via pending_count.
//
// Server Component renders the initial list from Postgres; the
// <ConversationList> client component takes it from there with a
// 5 s polling interval.

import { ConversationList } from "@/components/dash/conversation-list";
import { getPool } from "@/lib/db";
import { listConversations } from "@/lib/queries";
import type { ConversationRow } from "@/lib/types";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export default async function ChatsPage() {
  let rows: ConversationRow[] = [];
  let error: string | null = null;
  try {
    rows = await listConversations(getPool());
  } catch (err) {
    error = err instanceof Error ? err.message : String(err);
  }

  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-3 text-xl font-semibold">Chats</h2>
      {error ? (
        <p
          data-testid="chats-error"
          className="rounded border border-destructive/60 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {error}
        </p>
      ) : (
        <ConversationList initial={rows} />
      )}
    </section>
  );
}
