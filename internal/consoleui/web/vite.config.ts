import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Builds the single-bundle SPA into web/dist, which the Go binary
// embeds via //go:embed. During development, `npm run dev` proxies the
// JSON API + websocket to the running `w17ctl ui` server on :7917.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    rollupOptions: { output: { manualChunks: undefined } },
  },
  server: {
    proxy: {
      "/api": "http://127.0.0.1:7917",
      "/ws": { target: "ws://127.0.0.1:7917", ws: true },
    },
  },
});
