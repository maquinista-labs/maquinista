"use client";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useOutbox } from "@/lib/infinite";

export function OutboxList({ agentId }: { agentId: string }) {
  const q = useOutbox(agentId);
  const rows = q.data?.pages.flatMap((p) => p.rows) ?? [];

  if (q.isLoading) {
    return (
      <p
        data-testid="outbox-loading"
        className="py-6 text-center text-sm text-muted-foreground"
      >
        Loading outbox…
      </p>
    );
  }
  if (q.isError) {
    return (
      <p
        data-testid="outbox-error"
        className="py-6 text-center text-sm text-destructive"
      >
        {q.error.message}
      </p>
    );
  }
  if (rows.length === 0) {
    return (
      <p
        data-testid="outbox-empty"
        className="py-6 text-center text-sm text-muted-foreground"
      >
        No outbox rows.
      </p>
    );
  }

  return (
    <ul data-testid="outbox-list" className="flex flex-col gap-2 py-2">
      {rows.map((r) => (
        <li
          key={r.id}
          data-testid={`outbox-row-${r.id}`}
          className="rounded-lg border border-border/60 bg-card p-3"
        >
          <div className="flex items-center gap-2">
            <Badge variant="outline" className="text-xs">
              {r.status}
            </Badge>
            <time className="ml-auto text-xs text-muted-foreground">
              {new Date(r.created_at).toLocaleTimeString()}
            </time>
          </div>
          <p className="mt-1 text-sm">{r.excerpt ?? "<empty>"}</p>
        </li>
      ))}
      {q.hasNextPage && (
        <Button
          data-testid="outbox-load-more"
          variant="outline"
          onClick={() => q.fetchNextPage()}
          disabled={q.isFetchingNextPage}
        >
          {q.isFetchingNextPage ? "Loading…" : "Load more"}
        </Button>
      )}
    </ul>
  );
}
