import { ThemeToggle } from "@/components/dash/theme-toggle";

// Sticky header: app title on the left, theme toggle on the right.
// The `sticky top-0` + `backdrop-blur` gives iOS-style glass during
// scroll. Safe-area padding keeps the title below the notch on
// notched phones.
export function DashHeader({ title }: { title: string }) {
  return (
    <header
      data-testid="dash-header"
      className="sticky top-0 z-20 flex h-14 items-center justify-between border-b border-border/60 bg-background/80 px-4 backdrop-blur supports-[backdrop-filter]:bg-background/60"
      style={{ paddingTop: "env(safe-area-inset-top)" }}
    >
      <h1 className="text-base font-semibold tracking-tight">{title}</h1>
      <ThemeToggle />
    </header>
  );
}
