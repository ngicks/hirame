import preact from "@preact/preset-vite";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

// Kept in sync with server.APIPrefix in apps/search-api and with
// API_BASE_URL in src/api/transport.ts.
const API_PREFIX = "/api";

// In production the GUI and the API share an origin behind one reverse proxy
// (D-010); this proxy reproduces that during development so the client can
// use a relative baseUrl in both.
const DEV_API_TARGET = "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [preact(), tailwindcss()],
  server: {
    proxy: {
      [API_PREFIX]: {
        target: DEV_API_TARGET,
        changeOrigin: false,
      },
    },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    setupFiles: ["./src/test/setup.ts"],
    server: {
      // Ark UI reaches React through Preact's compatibility layer, which the
      // preact plugin wires up as a resolve alias. An externalized dependency
      // is loaded by Node instead of Vite and would miss that alias, ending up
      // with the real React beside Preact's renderer.
      deps: { inline: [/@ark-ui\//, /@zag-js\//] },
    },
  },
});
