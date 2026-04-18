"use client";

import { useState } from "react";
import { PlusIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
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
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="ghost" size="sm" className="gap-1 w-full justify-start">
          <PlusIcon className="h-4 w-4" />
          Create new template
        </Button>
      </DialogTrigger>
      <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Create Soul Template</DialogTitle>
          <DialogDescription>
            Define a reusable agent identity that can be assigned to new agents.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-4">
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2">
              <Label htmlFor="template-id">Template ID</Label>
              <Input
                id="template-id"
                value={form.id}
                onChange={(e) =>
                  setForm({ ...form, id: e.target.value.toLowerCase() })
                }
                placeholder="e.g. reviewer"
              />
              <p className="text-xs text-muted-foreground">
                Lowercase, hyphen allowed
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="template-name">Display Name</Label>
              <Input
                id="template-name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="e.g. Code Reviewer"
              />
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="template-tagline">Tagline</Label>
            <Input
              id="template-tagline"
              value={form.tagline}
              onChange={(e) => setForm({ ...form, tagline: e.target.value })}
              placeholder="e.g. Gatekeeper: catch bugs before they ship"
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="template-role">Role</Label>
            <Input
              id="template-role"
              value={form.role}
              onChange={(e) => setForm({ ...form, role: e.target.value })}
              placeholder="e.g. Reviewer"
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="template-goal">Goal</Label>
            <Textarea
              id="template-goal"
              value={form.goal}
              onChange={(e) => setForm({ ...form, goal: e.target.value })}
              placeholder="e.g. Review PRs thoroughly, flag issues, approve only when safe"
              rows={2}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="template-truths">Core Truths</Label>
            <Textarea
              id="template-truths"
              value={form.core_truths}
              onChange={(e) =>
                setForm({ ...form, core_truths: e.target.value })
              }
              placeholder="One principle per line, e.g.:&#10;- Always verify before approving&#10;- Flag security issues immediately"
              rows={3}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="template-boundaries">Boundaries</Label>
            <Textarea
              id="template-boundaries"
              value={form.boundaries}
              onChange={(e) =>
                setForm({ ...form, boundaries: e.target.value })
              }
              placeholder="What this agent should NOT do, e.g.:&#10;- Do not modify code&#10;- Do not merge without approval"
              rows={2}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="template-vibe">Vibe</Label>
            <Input
              id="template-vibe"
              value={form.vibe}
              onChange={(e) => setForm({ ...form, vibe: e.target.value })}
              placeholder="e.g. Thorough, skeptical, kind"
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="template-continuity">Continuity</Label>
            <Textarea
              id="template-continuity"
              value={form.continuity}
              onChange={(e) =>
                setForm({ ...form, continuity: e.target.value })
              }
              placeholder="How state persists, e.g.: Reviews persist in PR threads"
              rows={2}
            />
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>
            Cancel
          </Button>
          <Button disabled={!canSubmit} onClick={submit}>
            {busy ? "Creating…" : "Create Template"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}