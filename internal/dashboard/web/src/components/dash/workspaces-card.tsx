"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";

// Phase 7 of plans/active/workspace-scopes.md: read + mutate
// agent_workspaces from the dashboard. Kept deliberately minimal —
// the card sits under a "workspaces" tab, renders a list with
// Switch / Archive actions, and a New-workspace inline form.

type Workspace = {
  id: string;
  agent_id: string;
  scope: "shared" | "agent" | "task";
  repo_root: string;
  worktree_dir: string | null;
  branch: string | null;
  created_at: string;
};

type WorkspacesResponse = {
  active_workspace_id: string | null;
  workspaces: Workspace[];
};

async function fetchWorkspaces(agentId: string): Promise<WorkspacesResponse> {
  const res = await fetch(
    `/api/agents/${encodeURIComponent(agentId)}/workspaces`,
    { cache: "no-store" },
  );
  if (!res.ok) {
    throw new Error(`workspaces fetch: HTTP ${res.status}`);
  }
  return res.json();
}

export function WorkspacesCard({ agentId }: { agentId: string }) {
  const qc = useQueryClient();
  const { data, isLoading, error } = useQuery({
    queryKey: ["workspaces", agentId],
    queryFn: () => fetchWorkspaces(agentId),
  });

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["workspaces", agentId] });

  if (isLoading) {
    return <div className="p-4 text-sm text-muted-foreground">Loading…</div>;
  }
  if (error) {
    return (
      <div className="p-4 text-sm text-destructive">
        Workspaces unavailable: {error instanceof Error ? error.message : String(error)}
      </div>
    );
  }

  const active = data?.active_workspace_id ?? null;
  const rows = data?.workspaces ?? [];

  return (
    <div
      className="flex flex-col gap-4 p-2"
      data-testid="workspaces-card"
    >
      <ul className="flex flex-col gap-2">
        {rows.length === 0 && (
          <li className="text-sm text-muted-foreground">
            No workspaces yet — create one below.
          </li>
        )}
        {rows.map((ws) => (
          <WorkspaceRow
            key={ws.id}
            ws={ws}
            isActive={ws.id === active}
            onMutated={invalidate}
          />
        ))}
      </ul>
      <NewWorkspaceForm agentId={agentId} onCreated={invalidate} />
    </div>
  );
}

function WorkspaceRow({
  ws,
  isActive,
  onMutated,
}: {
  ws: Workspace;
  isActive: boolean;
  onMutated: () => void;
}) {
  const [busy, setBusy] = useState(false);

  async function switchTo() {
    setBusy(true);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(ws.agent_id)}/workspaces/${encodeURIComponent(ws.id)}`,
        { method: "PATCH" },
      );
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      toast.success(`Switched to ${ws.id}`);
      onMutated();
    } catch (err) {
      toast.error(
        `Switch failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  async function archive() {
    setBusy(true);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(ws.agent_id)}/workspaces/${encodeURIComponent(ws.id)}`,
        { method: "DELETE" },
      );
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      toast.success(`Archived ${ws.id}`);
      onMutated();
    } catch (err) {
      toast.error(
        `Archive failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <li
      data-testid="workspace-row"
      data-workspace-id={ws.id}
      className="flex items-center justify-between rounded-md border border-border px-3 py-2"
    >
      <div className="flex min-w-0 flex-col">
        <div className="flex items-center gap-2 truncate font-mono text-sm">
          <span aria-hidden>{isActive ? "★" : "·"}</span>
          <span className="truncate">{ws.id}</span>
          <span className="rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
            {ws.scope}
          </span>
        </div>
        <div className="truncate text-xs text-muted-foreground">
          {ws.repo_root || <em>(no repo)</em>}
          {ws.branch ? ` — ${ws.branch}` : null}
        </div>
      </div>
      <div className="flex gap-2">
        <Button
          data-testid="workspace-switch"
          size="sm"
          variant="secondary"
          disabled={busy || isActive}
          onClick={switchTo}
        >
          Switch
        </Button>
        <Button
          data-testid="workspace-archive"
          size="sm"
          variant="ghost"
          disabled={busy || isActive}
          onClick={archive}
        >
          Archive
        </Button>
      </div>
    </li>
  );
}

function NewWorkspaceForm({
  agentId,
  onCreated,
}: {
  agentId: string;
  onCreated: () => void;
}) {
  const [label, setLabel] = useState("");
  const [scope, setScope] = useState<"shared" | "agent" | "task">("agent");
  const [repoRoot, setRepoRoot] = useState("");
  const [busy, setBusy] = useState(false);

  const labelOk = /^[A-Za-z0-9._-]+$/.test(label);
  const repoOk = scope === "shared" || repoRoot.trim().length > 0;
  const canSubmit = !busy && labelOk && repoOk;

  async function submit() {
    setBusy(true);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(agentId)}/workspaces`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ label, scope, repo_root: repoRoot.trim() }),
        },
      );
      if (res.status === 409) {
        toast.error(`Label "${label}" already exists for this agent.`);
        return;
      }
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      toast.success(`Created + activated ${agentId}@${label}`);
      setLabel("");
      setRepoRoot("");
      onCreated();
    } catch (err) {
      toast.error(
        `Create failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      data-testid="new-workspace-form"
      onSubmit={(e) => {
        e.preventDefault();
        if (canSubmit) submit();
      }}
      className="flex flex-col gap-2 rounded-md border border-dashed border-border p-3"
    >
      <div className="text-sm font-medium">New workspace</div>
      <input
        data-testid="new-workspace-label"
        value={label}
        onChange={(e) => setLabel(e.target.value)}
        placeholder="label (e.g. project-b)"
        className="rounded-md border border-border bg-background px-2 py-1 text-sm"
      />
      <select
        data-testid="new-workspace-scope"
        value={scope}
        onChange={(e) =>
          setScope(e.target.value as "shared" | "agent" | "task")
        }
        className="rounded-md border border-border bg-background px-2 py-1 text-sm"
      >
        <option value="shared">shared (no git isolation)</option>
        <option value="agent">agent (per-agent worktree)</option>
        <option value="task">task (per-task worktree)</option>
      </select>
      <input
        data-testid="new-workspace-repo"
        value={repoRoot}
        onChange={(e) => setRepoRoot(e.target.value)}
        placeholder="/absolute/path/to/repo"
        className="rounded-md border border-border bg-background px-2 py-1 text-sm"
      />
      <div className="flex justify-end">
        <Button
          data-testid="new-workspace-submit"
          type="submit"
          size="sm"
          disabled={!canSubmit}
        >
          {busy ? "Creating…" : "Create + activate"}
        </Button>
      </div>
    </form>
  );
}
