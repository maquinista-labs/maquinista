// Inbox page — Phase 1 placeholder. Phase 3 feeds it from
// /api/agents/[id]/inbox (cross-agent merge view for the top-level
// route).

export default function InboxPage() {
  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-2 text-xl font-semibold">Inbox</h2>
      <p
        data-testid="inbox-placeholder"
        className="text-sm text-muted-foreground"
      >
        Pending and processing messages across all agents. Wired up
        in Phase 3.
      </p>
    </section>
  );
}
