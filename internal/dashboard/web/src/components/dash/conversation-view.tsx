"use client";

import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import ReactMarkdown from "react-markdown";

import { cn } from "@/lib/utils";
import type { ConversationItem } from "@/lib/types";

// Simple chat-bubble view. Outbox (agent) bubbles right-aligned,
// inbox (counterpart) left-aligned. Auto-scrolls on mount and
// when new items arrive so the most recent message is in view.
type Props =
  | { agentId: string; conversationId?: undefined }
  | { conversationId: string; agentId?: undefined };

export function ConversationView(props: Props) {
  const agentId = "agentId" in props ? props.agentId : undefined;
  const conversationId =
    "conversationId" in props ? props.conversationId : undefined;

  const queryKey = agentId
    ? (["conversation", "agent", agentId] as const)
    : (["conversation", conversationId] as const);
  const queryPath = agentId
    ? `/api/agents/${encodeURIComponent(agentId)}/timeline`
    : `/api/conversations/${encodeURIComponent(conversationId!)}`;

  const q = useQuery<{ items: ConversationItem[] }, Error>({
    queryKey,
    queryFn: async () => {
      const res = await fetch(queryPath, { cache: "no-store" });
      if (!res.ok) throw new Error(`GET ${queryPath} ${res.status}`);
      return res.json() as Promise<{ items: ConversationItem[] }>;
    },
  });

  const bottomRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [q.data]);

  if (q.isLoading) {
    return (
      <p
        data-testid="conv-loading"
        className="py-6 text-center text-sm text-muted-foreground"
      >
        Loading conversation…
      </p>
    );
  }
  if (q.isError) {
    return (
      <p
        data-testid="conv-error"
        className="py-6 text-center text-sm text-destructive"
      >
        {q.error.message}
      </p>
    );
  }
  const items = q.data?.items ?? [];
  if (items.length === 0) {
    return (
      <p
        data-testid="conv-empty"
        className="py-6 text-center text-sm text-muted-foreground"
      >
        No messages in this conversation.
      </p>
    );
  }

  return (
    <div
      data-testid="conv-view"
      data-agent-id={agentId ?? ""}
      data-conversation-id={conversationId ?? ""}
      className="flex flex-col gap-2 py-3"
    >
      {items.map((it) => {
        const right = it.kind === "outbox"; // agent speaks → right
        return (
          <div
            key={`${it.kind}-${it.id}`}
            data-testid={`conv-bubble-${it.id}`}
            className={cn(
              "flex w-full",
              right ? "justify-end" : "justify-start",
            )}
          >
            <div
              className={cn(
                "max-w-[80%] rounded-2xl px-3 py-2 text-sm shadow-sm",
                right
                  ? "bg-primary text-primary-foreground"
                  : "bg-card text-card-foreground",
              )}
            >
              <ReactMarkdown
                components={{
                  p: ({ children }) => <p className="mb-1 last:mb-0 leading-snug">{children}</p>,
                  strong: ({ children }) => <strong className="font-semibold">{children}</strong>,
                  em: ({ children }) => <em className="italic">{children}</em>,
                  code: ({ children }) => (
                    <code className="rounded bg-black/20 px-1 py-0.5 font-mono text-xs">{children}</code>
                  ),
                  pre: ({ children }) => (
                    <pre className="my-1 overflow-x-auto rounded bg-black/20 p-2 font-mono text-xs">{children}</pre>
                  ),
                  ul: ({ children }) => <ul className="my-1 ml-4 list-disc">{children}</ul>,
                  ol: ({ children }) => <ol className="my-1 ml-4 list-decimal">{children}</ol>,
                  li: ({ children }) => <li className="leading-snug">{children}</li>,
                  h1: ({ children }) => <h1 className="mb-1 text-base font-bold">{children}</h1>,
                  h2: ({ children }) => <h2 className="mb-1 text-sm font-bold">{children}</h2>,
                  h3: ({ children }) => <h3 className="mb-1 text-sm font-semibold">{children}</h3>,
                  table: ({ children }) => (
                    <div className="my-1 overflow-x-auto">
                      <table className="min-w-full text-xs border-collapse">{children}</table>
                    </div>
                  ),
                  thead: ({ children }) => <thead className="border-b border-current/20">{children}</thead>,
                  th: ({ children }) => <th className="px-2 py-1 text-left font-semibold">{children}</th>,
                  td: ({ children }) => <td className="px-2 py-1 border-t border-current/10">{children}</td>,
                  a: ({ href, children }) => (
                    <a href={href} className="underline underline-offset-2 opacity-80 hover:opacity-100" target="_blank" rel="noopener noreferrer">{children}</a>
                  ),
                  blockquote: ({ children }) => (
                    <blockquote className="my-1 border-l-2 border-current/40 pl-2 opacity-80">{children}</blockquote>
                  ),
                }}
              >
                {it.excerpt ?? "*empty*"}
              </ReactMarkdown>
              <time
                className={cn(
                  "mt-1 block text-[10px] opacity-70",
                  right ? "text-right" : "text-left",
                )}
              >
                {new Date(it.at).toLocaleTimeString()}
              </time>
            </div>
          </div>
        );
      })}
      <div ref={bottomRef} />
    </div>
  );
}
