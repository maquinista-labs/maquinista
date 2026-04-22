"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDownIcon, ChevronRightIcon, PlayIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import type { JobExecution, JobsList } from "@/lib/types";
import { JobFormModal } from "./job-form-modal";

export function JobsPageClient({ initial }: { initial: JobsList }) {
  const queryClient = useQueryClient();

  const q = useQuery<JobsList, Error>({
    queryKey: ["jobs"],
    queryFn: async () => {
      const res = await fetch("/api/jobs", { cache: "no-store" });
      if (!res.ok) throw new Error(`GET /api/jobs ${res.status}`);
      return (await res.json()) as JobsList;
    },
    initialData: initial,
    staleTime: 30_000,
    refetchInterval: 60_000,
  });

  const jobs = q.data ?? initial;

  async function toggleJob(id: string, enabled: boolean) {
    try {
      const res = await fetch(`/api/jobs/${id}/toggle`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enabled }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
      toast.success(enabled ? "Job enabled" : "Job disabled");
    } catch (err) {
      toast.error(`Toggle failed: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  async function runNow(id: string) {
    try {
      const res = await fetch(`/api/jobs/${id}/run`, { method: "POST" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      toast.success("Job queued — scheduler will fire within 30s");
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
    } catch (err) {
      toast.error(`Run failed: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  async function deleteJob(id: string, name: string) {
    if (!confirm(`Delete job "${name}"?`)) return;
    try {
      const res = await fetch(`/api/jobs/${id}`, { method: "DELETE" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      toast.success(`Deleted "${name}"`);
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
    } catch (err) {
      toast.error(`Delete failed: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  return (
    <div data-testid="jobs-page" className="flex flex-col gap-4">
      <section data-testid="jobs-scheduled">
        <div className="mb-2 flex items-center justify-between">
          <h3 className="text-sm font-semibold uppercase tracking-wider text-muted-foreground">
            Scheduled ({jobs.scheduled.length})
          </h3>
          <JobFormModal />
        </div>
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
              <ScheduledJobCard
                key={j.id}
                job={j}
                onToggle={toggleJob}
                onRunNow={runNow}
                onDelete={deleteJob}
              />
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

type ScheduledJobCardProps = {
  job: JobsList["scheduled"][number];
  onToggle: (id: string, enabled: boolean) => void;
  onRunNow: (id: string) => void;
  onDelete: (id: string, name: string) => void;
};

function ScheduledJobCard({ job: j, onToggle, onRunNow, onDelete }: ScheduledJobCardProps) {
  const [showHistory, setShowHistory] = useState(false);

  const executions = useQuery<JobExecution[], Error>({
    queryKey: ["job-executions", j.id],
    queryFn: async () => {
      const res = await fetch(`/api/jobs/${j.id}/executions`, { cache: "no-store" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json() as Promise<JobExecution[]>;
    },
    enabled: showHistory,
    staleTime: 30_000,
  });

  const target = j.soul_template_name
    ? `soul: ${j.soul_template_name}`
    : j.agent_id
    ? `agent: ${j.agent_id}`
    : "—";

  return (
    <Card
      data-testid={`scheduled-${j.name}`}
      className="p-3"
    >
      <div className="flex items-center gap-2">
        <Badge
          variant={j.enabled ? "default" : "outline"}
          className={cn(!j.enabled && "opacity-70")}
        >
          {j.enabled ? "enabled" : "disabled"}
        </Badge>
        <span className="font-medium">{j.name}</span>
        <code className="ml-auto text-xs text-muted-foreground">
          {j.cron_expr}
        </code>
        <div className="flex gap-1">
          <Button
            data-testid={`job-toggle-${j.name}`}
            size="sm"
            variant="ghost"
            className="h-6 px-2 text-xs"
            onClick={() => onToggle(j.id, !j.enabled)}
          >
            {j.enabled ? "Disable" : "Enable"}
          </Button>
          <Button
            data-testid={`job-run-${j.name}`}
            size="sm"
            variant="ghost"
            className="h-6 px-2 text-xs"
            title="Run now"
            onClick={() => onRunNow(j.id)}
          >
            <PlayIcon className="h-3 w-3" aria-hidden />
          </Button>
          <Button
            data-testid={`job-delete-${j.name}`}
            size="sm"
            variant="ghost"
            className="h-6 px-2 text-xs text-destructive hover:text-destructive"
            title="Delete"
            onClick={() => onDelete(j.id, j.name)}
          >
            <Trash2Icon className="h-3 w-3" aria-hidden />
          </Button>
        </div>
      </div>
      <div className="mt-1 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
        <span>{target}</span>
        <span>tz: {j.timezone}</span>
        <span>
          next: {new Date(j.next_run_at).toLocaleString()}
        </span>
        {j.last_run_at && (
          <span>
            last: {new Date(j.last_run_at).toLocaleString()}
          </span>
        )}
        <span>runs: {j.run_count}</span>
      </div>

      {/* Execution history toggle */}
      <button
        type="button"
        data-testid={`job-history-toggle-${j.name}`}
        className="mt-2 flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
        onClick={() => setShowHistory((v) => !v)}
      >
        {showHistory ? (
          <ChevronDownIcon className="h-3 w-3" aria-hidden />
        ) : (
          <ChevronRightIcon className="h-3 w-3" aria-hidden />
        )}
        Execution history
      </button>

      {showHistory && (
        <div className="mt-2">
          {executions.isLoading && (
            <p className="text-xs text-muted-foreground">Loading…</p>
          )}
          {executions.isError && (
            <p className="text-xs text-destructive">{executions.error.message}</p>
          )}
          {executions.data && executions.data.length === 0 && (
            <p className="text-xs text-muted-foreground">No executions yet.</p>
          )}
          {executions.data && executions.data.length > 0 && (
            <table className="w-full text-xs">
              <thead>
                <tr className="text-muted-foreground">
                  <th className="text-left">Agent</th>
                  <th className="text-left">Status</th>
                  <th className="text-left">Started</th>
                </tr>
              </thead>
              <tbody>
                {executions.data.map((e) => (
                  <tr key={e.id} className="border-t border-border/50">
                    <td className="py-0.5 font-mono">{e.agent_id ?? "—"}</td>
                    <td className="py-0.5">{e.agent_status ?? "—"}</td>
                    <td className="py-0.5">
                      {new Date(e.started_at).toLocaleString()}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </Card>
  );
}
