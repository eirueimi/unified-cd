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
      '/api': process.env.VITE_API_URL || 'http://localhost:8080',
      '/webhook': process.env.VITE_API_URL || 'http://localhost:8080',
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
