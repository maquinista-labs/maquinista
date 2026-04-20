"use client";

import { AnimatePresence, motion } from "framer-motion";
import { useAgentLive, formatElapsed } from "@/lib/use-agent-live";

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

export function LiveToolCallBanner({ agentId }: { agentId: string }) {
  const calls = useAgentLive(agentId);

  return (
    <AnimatePresence initial={false}>
      {calls.length > 0 && (
        <motion.div
          data-testid="live-tool-banner"
          className="flex flex-col gap-1 rounded-lg border border-border bg-muted/50 px-3 py-2 text-xs font-mono text-muted-foreground overflow-hidden"
          initial={{ opacity: 0, height: 0, marginBottom: 0 }}
          animate={{ opacity: 1, height: "auto", marginBottom: 8 }}
          exit={{ opacity: 0, height: 0, marginBottom: 0 }}
          transition={{ type: "spring", stiffness: 400, damping: 32 }}
        >
          <AnimatePresence initial={false}>
            {calls.map((call, i) => (
              <motion.div
                key={call.id}
                className="flex items-center gap-2"
                initial={{ opacity: 0, x: -8 }}
                animate={{ opacity: 1, x: 0 }}
                exit={{ opacity: 0, x: 8 }}
                transition={{ type: "spring", stiffness: 400, damping: 32, delay: i * 0.03 }}
              >
                <span>{toolEmoji(call.toolName)}</span>
                <span className="font-medium text-foreground">{call.toolName}</span>
                {call.toolInput && (
                  <span className="opacity-50 truncate max-w-[160px]">({call.toolInput})</span>
                )}
                {call.elapsedMs !== undefined && (
                  <span className="tabular-nums opacity-60">{formatElapsed(call.elapsedMs)}</span>
                )}

                {/* status indicator */}
                <motion.span
                  initial={false}
                  animate={call.status !== "running" ? { scale: 1, opacity: 1 } : { scale: 0, opacity: 0 }}
                  transition={{ type: "spring", stiffness: 500, damping: 20 }}
                >
                  {call.status === "done" && (
                    <span className="text-green-600 dark:text-green-400">✓</span>
                  )}
                  {call.status === "error" && (
                    <span className="text-destructive">✗</span>
                  )}
                </motion.span>

                {call.status === "running" && (
                  <motion.span
                    className="text-muted-foreground"
                    animate={{ opacity: [1, 0.3, 1] }}
                    transition={{ duration: 1.2, repeat: Infinity, ease: "easeInOut" }}
                  >
                    ···
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
