import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Local dev proxy: point these at the genset/propulsion API IP:port when
// running `npm run dev` outside the cluster (see README.md).
const GENSET_DEV_TARGET = process.env.VITE_GENSET_TARGET ?? "http://localhost:8001";
const PROPULSION_DEV_TARGET = process.env.VITE_PROPULSION_TARGET ?? "http://localhost:8002";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api/genset": {
        target: GENSET_DEV_TARGET,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api\/genset/, ""),
      },
      "/api/propulsion": {
        target: PROPULSION_DEV_TARGET,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api\/propulsion/, ""),
      },
    },
  },
});
