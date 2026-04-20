"use client";

import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { AnimatePresence, motion } from "framer-motion";

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
    <code className="rounded px-1 py-0.5 font-mono text-xs">
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

  // Live tool events from SSE — appended inline below historical items.
  const liveEvents = useAgentLive(liveAgentId ?? "");

  const bottomRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    // Defer one frame so framer-motion has committed the new element's height
    // before we measure scrollHeight. Target the outer scroll container directly
    // (dash-main) rather than relying on scrollIntoView, which can be confused
    // by absolutely-positioned children and layout shifts.
    const raf = requestAnimationFrame(() => {
      const main = document.querySelector(
        '[data-testid="dash-main"]',
      ) as HTMLElement | null;
      if (main) {
        main.scrollTo({ top: main.scrollHeight, behavior: "smooth" });
      }
    });
    return () => cancelAnimationFrame(raf);
  }, [q.data, liveEvents.length]);

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

  const isEmpty = items.length === 0 && liveEvents.length === 0;
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
      <AnimatePresence>
        {items.map((it, index) => {
          const right = it.kind === "outbox"; // agent speaks → right
          return (
            <motion.div
              key={`${it.kind}-${it.id}`}
              data-testid={`conv-bubble-${it.id}`}
              className={cn("flex w-full", right ? "justify-end" : "justify-start")}
              initial={{ opacity: 0, y: 12, scale: 0.96 }}
              animate={{ opacity: 1, y: 0, scale: 1 }}
              exit={{ opacity: 0, scale: 0.95, y: -4 }}
              transition={{ type: "spring", stiffness: 380, damping: 30, delay: Math.min(index * 0.04, 0.3) }}
            >
              <div
                className={cn(
                  "max-w-[80%] rounded-2xl px-3 py-2 text-sm",
                  right
                    ? "bg-primary/12 text-foreground border border-primary/20"
                    : "bg-muted/50 text-foreground border border-border/60",
                )}
              >
                <ReactMarkdown components={mdComponents} remarkPlugins={[remarkGfm]}>
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
            </motion.div>
          );
        })}
      </AnimatePresence>

      {/* Live tool events — rendered as regular bubbles, never removed */}
      <AnimatePresence>
        {liveEvents.map((ev) => {
          const label =
            ev.kind === "tool_use"
              ? `${toolEmoji(ev.toolName)} ${ev.toolName}${ev.toolInput ? `(${ev.toolInput})` : "()"}`
              : `${ev.isError ? "✗" : "✓"} ${ev.toolName}${ev.text ? `: ${ev.text}` : ""}`;
          return (
            <motion.div
              key={ev.id}
              data-testid={`conv-tool-${ev.id}`}
              className="flex w-full justify-start"
              initial={{ opacity: 0, y: 10, scale: 0.97 }}
              animate={{ opacity: 1, y: 0, scale: 1 }}
              exit={{ opacity: 0, scale: 0.95, y: -4 }}
              transition={{ type: "spring", stiffness: 380, damping: 30 }}
            >
              <div
                className={cn(
                  "max-w-[80%] rounded-2xl px-3 py-2 text-sm font-mono",
                  ev.kind === "tool_use"
                    ? "bg-muted/50 text-foreground border border-border/60"
                    : ev.isError
                      ? "bg-destructive/10 text-foreground border border-destructive/30"
                      : "bg-muted/30 text-muted-foreground border border-border/40",
                )}
              >
                {label}
                <time className="mt-1 block text-[10px] text-muted-foreground text-left">
                  {ev.at.toLocaleTimeString()}
                </time>
              </div>
            </motion.div>
          );
        })}
      </AnimatePresence>

      <div ref={bottomRef} />
    </div>
  );
}
