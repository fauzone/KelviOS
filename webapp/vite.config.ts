import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// KelviOS web UI. In production the Go core serves dist/ as embedded assets;
// during development Vite serves it standalone on port 5173.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
  },
  preview: {
    port: 4173,
  },
});
