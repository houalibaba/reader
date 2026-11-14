import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';

export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: '../server/internal/web/dist',
    emptyOutDir: true,
    target: 'es2020'
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8380',
      '/media': 'http://127.0.0.1:8380'
    }
  }
});
