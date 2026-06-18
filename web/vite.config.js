import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Resolve the aggregated Semi Design stylesheet by file path. The package's
// strict `exports` map does not expose `dist/css/semi.min.css` (nor
// package.json) as a bare specifier, so we point straight at the file on disk
// under node_modules and alias it.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const semiCssPath = path.resolve(
  __dirname,
  'node_modules/@douyinfe/semi-ui/dist/css/semi.min.css'
);

// Dev proxy forwards /api and /v1 to the backend (Tech Design §2, §9).
// Backend target is http://localhost:3000 by default, overridable via
// VITE_BACKEND_URL (e.g. when running the backend on another host/port).
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const backendTarget = env.VITE_BACKEND_URL || 'http://localhost:3000';

  return {
    plugins: [react()],
    resolve: {
      alias: {
        'semi-ui-css': semiCssPath,
      },
    },
    server: {
      port: 5173,
      proxy: {
        '/api': {
          target: backendTarget,
          changeOrigin: true,
        },
        '/v1': {
          target: backendTarget,
          changeOrigin: true,
        },
      },
    },
    build: {
      outDir: 'dist',
      sourcemap: false,
    },
  };
});
