"use client";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useInbox } from "@/lib/infinite";

// InboxList — reverse-chronological inbox rows with a "load more"
// button (explicit, not an intersection-observer scroll trigger —
// avoids accidental runaway pagination on mobile Safari).
export function InboxList({ agentId }: { agentId: string }) {
  const q = useInbox(agentId);

  const rows = q.data?.pages.flatMap((p) => p.rows) ?? [];

  if (q.isLoading) {
    return (
      <p
        data-testid="inbox-loading"
        className="py-6 text-center text-sm text-muted-foreground"
      >
        Loading inbox…
      </p>
    );
  }
  if (q.isError) {
    return (
      <p
        data-testid="inbox-error"
        className="py-6 text-center text-sm text-destructive"
      >
        {q.error.message}
      </p>
    );
  }
  if (rows.length === 0) {
    return (
      <p
        data-testid="inbox-empty"
        className="py-6 text-center text-sm text-muted-foreground"
      >
        No inbox rows.
      </p>
    );
  }

  return (
    <ul data-testid="inbox-list" className="flex flex-col gap-2 py-2">
      {rows.map((r) => (
        <li
          key={r.id}
          data-testid={`inbox-row-${r.id}`}
          className="rounded-lg border border-border/60 bg-card p-3"
        >
          <div className="flex items-center gap-2">
            <Badge variant="secondary" className="uppercase text-xs">
              {r.from_kind}
            </Badge>
            <Badge variant="outline" className="text-xs">
              {r.status}
            </Badge>
            {r.origin_channel && (
              <span className="text-xs text-muted-foreground">
                {r.origin_channel}
              </span>
            )}
            <time className="ml-auto text-xs text-muted-foreground">
              {new Date(r.enqueued_at).toLocaleTimeString()}
            </time>
          </div>
          <p className="mt-1 text-sm">{r.excerpt ?? "<empty>"}</p>
        </li>
      ))}
      {q.hasNextPage && (
        <Button
          data-testid="inbox-load-more"
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
