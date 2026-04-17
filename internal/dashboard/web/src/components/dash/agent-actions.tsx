"use client";

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
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

// AgentActions — confirm-then-post trio (Interrupt / Kill /
// Respawn). Uses a Sheet rather than a dropdown so the destructive
// confirmation is explicit on touch. Each action shows a toast
// with the result and invalidates the agents list so status dots
// update immediately.
export function AgentActions({ agentId }: { agentId: string }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState<"kill" | "interrupt" | "respawn" | null>(
    null,
  );
  const queryClient = useQueryClient();

  async function fire(action: "interrupt" | "kill" | "respawn") {
    setBusy(action);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(agentId)}/${action}`,
        { method: "POST" },
      );
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      toast.success(`${action}: ok`);
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      setOpen(false);
    } catch (err) {
      toast.error(
        `${action} failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(null);
    }
  }

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button
          data-testid="agent-actions-trigger"
          size="sm"
          variant="outline"
        >
          Actions…
        </Button>
      </SheetTrigger>
      <SheetContent side="bottom" data-testid="agent-actions-sheet">
        <SheetHeader>
          <SheetTitle>Agent actions — {agentId}</SheetTitle>
          <SheetDescription>
            Interrupt sends Ctrl+C. Kill flips stop_requested and the
            reconcile loop takes the pane down. Respawn clears
            tmux_window so the next reconcile starts fresh.
          </SheetDescription>
        </SheetHeader>
        <div className="flex flex-col gap-2 p-4">
          <Button
            data-testid="action-interrupt"
            onClick={() => fire("interrupt")}
            disabled={busy !== null}
            variant="secondary"
          >
            {busy === "interrupt" ? "Interrupting…" : "Interrupt"}
          </Button>
          <Button
            data-testid="action-respawn"
            onClick={() => fire("respawn")}
            disabled={busy !== null}
            variant="secondary"
          >
            {busy === "respawn" ? "Respawning…" : "Respawn"}
          </Button>
          <Button
            data-testid="action-kill"
            onClick={() => fire("kill")}
            disabled={busy !== null}
            variant="destructive"
          >
            {busy === "kill" ? "Killing…" : "Kill"}
          </Button>
        </div>
        <SheetFooter />
      </SheetContent>
    </Sheet>
  );
}
