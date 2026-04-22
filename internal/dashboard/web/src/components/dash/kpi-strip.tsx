"use client";

import { useQuery } from "@tanstack/react-query";

import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import type { KPIs } from "@/lib/types";

function centsToUsd(cents: number): string {
  const d = cents / 100;
  return d.toFixed(d < 10 ? 2 : 0);
}

export function KpiStrip({ initial }: { initial?: KPIs }) {
  const q = useQuery<KPIs, Error>({
    queryKey: ["kpis"],
    queryFn: async () => {
      const res = await fetch("/api/kpis", { cache: "no-store" });
      if (!res.ok) throw new Error(`GET /api/kpis ${res.status}`);
      return (await res.json()) as KPIs;
    },
    initialData: initial,
    staleTime: 10_000,
  });

  const k = q.data;
  if (!k) return null;

  const tiles: Array<{ label: string; value: string; testid: string }> = [
    {
      label: "Active",
      value: `${k.active_agents}/${k.total_agents}`,
      testid: "kpi-active",
    },
    {
      label: "Messages",
      value: `${k.messages}`,
      testid: "kpi-messages",
    },
    {
      label: "Tokens (in)",
      value: `${(k.tokens_today.input / 1000).toFixed(1)}k`,
      testid: "kpi-tokens-in",
    },
    {
      label: "Tokens (out)",
      value: `${(k.tokens_today.output / 1000).toFixed(1)}k`,
      testid: "kpi-tokens-out",
    },
    {
      label: "Today",
      value: `$${centsToUsd(k.cost_today_cents)}`,
      testid: "kpi-cost-today",
    },
    {
      label: "Month proj.",
      value: `$${centsToUsd(k.cost_month_projected_cents)}`,
      testid: "kpi-cost-month",
    },
  ];

  return (
    <div data-testid="kpi-strip" className="mb-4">
      <div className="grid grid-cols-3 gap-2 sm:grid-cols-6">
        {tiles.map((t) => (
          <Card
            key={t.testid}
            data-testid={t.testid}
            className="p-2 text-center"
          >
            <div className="text-[10px] uppercase tracking-wider text-muted-foreground">
              {t.label}
            </div>
            <div className="text-lg font-semibold tabular-nums">{t.value}</div>
          </Card>
        ))}
      </div>
      {k.cost_by_model.length > 0 && (
        <CostDonut
          slices={k.cost_by_model.map((x) => ({
            label: x.model,
            value: x.cents,
          }))}
        />
      )}
    </div>
  );
}

// Inline-SVG donut. Deliberately tiny (~40 lines) — Recharts would
// be lovely but +30 kB isn't worth it for a four-slice chart.
type DonutSlice = { label: string; value: number };

function CostDonut({ slices }: { slices: DonutSlice[] }) {
  const total = slices.reduce((s, x) => s + x.value, 0);
  if (total === 0) return null;

  const radius = 22;
  const circumference = 2 * Math.PI * radius;
  const palette = [
    "hsl(142 71% 45%)", // emerald
    "hsl(217 91% 60%)", // blue
    "hsl(48 96% 53%)",  // amber
    "hsl(0 84% 60%)",   // rose
    "hsl(262 83% 58%)", // violet
  ];

  let acc = 0;
  return (
    <div
      data-testid="cost-donut"
      className="mt-2 flex items-center gap-3 rounded-lg border border-border/60 bg-card p-2"
    >
      <svg viewBox="0 0 60 60" className="h-14 w-14 shrink-0 -rotate-90">
        <circle cx="30" cy="30" r={radius} fill="transparent" strokeWidth="8" className="stroke-muted" />
        {slices.map((s, i) => {
          const pct = s.value / total;
          const dash = pct * circumference;
          const offset = circumference - acc * circumference;
          acc += pct;
          return (
            <circle
              key={s.label}
              cx="30"
              cy="30"
              r={radius}
              fill="transparent"
              stroke={palette[i % palette.length]}
              strokeWidth="8"
              strokeDasharray={`${dash} ${circumference - dash}`}
              strokeDashoffset={offset}
            />
          );
        })}
      </svg>
      <ul className="grid grid-cols-1 gap-0.5 text-xs">
        {slices.map((s, i) => (
          <li key={s.label} className={cn("flex items-center gap-2")}>
            <span
              className="inline-block h-2 w-2 rounded-sm"
              style={{ background: palette[i % palette.length] }}
            />
            <span className="text-muted-foreground">{s.label}</span>
            <span className="ml-auto font-mono tabular-nums">
              ${centsToUsd(s.value)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
