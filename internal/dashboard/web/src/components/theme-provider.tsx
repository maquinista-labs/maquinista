"use client";

// Thin wrapper around next-themes so the rest of the app imports a
// stable @/components/theme-provider path and the "use client"
// boundary is co-located with the provider.

import { ThemeProvider as NextThemesProvider } from "next-themes";
import type { ComponentProps } from "react";

type Props = ComponentProps<typeof NextThemesProvider>;

export function ThemeProvider({ children, ...props }: Props) {
  return <NextThemesProvider {...props}>{children}</NextThemesProvider>;
}
