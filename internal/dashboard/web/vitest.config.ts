import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

// Vitest configuration. jsdom environment for everything — our
// route-handler tests don't need a real network, and jsdom is a
// few hundred ms slower than node but one less config axis.
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
    setupFiles: ["./tests/vitest.setup.ts"],
    // Keep Playwright specs out — different runner.
    exclude: ["tests/e2e/**", "node_modules/**", ".next/**"],
  },
});
