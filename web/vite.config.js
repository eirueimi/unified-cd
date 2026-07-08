import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  base: '/ui/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    host: true,
    port: 5173,
    proxy: {
      // changeOrigin must stay off (default false) for both entries below:
      // the controller's originCheckMiddleware (internal/controller/hardening.go)
      // compares the browser's Origin header host against the request's Host
      // header. String-shorthand proxy entries (e.g. '/api': 'http://...') are
      // expanded by Vite to `{ target, changeOrigin: true }`, which rewrites
      // Host to the target (localhost:8080) while Origin still says
      // localhost:5173 — a mismatch that gets every POST/PUT/DELETE 403'd.
      // Object form without changeOrigin preserves the browser's Host end to
      // end so it matches Origin.
      '/api': { target: process.env.VITE_API_URL || 'http://localhost:8080' },
      '/webhook': { target: process.env.VITE_API_URL || 'http://localhost:8080' },
    },
    allowedHosts: [
      "vite"
    ],
    // inotify does not fire for Windows host filesystem changes inside Docker Desktop.
    // usePolling ensures HMR works when running in a Linux container on Windows.
    watch: {
      usePolling: true,
      interval: 500,
    },
  },
});
