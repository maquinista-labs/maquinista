// Conversations page — Phase 1 placeholder. Phase 3 renders
// threaded chat bubbles merged from agent_inbox + agent_outbox.

export default function ConversationsPage() {
  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-2 text-xl font-semibold">Conversations</h2>
      <p
        data-testid="conversations-placeholder"
        className="text-sm text-muted-foreground"
      >
        Threaded conversation view across all agents. Wired up in
        Phase 3.
      </p>
    </section>
  );
}
