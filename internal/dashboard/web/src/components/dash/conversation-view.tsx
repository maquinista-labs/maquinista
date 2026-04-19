"use client";

import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import ReactMarkdown from "react-markdown";

import { cn } from "@/lib/utils";
import { useAgentLive } from "@/lib/use-agent-live";
import type { ConversationItem } from "@/lib/types";

const TOOL_EMOJI: Record<string, string> = {
  bash: "🖥",
  computer: "🖥",
  edit_file: "✏️",
  edit: "✏️",
  write_file: "✏️",
  write: "✏️",
  read_file: "📄",
  read: "📄",
  list_files: "📂",
  glob: "📁",
  web_search: "🔍",
  websearch: "🔍",
  web_fetch: "🌐",
  webfetch: "🌐",
  grep: "🔎",
};

function toolEmoji(name: string): string {
  return TOOL_EMOJI[name.toLowerCase()] ?? "🔮";
}

function formatElapsed(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

// Props: either agentId (timeline view) or conversationId (single thread).
// liveAgentId enables inline tool-call display via SSE — pass the agent's id
// in both cases so the live feed is always wired up.
type Props =
  | { agentId: string; conversationId?: undefined; liveAgentId?: string }
  | { conversationId: string; agentId?: undefined; liveAgentId?: string };

const mdComponents = {
  p: ({ children }: { children?: React.ReactNode }) => (
    <p className="mb-1 last:mb-0 leading-snug">{children}</p>
  ),
  strong: ({ children }: { children?: React.ReactNode }) => (
    <strong className="font-semibold">{children}</strong>
  ),
  em: ({ children }: { children?: React.ReactNode }) => (
    <em className="italic">{children}</em>
  ),
  code: ({ children }: { children?: React.ReactNode }) => (
    <code className="rounded bg-foreground/10 px-1 py-0.5 font-mono text-xs">
      {children}
    </code>
  ),
  pre: ({ children }: { children?: React.ReactNode }) => (
    <pre className="my-1 overflow-x-auto rounded bg-foreground/8 border border-border/40 p-2 font-mono text-xs">
      {children}
    </pre>
  ),
  ul: ({ children }: { children?: React.ReactNode }) => (
    <ul className="my-1 ml-4 list-disc">{children}</ul>
  ),
  ol: ({ children }: { children?: React.ReactNode }) => (
    <ol className="my-1 ml-4 list-decimal">{children}</ol>
  ),
  li: ({ children }: { children?: React.ReactNode }) => (
    <li className="leading-snug">{children}</li>
  ),
  h1: ({ children }: { children?: React.ReactNode }) => (
    <h1 className="mb-1 text-base font-bold">{children}</h1>
  ),
  h2: ({ children }: { children?: React.ReactNode }) => (
    <h2 className="mb-1 text-sm font-bold">{children}</h2>
  ),
  h3: ({ children }: { children?: React.ReactNode }) => (
    <h3 className="mb-1 text-sm font-semibold">{children}</h3>
  ),
  table: ({ children }: { children?: React.ReactNode }) => (
    <div className="my-1 overflow-x-auto">
      <table className="min-w-full text-xs border-collapse">{children}</table>
    </div>
  ),
  thead: ({ children }: { children?: React.ReactNode }) => (
    <thead className="border-b border-current/20">{children}</thead>
  ),
  th: ({ children }: { children?: React.ReactNode }) => (
    <th className="px-2 py-1 text-left font-semibold">{children}</th>
  ),
  td: ({ children }: { children?: React.ReactNode }) => (
    <td className="px-2 py-1 border-t border-current/10">{children}</td>
  ),
  a: ({ href, children }: { href?: string; children?: React.ReactNode }) => (
    <a
      href={href}
      className="underline underline-offset-2 opacity-80 hover:opacity-100"
      target="_blank"
      rel="noopener noreferrer"
    >
      {children}
    </a>
  ),
  blockquote: ({ children }: { children?: React.ReactNode }) => (
    <blockquote className="my-1 border-l-2 border-current/40 pl-2 opacity-80">
      {children}
    </blockquote>
  ),
};

export function ConversationView(props: Props) {
  const agentId = "agentId" in props ? props.agentId : undefined;
  const conversationId =
    "conversationId" in props ? props.conversationId : undefined;
  const liveAgentId = props.liveAgentId ?? agentId;

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
    refetchInterval: 3000,
  });

  // Live tool calls from SSE — shown inline at the bottom while in-flight.
  const liveCalls = useAgentLive(liveAgentId ?? "");

  const bottomRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [q.data, liveCalls.length]);

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

  const isEmpty = items.length === 0 && liveCalls.length === 0;
  if (isEmpty) {
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
                "max-w-[80%] rounded-2xl px-3 py-2 text-sm",
                right
                  ? "bg-primary/12 text-foreground border border-primary/20"
                  : "bg-muted/50 text-foreground border border-border/60",
              )}
            >
              <ReactMarkdown components={mdComponents}>
                {it.excerpt ?? "*empty*"}
              </ReactMarkdown>
              <time
                className={cn(
                  "mt-1 block text-[10px] text-muted-foreground",
                  right ? "text-right" : "text-left",
                )}
              >
                {new Date(it.at).toLocaleTimeString()}
              </time>
            </div>
          </div>
        );
      })}

      {/* Live tool calls — inline at bottom, fade out when done */}
      {liveCalls.map((c) => (
        <div
          key={c.callId}
          data-testid={`conv-tool-${c.callId}`}
          className={cn(
            "flex w-full justify-start transition-opacity duration-500",
            c.status === "done" ? "opacity-30" : "opacity-100",
          )}
        >
          <div className="flex items-center gap-1.5 rounded-lg border border-border/50 bg-muted/30 px-2.5 py-1.5 font-mono text-xs text-muted-foreground">
            <span className="text-base leading-none">{toolEmoji(c.toolName)}</span>
            <span className="font-medium text-foreground">{c.toolName}</span>
            <span className="opacity-60">{formatElapsed(c.elapsedMs)}</span>
            {c.status === "done" && (
              <span className="text-green-600 dark:text-green-400">✓</span>
            )}
            {c.status === "running" && (
              <span className="animate-pulse opacity-70">…</span>
            )}
          </div>
        </div>
      ))}

      <div ref={bottomRef} />
    </div>
  );
}
