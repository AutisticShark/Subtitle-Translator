import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [
    svelte(),
    {
      name: 'go-embed-directory',
      generateBundle() {
        this.emitFile({ type: 'asset', fileName: '.gitkeep', source: '' });
      },
    },
  ],
  build: { target: 'es2023', sourcemap: false },
  server: {
    host: '127.0.0.1',
    proxy: { '/api': 'http://127.0.0.1:8000', '/healthz': 'http://127.0.0.1:8000' },
  },
});
