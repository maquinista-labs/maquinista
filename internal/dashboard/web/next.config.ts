import type { NextConfig } from "next";

/**
 * Next.js configuration for the maquinista dashboard.
 *
 * `output: 'standalone'` is the load-bearing setting for this
 * project: Next emits a self-contained server bundle at
 * `.next/standalone/` that the Go CLI embeds via `//go:embed`
 * (Phase 1 Commit 1.5) and extracts at `maquinista dashboard
 * start` (Commit 1.6). Launch with `node .next/standalone/server.js`.
 *
 * Per Next 16 docs, the standalone output does not copy `public/`
 * or `.next/static/`; the `dashboard-web-package` Make target runs
 * the copy as part of tarball packaging.
 */
const nextConfig: NextConfig = {
  output: "standalone",
};

export default nextConfig;
