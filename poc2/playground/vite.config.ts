import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Local dev proxy: point these at the genset/propulsion API IP:port when
// running `npm run dev` outside the cluster (see README.md).
const GENSET_DEV_TARGET = process.env.VITE_GENSET_TARGET ?? "http://localhost:8001";
const PROPULSION_DEV_TARGET = process.env.VITE_PROPULSION_TARGET ?? "http://localhost:8002";
const DATASET_DEV_TARGET = process.env.VITE_DATASET_TARGET ?? "http://localhost:8000";
const DATASET_COLLECTOR_DEV_TARGET = process.env.VITE_DATASET_COLLECTOR_TARGET ?? "http://localhost:8003";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api/dataset/collector": {
        target: DATASET_COLLECTOR_DEV_TARGET,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api\/dataset\/collector/, ""),
      },
      "/api/dataset": {
        target: DATASET_DEV_TARGET,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api\/dataset/, ""),
      },
      // Strips both the "/api/genset" prefix and the "/<id>" instance
      // segment, since local dev only points at a single genset/propulsion
      // instance (see public/config.json for the matching dev id list).
      "/api/genset": {
        target: GENSET_DEV_TARGET,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api\/genset\/[^/]+/, ""),
      },
      "/api/propulsion": {
        target: PROPULSION_DEV_TARGET,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api\/propulsion\/[^/]+/, ""),
      },
    },
  },
});

