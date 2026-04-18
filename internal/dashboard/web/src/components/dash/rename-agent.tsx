"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";
import { PencilIcon } from "lucide-react";
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
import { HANDLE_REGEX, isValidHandle } from "@/lib/utils";

// RenameAgent — pencil-button affordance next to the agent-detail
// title. Opens a Sheet with a single input + Save/Clear buttons.
// Save is disabled while the input fails the regex. Submit posts to
// /api/agents/:id/rename; 409 surfaces a targeted toast.
export function RenameAgent({
  agentId,
  currentHandle,
}: {
  agentId: string;
  currentHandle: string | null;
}) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(currentHandle ?? "");
  const [busy, setBusy] = useState(false);
  const router = useRouter();
  const queryClient = useQueryClient();

  const trimmed = value.trim();
  const valid = trimmed === "" || isValidHandle(trimmed);
  const dirty = trimmed !== (currentHandle ?? "");

  async function submit(handle: string | null) {
    setBusy(true);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(agentId)}/rename`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ handle }),
        },
      );
      if (res.status === 409) {
        const body = (await res.json().catch(() => ({}))) as {
          handle?: string;
        };
        toast.error(
          `Handle ${body.handle ?? handle} is already taken — pick another.`,
        );
        return;
      }
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      toast.success(handle ? `Renamed to ${handle}` : "Handle cleared");
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      setOpen(false);
      // The page title is server-rendered — refresh the route so
      // the new label takes effect without a client-side patch.
      router.refresh();
    } catch (err) {
      toast.error(
        `Rename failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button
          data-testid="rename-agent-trigger"
          size="icon"
          variant="ghost"
          aria-label="Rename agent"
        >
          <PencilIcon className="h-4 w-4" aria-hidden />
        </Button>
      </SheetTrigger>
      <SheetContent side="bottom" data-testid="rename-agent-sheet">
        <SheetHeader>
          <SheetTitle>Rename agent</SheetTitle>
          <SheetDescription>
            Sets the operator-facing handle. The stable id ({agentId})
            is unchanged — mailbox history, souls, and tmux names are
            untouched.
          </SheetDescription>
        </SheetHeader>
        <div className="flex flex-col gap-3 p-4">
          <label className="text-sm text-muted-foreground">
            Handle
            <input
              data-testid="rename-agent-input"
              autoFocus
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder="e.g. coder"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>
          <p
            className="text-xs text-muted-foreground"
            data-testid="rename-agent-hint"
          >
            {HANDLE_REGEX.source} — 2 to 32 lowercase letters, digits,
            hyphen, underscore. Reserved prefix <code>t-</code> is
            forbidden.
          </p>
          {!valid && (
            <p
              data-testid="rename-agent-invalid"
              className="text-xs text-destructive"
            >
              Handle does not match the required format.
            </p>
          )}
        </div>
        <SheetFooter>
          <div className="flex gap-2 p-4">
            <Button
              data-testid="rename-agent-clear"
              variant="secondary"
              disabled={busy || currentHandle === null}
              onClick={() => submit(null)}
            >
              Clear
            </Button>
            <Button
              data-testid="rename-agent-save"
              disabled={busy || !valid || !dirty || trimmed.length === 0}
              onClick={() => submit(trimmed)}
            >
              {busy ? "Saving…" : "Save"}
            </Button>
          </div>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
