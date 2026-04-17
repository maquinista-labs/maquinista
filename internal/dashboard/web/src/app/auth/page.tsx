// /auth — login page. Renders a small username/password form that
// posts to /api/auth/login; on success the Route Handler sets a
// session cookie and the page redirects to ?next or /agents.

import { LoginForm } from "@/components/dash/login-form";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export default async function AuthPage({
  searchParams,
}: {
  searchParams: Promise<{ next?: string; error?: string }>;
}) {
  const { next, error } = await searchParams;
  const mode = (process.env.MAQUINISTA_DASHBOARD_AUTH ?? "none").toLowerCase();

  return (
    <main className="flex min-h-screen items-center justify-center bg-background p-4">
      <div
        data-testid="auth-panel"
        className="w-full max-w-sm rounded-2xl border border-border/60 bg-card p-6 shadow-sm"
      >
        <h1 className="mb-1 text-xl font-semibold">maquinista dashboard</h1>
        <p className="mb-4 text-sm text-muted-foreground">
          Sign in to continue. Mode: <code>{mode}</code>
        </p>
        {mode === "telegram" ? (
          <div
            data-testid="auth-telegram-stub"
            className="rounded border border-amber-500/60 bg-amber-500/10 p-3 text-sm"
          >
            Telegram magic-link not wired yet. Set
            <code> MAQUINISTA_DASHBOARD_AUTH=password </code>
            or <code>none</code> until the bot hook lands.
          </div>
        ) : (
          <LoginForm next={next ?? "/agents"} initialError={error ?? null} />
        )}
      </div>
    </main>
  );
}
