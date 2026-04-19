"use client";

import { useAgentLive } from "@/lib/use-agent-live";
import { cn } from "@/lib/utils";

// Emoji assigned to common tool names — mirrors the Telegram bot rendering.
const TOOL_EMOJI: Record<string, string> = {
  bash: "🖥",
  computer: "🖥",
  edit_file: "✏️",
  write_file: "✏️",
  read_file: "📄",
  list_files: "📂",
  web_search: "🔍",
  web_fetch: "🌐",
  grep: "🔎",
  glob: "📁",
};

function toolEmoji(name: string): string {
  return TOOL_EMOJI[name.toLowerCase()] ?? "🔮";
}

function formatElapsed(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

// LiveToolCallBanner renders an unobtrusive strip at the top of the
// conversation pane while tool calls are in-flight. Disappears automatically
// when all calls complete and their TTL expires.
export function LiveToolCallBanner({ agentId }: { agentId: string }) {
  const calls = useAgentLive(agentId);

  if (calls.length === 0) return null;

  return (
    <div
      data-testid="live-tool-banner"
      className="flex flex-col gap-1 rounded-lg border border-border bg-muted/50 px-3 py-2 text-xs font-mono text-muted-foreground"
    >
      {calls.map((c) => (
        <div
          key={c.callId}
          className={cn(
            "flex items-center gap-2 transition-opacity duration-500",
            c.status === "done" ? "opacity-40" : "opacity-100",
          )}
        >
          <span>{toolEmoji(c.toolName)}</span>
          <span className="font-medium text-foreground">{c.toolName}</span>
          <span className="text-muted-foreground">
            {formatElapsed(c.elapsedMs)}
          </span>
          {c.status === "done" && (
            <span className="ml-auto text-[10px] text-green-600 dark:text-green-400">
              ✓
            </span>
          )}
        </div>
      ))}
    </div>
  );
}
