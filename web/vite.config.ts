import { execFileSync } from "node:child_process";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

// Settings → About shows the commit the bundle was built from; the binary embeds the same bundle.
function version(): string {
  try {
    return execFileSync("git", ["describe", "--always", "--dirty"], { encoding: "utf8" }).trim();
  } catch {
    return "unknown";
  }
}

export default defineConfig({
  plugins: [svelte()],
  define: {
    __APP_VERSION__: JSON.stringify(version()),
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // The CSP is default-src 'self', so no asset may become a data: URI.
    assetsInlineLimit: 0,
    rolldownOptions: {
      input: { index: "index.html", sw: "src/sw.ts" },
      output: {
        // The service worker keeps one URL, /sw.js, so the browser can check it for updates.
        // It shares no module with the app, so it is a single file with no imports.
        entryFileNames: (chunk) => (chunk.name === "sw" ? "sw.js" : "assets/[name]-[hash].js"),
      },
    },
  },
  server: {
    proxy: {
      // The browser's Origin passes through unchanged; `just dev` allows
      // vite's own origin with --dev-origin, so other pages stay refused.
      "/ws": {
        target: "ws://127.0.0.1:8390",
        ws: true,
      },
    },
  },
});
