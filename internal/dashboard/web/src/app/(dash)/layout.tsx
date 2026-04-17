import { BottomNav } from "@/components/dash/bottom-nav";
import { DashHeader } from "@/components/dash/header";

// The (dash) route group wraps every operator-facing page with the
// sticky header + bottom nav. Non-dashboard surfaces (e.g. Phase 6
// /auth) live outside this group so they get a clean chromeless
// layout.

// Per-route title resolution is via a simple prop; Phase 3 can
// replace this with dynamic titles once agent detail lands.
export default function DashLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-full flex-1 flex-col">
      <DashHeader title="maquinista" />
      <main
        data-testid="dash-main"
        className="flex-1 overflow-y-auto overscroll-contain"
      >
        {children}
      </main>
      <BottomNav />
    </div>
  );
}
