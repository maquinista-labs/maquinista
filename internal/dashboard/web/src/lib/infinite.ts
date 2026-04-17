"use client";

import {
  useInfiniteQuery,
  type QueryKey,
} from "@tanstack/react-query";

import type { InboxRow, OutboxRow } from "@/lib/types";

type Page<T> = { rows: T[]; nextCursor: string | null };

async function fetchPage<T>(
  url: string,
  cursor: string | undefined,
): Promise<Page<T>> {
  const u = new URL(url, window.location.origin);
  if (cursor) u.searchParams.set("before", cursor);
  const res = await fetch(u.toString(), { cache: "no-store" });
  if (!res.ok) throw new Error(`GET ${u.pathname} ${res.status}`);
  return (await res.json()) as Page<T>;
}

export function useInbox(agentId: string) {
  return useInfiniteQuery<Page<InboxRow>, Error>({
    queryKey: ["inbox", agentId] as QueryKey,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      fetchPage<InboxRow>(
        `/api/agents/${encodeURIComponent(agentId)}/inbox?limit=50`,
        pageParam as string | undefined,
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
}

export function useOutbox(agentId: string) {
  return useInfiniteQuery<Page<OutboxRow>, Error>({
    queryKey: ["outbox", agentId] as QueryKey,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      fetchPage<OutboxRow>(
        `/api/agents/${encodeURIComponent(agentId)}/outbox?limit=50`,
        pageParam as string | undefined,
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
}
