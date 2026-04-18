"use client";

import { useState } from "react";
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
import { useQueryClient } from "@tanstack/react-query";

type CreateTemplateProps = {
  onCreated?: (templateId: string) => void;
};

export function CreateTemplate({ onCreated }: CreateTemplateProps) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({
    id: "",
    name: "",
    tagline: "",
    role: "",
    goal: "",
    core_truths: "",
    boundaries: "",
    vibe: "",
    continuity: "",
  });
  const queryClient = useQueryClient();

  const canSubmit =
    !busy &&
    form.id.trim().length > 0 &&
    form.name.trim().length > 0 &&
    form.role.trim().length > 0 &&
    form.goal.trim().length > 0;

  async function submit() {
    setBusy(true);
    try {
      const res = await fetch("/api/soul-templates", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          id: form.id.trim().toLowerCase().replace(/[^a-z0-9-]/g, "-"),
          name: form.name.trim(),
          tagline: form.tagline.trim() || null,
          role: form.role.trim(),
          goal: form.goal.trim(),
          core_truths: form.core_truths.trim(),
          boundaries: form.boundaries.trim(),
          vibe: form.vibe.trim(),
          continuity: form.continuity.trim(),
        }),
      });
      if (res.status === 409) {
        const body = (await res.json().catch(() => ({}))) as {
          existing_id?: string;
        };
        toast.error(
          `Template ${body.existing_id ?? form.id} already exists — pick a different ID.`,
        );
        return;
      }
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as {
          error?: string;
        };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      const body = (await res.json()) as { id: string };
      toast.success(`Created template ${body.id}`);
      queryClient.invalidateQueries({ queryKey: ["agents", "new-catalog"] });
      setOpen(false);
      setForm({
        id: "",
        name: "",
        tagline: "",
        role: "",
        goal: "",
        core_truths: "",
        boundaries: "",
        vibe: "",
        continuity: "",
      });
      onCreated?.(body.id);
    } catch (err) {
      toast.error(
        `Create failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button variant="ghost" size="sm" className="gap-1 w-full justify-start">
          <PlusIcon className="h-4 w-4" />
          Create new template
        </Button>
      </SheetTrigger>
      <SheetContent side="bottom" className="max-w-lg max-h-[90vh] overflow-y-auto">
        <SheetHeader>
          <SheetTitle>Create Soul Template</SheetTitle>
          <SheetDescription>
            Define a reusable agent identity that can be assigned to new agents.
          </SheetDescription>
        </SheetHeader>

        <div className="flex flex-col gap-3 p-4">
          <div className="grid grid-cols-2 gap-3">
            <label className="text-sm text-muted-foreground">
              Template ID
              <input
                value={form.id}
                onChange={(e) =>
                  setForm({ ...form, id: e.target.value.toLowerCase() })
                }
                placeholder="e.g. reviewer"
                className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
              />
            </label>
            <label className="text-sm text-muted-foreground">
              Display Name
              <input
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="e.g. Code Reviewer"
                className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
              />
            </label>
          </div>

          <label className="text-sm text-muted-foreground">
            Tagline
            <input
              value={form.tagline}
              onChange={(e) => setForm({ ...form, tagline: e.target.value })}
              placeholder="e.g. Gatekeeper: catch bugs before they ship"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <label className="text-sm text-muted-foreground">
            Role
            <input
              value={form.role}
              onChange={(e) => setForm({ ...form, role: e.target.value })}
              placeholder="e.g. Reviewer"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <label className="text-sm text-muted-foreground">
            Goal
            <textarea
              value={form.goal}
              onChange={(e) => setForm({ ...form, goal: e.target.value })}
              placeholder="e.g. Review PRs thoroughly, flag issues, approve only when safe"
              rows={2}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <label className="text-sm text-muted-foreground">
            Core Truths
            <textarea
              value={form.core_truths}
              onChange={(e) =>
                setForm({ ...form, core_truths: e.target.value })
              }
              placeholder="One principle per line, e.g.:&#10;- Always verify before approving&#10;- Flag security issues immediately"
              rows={3}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <label className="text-sm text-muted-foreground">
            Boundaries
            <textarea
              value={form.boundaries}
              onChange={(e) =>
                setForm({ ...form, boundaries: e.target.value })
              }
              placeholder="What this agent should NOT do, e.g.:&#10;- Do not modify code&#10;- Do not merge without approval"
              rows={2}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <label className="text-sm text-muted-foreground">
            Vibe
            <input
              value={form.vibe}
              onChange={(e) => setForm({ ...form, vibe: e.target.value })}
              placeholder="e.g. Thorough, skeptical, kind"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>

          <label className="text-sm text-muted-foreground">
            Continuity
            <textarea
              value={form.continuity}
              onChange={(e) =>
                setForm({ ...form, continuity: e.target.value })
              }
              placeholder="How state persists, e.g.: Reviews persist in PR threads"
              rows={2}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>
        </div>

        <SheetFooter>
          <div className="flex gap-2 p-4">
            <Button variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!canSubmit} onClick={submit}>
              {busy ? "Creating…" : "Create Template"}
            </Button>
          </div>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}