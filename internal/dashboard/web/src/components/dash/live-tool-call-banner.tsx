"use client";

import { useAgentLive } from "@/lib/use-agent-live";

// Emoji assigned to common tool names.
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

// LiveToolCallBanner renders recent tool events above the conversation pane.
// Events accumulate until page reload (no TTL).
export function LiveToolCallBanner({ agentId }: { agentId: string }) {
  const events = useAgentLive(agentId);

  if (events.length === 0) return null;

  return (
    <div
      data-testid="live-tool-banner"
      className="flex flex-col gap-1 rounded-lg border border-border bg-muted/50 px-3 py-2 text-xs font-mono text-muted-foreground"
    >
      {events.map((ev) => (
        <div key={ev.id} className="flex items-center gap-2">
          <span>{toolEmoji(ev.toolName)}</span>
          <span className="font-medium text-foreground">{ev.toolName}</span>
          {ev.kind === "tool_use" && ev.toolInput && (
            <span className="opacity-60 truncate max-w-[200px]">{ev.toolInput}</span>
          )}
          {ev.kind === "tool_result" && (
            <span className={ev.isError ? "text-destructive" : "text-green-600 dark:text-green-400"}>
              {ev.isError ? "✗" : "✓"}
            </span>
          )}
        </div>
      ))}
    </div>
  );
}
