"use client";

import { useQuery } from "@tanstack/react-query";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import type { JobsList } from "@/lib/types";

export function JobsPageClient({ initial }: { initial: JobsList }) {
  const q = useQuery<JobsList, Error>({
    queryKey: ["jobs"],
    queryFn: async () => {
      const res = await fetch("/api/jobs", { cache: "no-store" });
      if (!res.ok) throw new Error(`GET /api/jobs ${res.status}`);
      return (await res.json()) as JobsList;
    },
    initialData: initial,
    staleTime: 30_000,
  });

  const jobs = q.data ?? initial;

  return (
    <div data-testid="jobs-page" className="flex flex-col gap-4">
      <section data-testid="jobs-scheduled">
        <h3 className="mb-2 text-sm font-semibold uppercase tracking-wider text-muted-foreground">
          Scheduled ({jobs.scheduled.length})
        </h3>
        {jobs.scheduled.length === 0 ? (
          <p
            data-testid="jobs-scheduled-empty"
            className="text-sm text-muted-foreground"
          >
            No scheduled jobs.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {jobs.scheduled.map((j) => (
              <Card
                key={j.id}
                data-testid={`scheduled-${j.name}`}
                className="p-3"
              >
                <div className="flex items-center gap-2">
                  <Badge
                    variant={j.enabled ? "default" : "outline"}
                    className={cn(
                      !j.enabled && "opacity-70",
                    )}
                  >
                    {j.enabled ? "enabled" : "disabled"}
                  </Badge>
                  <span className="font-medium">{j.name}</span>
                  <code className="ml-auto text-xs text-muted-foreground">
                    {j.cron_expr}
                  </code>
                </div>
                <div className="mt-1 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
                  <span>agent: {j.agent_id}</span>
                  <span>tz: {j.timezone}</span>
                  <span>
                    next: {new Date(j.next_run_at).toLocaleString()}
                  </span>
                  {j.last_run_at && (
                    <span>
                      last: {new Date(j.last_run_at).toLocaleString()}
                    </span>
                  )}
                </div>
              </Card>
            ))}
          </ul>
        )}
      </section>

      <section data-testid="jobs-webhooks">
        <h3 className="mb-2 text-sm font-semibold uppercase tracking-wider text-muted-foreground">
          Webhooks ({jobs.webhooks.length})
        </h3>
        {jobs.webhooks.length === 0 ? (
          <p
            data-testid="jobs-webhooks-empty"
            className="text-sm text-muted-foreground"
          >
            No webhook handlers.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {jobs.webhooks.map((w) => (
              <Card
                key={w.id}
                data-testid={`webhook-${w.name}`}
                className="p-3"
              >
                <div className="flex items-center gap-2">
                  <Badge variant={w.enabled ? "default" : "outline"}>
                    {w.enabled ? "enabled" : "disabled"}
                  </Badge>
                  <span className="font-medium">{w.name}</span>
                  <code className="ml-auto text-xs text-muted-foreground">
                    {w.path}
                  </code>
                </div>
                <div className="mt-1 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
                  <span>agent: {w.agent_id}</span>
                  <span>limit: {w.rate_limit_per_min}/min</span>
                </div>
              </Card>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
