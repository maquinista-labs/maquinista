// Jobs page — Phase 1 placeholder. Phase 4 wires it to
// scheduled_jobs + webhook_handlers.

export default function JobsPage() {
  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-2 text-xl font-semibold">Jobs</h2>
      <p
        data-testid="jobs-placeholder"
        className="text-sm text-muted-foreground"
      >
        Scheduled jobs and webhook handlers. Wired up in Phase 4.
      </p>
    </section>
  );
}
