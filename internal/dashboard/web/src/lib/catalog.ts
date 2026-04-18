// Static model + runtime catalog for the G.5 spawn-agent modal.
//
// Kept in TS rather than YAML (plan originally proposed embedded
// YAML in the Go binary) because the Next.js process doesn't see
// the Go runner registry across the daemon boundary. This means we
// keep the list of runtimes / models in sync manually — cheap since
// they change infrequently.
//
// Default per runner is the first entry in its model list.

export type RunnerChoice = { id: string; label: string };
export type ModelChoice = { id: string; label: string };

export const RUNNERS: RunnerChoice[] = [
  { id: "claude", label: "Claude (Anthropic)" },
  { id: "openclaude", label: "Openclaude (z.ai / Minimax)" },
  { id: "opencode", label: "OpenCode" },
];

export const MODELS: Record<string, ModelChoice[]> = {
  claude: [
    { id: "claude-opus-4-7", label: "Opus 4.7 (default)" },
    { id: "claude-sonnet-4-6", label: "Sonnet 4.6" },
  ],
  openclaude: [
    { id: "GLM-5.1", label: "GLM-5.1 (z.ai, default)" },
    { id: "minimax-m1", label: "MiniMax M1" },
  ],
  opencode: [{ id: "claude-sonnet-4-6", label: "Sonnet 4.6 via opencode" }],
};

export function defaultModelFor(runner: string): string | null {
  const list = MODELS[runner];
  return list && list.length > 0 ? list[0].id : null;
}

export function isKnownRunner(runner: string): boolean {
  return RUNNERS.some((r) => r.id === runner);
}

export function isKnownModel(runner: string, model: string): boolean {
  return (MODELS[runner] ?? []).some((m) => m.id === model);
}
