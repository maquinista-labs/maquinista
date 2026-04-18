"use client";

import { useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
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
import type { ModelChoice, RunnerChoice } from "@/lib/catalog";
import { HANDLE_REGEX, isValidHandle } from "@/lib/utils";

type Catalog = {
  runners: RunnerChoice[];
  models: Record<string, ModelChoice[]>;
  souls: Array<{ id: string; name: string; tagline: string | null }>;
};

type Availability =
  | { state: "idle" }
  | { state: "checking" }
  | { state: "available" }
  | { state: "taken" }
  | { state: "invalid" };

// SpawnAgent — "New agent" button on /agents opens a Sheet with
// four fields (handle, runtime, model, soul). Submit creates the
// agent via POST /api/agents and navigates to the detail page.
export function SpawnAgent() {
  const [open, setOpen] = useState(false);
  const [handle, setHandle] = useState("");
  const [runner, setRunner] = useState("");
  const [model, setModel] = useState("");
  const [soul, setSoul] = useState("");
  const [busy, setBusy] = useState(false);
  const [availability, setAvailability] = useState<Availability>({
    state: "idle",
  });
  const router = useRouter();
  const queryClient = useQueryClient();

  const catalog = useQuery<Catalog, Error>({
    queryKey: ["agents", "new-catalog"],
    queryFn: async () => {
      const res = await fetch("/api/agents/new-catalog", { cache: "no-store" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json() as Promise<Catalog>;
    },
    enabled: open,
  });

  // Default picklist seeds once the catalog lands.
  useEffect(() => {
    if (!catalog.data) return;
    if (!runner && catalog.data.runners.length > 0) {
      const firstRunner = catalog.data.runners[0].id;
      setRunner(firstRunner);
      setModel(catalog.data.models[firstRunner]?.[0]?.id ?? "");
    }
    if (!soul && catalog.data.souls.length > 0) {
      setSoul(catalog.data.souls[0].id);
    }
  }, [catalog.data, runner, soul]);

  // When the operator changes runner, default model follows.
  useEffect(() => {
    if (!catalog.data || !runner) return;
    const list = catalog.data.models[runner] ?? [];
    if (!list.find((m) => m.id === model)) {
      setModel(list[0]?.id ?? "");
    }
  }, [runner, catalog.data, model]);

  // Live handle availability, 300ms debounce.
  const debounceRef = useRef<number | null>(null);
  useEffect(() => {
    if (debounceRef.current !== null) {
      window.clearTimeout(debounceRef.current);
    }
    const trimmed = handle.trim();
    if (trimmed.length === 0) {
      setAvailability({ state: "idle" });
      return;
    }
    if (!isValidHandle(trimmed)) {
      setAvailability({ state: "invalid" });
      return;
    }
    setAvailability({ state: "checking" });
    debounceRef.current = window.setTimeout(async () => {
      try {
        const res = await fetch(
          `/api/agents/check-handle?h=${encodeURIComponent(trimmed)}`,
          { cache: "no-store" },
        );
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const body = (await res.json()) as {
          available: boolean;
          reason?: string;
        };
        if (!body.available && body.reason === "taken") {
          setAvailability({ state: "taken" });
        } else if (!body.available) {
          setAvailability({ state: "invalid" });
        } else {
          setAvailability({ state: "available" });
        }
      } catch {
        setAvailability({ state: "idle" });
      }
    }, 300);
    return () => {
      if (debounceRef.current !== null) {
        window.clearTimeout(debounceRef.current);
      }
    };
  }, [handle]);

  const canSubmit =
    !busy &&
    availability.state === "available" &&
    runner.length > 0 &&
    soul.length > 0;

  async function submit() {
    setBusy(true);
    try {
      const res = await fetch("/api/agents", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          handle: handle.trim(),
          runner,
          model,
          soul_template: soul,
        }),
      });
      if (res.status === 409) {
        const body = (await res.json().catch(() => ({}))) as {
          handle?: string;
        };
        toast.error(
          `Handle ${body.handle ?? handle.trim()} is already taken — pick another.`,
        );
        setAvailability({ state: "taken" });
        return;
      }
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      const body = (await res.json()) as { id: string };
      toast.success(`Spawned ${body.id}`);
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      setOpen(false);
      router.push(`/agents/${encodeURIComponent(body.id)}`);
    } catch (err) {
      toast.error(
        `Spawn failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  const soulList = catalog.data?.souls ?? [];
  const modelList = (catalog.data?.models[runner] ?? []) as ModelChoice[];
  const runnerList = catalog.data?.runners ?? [];

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button
          data-testid="spawn-agent-trigger"
          size="sm"
          variant="default"
          className="gap-1"
        >
          <PlusIcon className="h-4 w-4" aria-hidden />
          New agent
        </Button>
      </SheetTrigger>
      <SheetContent side="bottom" data-testid="spawn-agent-sheet">
        <SheetHeader>
          <SheetTitle>New agent</SheetTitle>
          <SheetDescription>
            Creates a fresh agent row + cloned soul. Reconcile brings
            its tmux pane online within seconds.
          </SheetDescription>
        </SheetHeader>

        <div className="flex flex-col gap-3 p-4">
          <label className="text-sm text-muted-foreground">
            Handle
            <input
              data-testid="spawn-agent-handle"
              autoFocus
              value={handle}
              onChange={(e) => setHandle(e.target.value)}
              placeholder="e.g. coder"
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            />
          </label>
          <p className="text-xs text-muted-foreground">
            {HANDLE_REGEX.source} — 2 to 32 lowercase letters, digits,
            hyphen, underscore. Reserved <code>t-</code> prefix is
            forbidden.
          </p>

          <div
            data-testid="spawn-agent-availability"
            data-state={availability.state}
            className="text-xs"
          >
            {availability.state === "checking" && (
              <span className="text-muted-foreground">Checking…</span>
            )}
            {availability.state === "available" && (
              <span className="text-emerald-500">Available</span>
            )}
            {availability.state === "taken" && (
              <span className="text-destructive">Already taken</span>
            )}
            {availability.state === "invalid" && (
              <span className="text-destructive">
                Handle does not match required format
              </span>
            )}
          </div>

          <label className="text-sm text-muted-foreground">
            Runtime
            <select
              data-testid="spawn-agent-runner"
              value={runner}
              onChange={(e) => setRunner(e.target.value)}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            >
              {runnerList.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.label}
                </option>
              ))}
            </select>
          </label>

          <label className="text-sm text-muted-foreground">
            Model
            <select
              data-testid="spawn-agent-model"
              value={model}
              onChange={(e) => setModel(e.target.value)}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            >
              {modelList.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.label}
                </option>
              ))}
            </select>
          </label>

          <label className="text-sm text-muted-foreground">
            Agent type
            <select
              data-testid="spawn-agent-soul"
              value={soul}
              onChange={(e) => setSoul(e.target.value)}
              className="mt-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm"
            >
              {soulList.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name} — {s.tagline ?? s.id}
                </option>
              ))}
            </select>
          </label>
        </div>

        <SheetFooter>
          <div className="flex gap-2 p-4">
            <Button
              data-testid="spawn-agent-submit"
              disabled={!canSubmit}
              onClick={submit}
            >
              {busy ? "Spawning…" : "Spawn"}
            </Button>
          </div>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
