"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import type { SoulTemplate } from "@/lib/types";

const TIMEZONES = [
  "UTC",
  "America/New_York",
  "America/Chicago",
  "America/Denver",
  "America/Los_Angeles",
  "America/Sao_Paulo",
  "Europe/London",
  "Europe/Paris",
  "Europe/Berlin",
  "Asia/Tokyo",
  "Asia/Shanghai",
  "Australia/Sydney",
];

// JobFormModal — "New job" button opens a Sheet form for creating a
// scheduled_jobs row.
export function JobFormModal() {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [cronExpr, setCronExpr] = useState("0 8 * * *");
  const [timezone, setTimezone] = useState("UTC");
  const [soulTemplateId, setSoulTemplateId] = useState("");
  const [prompt, setPrompt] = useState("");
  const [contextMarkdown, setContextMarkdown] = useState("");
  const [agentCwd, setAgentCwd] = useState("");
  const [showContext, setShowContext] = useState(false);
  const [busy, setBusy] = useState(false);

  const queryClient = useQueryClient();

  const templates = useQuery<SoulTemplate[], Error>({
    queryKey: ["soul-templates"],
    queryFn: async () => {
      const res = await fetch("/api/soul-templates", { cache: "no-store" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json() as Promise<SoulTemplate[]>;
    },
    enabled: open,
  });

  function reset() {
    setName("");
    setCronExpr("0 8 * * *");
    setTimezone("UTC");
    setSoulTemplateId("");
    setPrompt("");
    setContextMarkdown("");
    setAgentCwd("");
    setShowContext(false);
  }

  const canSubmit =
    !busy &&
    name.trim().length > 0 &&
    cronExpr.trim().length > 0 &&
    soulTemplateId.length > 0 &&
    prompt.trim().length > 0;

  async function submit() {
    setBusy(true);
    try {
      const res = await fetch("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: name.trim(),
          cron_expr: cronExpr.trim(),
          timezone,
          soul_template_id: soulTemplateId || undefined,
          prompt: prompt.trim(),
          context_markdown: contextMarkdown || undefined,
          agent_cwd: agentCwd || undefined,
        }),
      });
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      toast.success(`Job "${name.trim()}" created`);
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
      setOpen(false);
      reset();
    } catch (err) {
      toast.error(
        `Create failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  const tplList = templates.data ?? [];

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button
          data-testid="new-job-trigger"
          size="sm"
          variant="default"
          className="gap-1"
        >
          <PlusIcon className="h-4 w-4" aria-hidden />
          New job
        </Button>
      </SheetTrigger>
      <SheetContent side="bottom" data-testid="new-job-sheet">
        <SheetHeader>
          <SheetTitle>New scheduled job</SheetTitle>
          <SheetDescription>
            Spawns a fresh agent on the configured cron schedule.
          </SheetDescription>
        </SheetHeader>

        <div className="flex flex-col gap-3 p-4">
          <label className="text-sm text-muted-foreground">
            Job name
            <input
              data-testid="job-name"
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. daily-digest"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <label className="text-sm text-muted-foreground">
            Cron expression
            <input
              data-testid="job-cron"
              value={cronExpr}
              onChange={(e) => setCronExpr(e.target.value)}
              placeholder="0 8 * * *"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 font-mono text-sm"
            />
          </label>
          <p className="text-xs text-muted-foreground">
            5-field cron: minute hour dom month dow. Example:{" "}
            <code>0 8 * * *</code> = every day at 08:00.
          </p>

          <label className="text-sm text-muted-foreground">
            Timezone
            <select
              data-testid="job-timezone"
              value={timezone}
              onChange={(e) => setTimezone(e.target.value)}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            >
              {TIMEZONES.map((tz) => (
                <option key={tz} value={tz}>
                  {tz}
                </option>
              ))}
            </select>
          </label>

          <label className="text-sm text-muted-foreground">
            Soul template
            <select
              data-testid="job-soul-template"
              value={soulTemplateId}
              onChange={(e) => setSoulTemplateId(e.target.value)}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            >
              <option value="">— select —</option>
              {tplList.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name} {t.tagline ? `— ${t.tagline}` : ""}
                </option>
              ))}
            </select>
          </label>

          <label className="text-sm text-muted-foreground">
            Prompt
            <textarea
              data-testid="job-prompt"
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              rows={3}
              placeholder="What should the agent do on each run?"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <button
            type="button"
            onClick={() => setShowContext((v) => !v)}
            className="text-left text-xs text-muted-foreground underline underline-offset-2"
          >
            {showContext ? "Hide" : "Show"} background context (optional)
          </button>
          {showContext && (
            <label className="text-sm text-muted-foreground">
              Background context (markdown)
              <textarea
                data-testid="job-context"
                value={contextMarkdown}
                onChange={(e) => setContextMarkdown(e.target.value)}
                rows={4}
                placeholder="Optional markdown injected before the prompt on each run."
                className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
              />
            </label>
          )}

          <label className="text-sm text-muted-foreground">
            Working directory (optional)
            <input
              data-testid="job-cwd"
              value={agentCwd}
              onChange={(e) => setAgentCwd(e.target.value)}
              placeholder="/home/user/project"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 font-mono text-sm"
            />
          </label>
        </div>

        <SheetFooter>
          <div className="flex gap-2 p-4">
            <Button
              data-testid="job-submit"
              disabled={!canSubmit}
              onClick={submit}
            >
              {busy ? "Creating…" : "Create job"}
            </Button>
          </div>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
