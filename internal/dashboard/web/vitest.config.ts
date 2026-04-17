import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

// Vitest configuration. Component tests run in jsdom; plain module
// tests run in node. Path aliases mirror tsconfig so imports match
// the app.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "src"),
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    environmentMatchGlobs: [
      // db.ts + route-handler + SSE tests don't touch the DOM; run
      // them under node for speed and to avoid jsdom's fetch polyfill
      // surprises.
      ["src/lib/**/*.test.ts", "node"],
      ["src/app/api/**/*.test.ts", "node"],
    ],
    setupFiles: ["./tests/vitest.setup.ts"],
    // Keep Playwright specs out — different runner.
    exclude: ["tests/e2e/**", "node_modules/**", ".next/**"],
  },
});
