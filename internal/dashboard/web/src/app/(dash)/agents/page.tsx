// Agents page — Phase 1 placeholder. Phase 2 replaces this with
// the live agent-card list backed by /api/agents + SSE.

export default function AgentsPage() {
  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-2 text-xl font-semibold">Agents</h2>
      <p
        data-testid="agents-placeholder"
        className="text-sm text-muted-foreground"
      >
        No agents to show yet. Phase 2 lights this page up with live
        cards from <code>/api/agents</code>.
      </p>
    </section>
  );
}
