"use client";

import { AnimatePresence, motion } from "framer-motion";
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

  return (
    <AnimatePresence initial={false}>
      {events.length > 0 && (
        <motion.div
          data-testid="live-tool-banner"
          className="flex flex-col gap-1 rounded-lg border border-border bg-muted/50 px-3 py-2 text-xs font-mono text-muted-foreground overflow-hidden"
          initial={{ opacity: 0, height: 0, marginBottom: 0 }}
          animate={{ opacity: 1, height: "auto", marginBottom: 8 }}
          exit={{ opacity: 0, height: 0, marginBottom: 0 }}
          transition={{ type: "spring", stiffness: 400, damping: 32 }}
        >
          <AnimatePresence initial={false}>
            {events.map((ev, i) => (
              <motion.div
                key={ev.id}
                className="flex items-center gap-2"
                initial={{ opacity: 0, x: -8 }}
                animate={{ opacity: 1, x: 0 }}
                exit={{ opacity: 0, x: 8 }}
                transition={{ type: "spring", stiffness: 400, damping: 32, delay: i * 0.03 }}
              >
                <span>{toolEmoji(ev.toolName)}</span>
                <span className="font-medium text-foreground">{ev.toolName}</span>
                {ev.kind === "tool_use" && ev.toolInput && (
                  <span className="opacity-60 truncate max-w-[200px]">{ev.toolInput}</span>
                )}
                {ev.kind === "tool_result" && (
                  <motion.span
                    className={ev.isError ? "text-destructive" : "text-green-600 dark:text-green-400"}
                    initial={{ scale: 0.5, opacity: 0 }}
                    animate={{ scale: 1, opacity: 1 }}
                    transition={{ type: "spring", stiffness: 500, damping: 20 }}
                  >
                    {ev.isError ? "✗" : "✓"}
                  </motion.span>
                )}
              </motion.div>
            ))}
          </AnimatePresence>
        </motion.div>
      )}
    </AnimatePresence>
  );
}
