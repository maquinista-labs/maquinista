"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";

import { Button } from "@/components/ui/button";

export function LoginForm({
  next,
  initialError,
}: {
  next: string;
  initialError: string | null;
}) {
  const router = useRouter();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(initialError);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!username || !password || busy) return;
    setBusy(true);
    setError(null);
    try {
      const res = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        setError(body.error ?? `HTTP ${res.status}`);
        return;
      }
      router.replace(next);
      router.refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      data-testid="login-form"
      className="flex flex-col gap-3"
      onSubmit={submit}
    >
      {error && (
        <p
          data-testid="login-error"
          className="rounded border border-destructive/60 bg-destructive/10 p-2 text-sm text-destructive"
        >
          {error}
        </p>
      )}
      <label className="flex flex-col gap-1 text-sm">
        <span className="text-muted-foreground">Username</span>
        <input
          data-testid="login-username"
          autoComplete="username"
          className="rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          disabled={busy}
          required
        />
      </label>
      <label className="flex flex-col gap-1 text-sm">
        <span className="text-muted-foreground">Password</span>
        <input
          data-testid="login-password"
          type="password"
          autoComplete="current-password"
          className="rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          disabled={busy}
          required
        />
      </label>
      <Button
        data-testid="login-submit"
        type="submit"
        disabled={busy || !username || !password}
      >
        {busy ? "Signing in…" : "Sign in"}
      </Button>
    </form>
  );
}
