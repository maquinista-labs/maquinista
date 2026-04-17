"use client";

import { Laptop, Moon, Sun } from "lucide-react";
import { useTheme } from "next-themes";
import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";

// ThemeToggle cycles system → light → dark → system on click. A
// cycle button beats a dropdown on touch: one tap per state vs
// tap-trigger + tap-menuitem, and no portal-escape weirdness with
// the dropdown library. The current mode is shown via the rendered
// icon, which is discoverable enough for three states.
const order = ["system", "light", "dark"] as const;
type Mode = (typeof order)[number];

function nextMode(current: string | undefined): Mode {
  const idx = order.indexOf((current ?? "system") as Mode);
  return order[(idx + 1) % order.length];
}

export function ThemeToggle() {
  const { theme, setTheme } = useTheme();
  const [mounted, setMounted] = useState(false);

  // next-themes reads the theme from localStorage on mount; until
  // then `theme` is undefined. Rendering a deterministic label on
  // the server avoids hydration mismatches.
  useEffect(() => setMounted(true), []);

  const effective: Mode = mounted ? ((theme ?? "system") as Mode) : "system";
  const label = `Toggle theme (current: ${effective})`;
  const next = nextMode(effective);

  const Icon =
    effective === "dark" ? Moon : effective === "light" ? Sun : Laptop;

  return (
    <Button
      variant="ghost"
      size="icon"
      className="h-9 w-9"
      aria-label={label}
      data-testid="theme-toggle"
      data-theme-mode={effective}
      data-theme-next={next}
      onClick={() => setTheme(next)}
    >
      <Icon className="h-5 w-5" />
    </Button>
  );
}
