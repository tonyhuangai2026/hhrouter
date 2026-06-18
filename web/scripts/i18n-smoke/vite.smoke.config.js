// Builds scripts/i18n-smoke/entry.js into a single ESM bundle that resolves
// import.meta.glob (so the REAL i18n resources load) and the Semi locale imports.
import { defineConfig } from 'vite';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const __dirname = dirname(fileURLToPath(import.meta.url));
const root = resolve(__dirname, '..', '..');

export default defineConfig({
  root,
  define: { 'import.meta.env.DEV': 'false' },
  build: {
    target: 'node18',
    minify: false,
    write: true,
    outDir: resolve(__dirname, 'dist'),
    emptyOutDir: true,
    lib: {
      entry: resolve(__dirname, 'entry.js'),
      formats: ['es'],
      fileName: () => 'smoke.mjs',
    },
    rollupOptions: {
      // Bundle everything so Node can run the single file without node_modules ESM quirks.
      external: [],
    },
  },
});
