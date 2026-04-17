// Jobs page — scheduled_jobs + webhook_handlers, fed by /api/jobs.

import { JobsPageClient } from "@/components/dash/jobs-page-client";
import { getPool } from "@/lib/db";
import { listJobs } from "@/lib/queries";
import type { JobsList } from "@/lib/types";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export default async function JobsPage() {
  let jobs: JobsList = { scheduled: [], webhooks: [] };
  let error: string | null = null;
  try {
    jobs = await listJobs(getPool());
  } catch (err) {
    error = err instanceof Error ? err.message : String(err);
  }

  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-3 text-xl font-semibold">Jobs</h2>

      {error && (
        <p
          data-testid="jobs-error"
          className="rounded border border-destructive/60 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {error}
        </p>
      )}

      {!error && <JobsPageClient initial={jobs} />}
    </section>
  );
}
