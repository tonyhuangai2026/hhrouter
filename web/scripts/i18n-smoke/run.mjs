// Headless runner for the i18n switch smoke test.
// 1. Installs a minimal DOM/localStorage/navigator shim (navigator.language=zh-CN,
//    NO arp_lang stored) BEFORE importing the built bundle, so the production
//    detector chain runs against a realistic "fresh zh browser" environment.
// 2. Builds the bundle via Vite (resolves import.meta.glob + Semi locales).
// 3. Imports the bundle, awaits its run(), prints PASS/FAIL per assertion.
import { execFileSync } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { existsSync } from 'node:fs';

const __dirname = dirname(fileURLToPath(import.meta.url));
const webRoot = resolve(__dirname, '..', '..');

// --- DOM / browser shim (set BEFORE importing the bundle) ---
const store = new Map();
globalThis.localStorage = {
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => store.set(k, String(v)),
  removeItem: (k) => store.delete(k),
  clear: () => store.clear(),
};
const htmlEl = { lang: '' };
globalThis.document = { documentElement: htmlEl };
// Fresh "Chinese browser": navigator says zh-CN, no stored preference.
// Node 24 exposes a read-only `navigator`, so override via defineProperty.
Object.defineProperty(globalThis, 'navigator', {
  value: { language: 'zh-CN', languages: ['zh-CN', 'zh'] },
  configurable: true,
  writable: true,
});
globalThis.window = globalThis;

// --- Build the smoke bundle with Vite ---
const cfg = resolve(__dirname, 'vite.smoke.config.js');
console.log('Building i18n smoke bundle via Vite...');
execFileSync(
  process.execPath,
  [resolve(webRoot, 'node_modules/vite/bin/vite.js'), 'build', '--config', cfg, '--logLevel', 'warn'],
  { cwd: webRoot, stdio: 'inherit' }
);

const bundle = resolve(__dirname, 'dist', 'smoke.mjs');
if (!existsSync(bundle)) {
  console.error('FAILED: smoke bundle not produced at', bundle);
  process.exit(1);
}

// --- Execute the production i18n code paths ---
const mod = await import(pathToFileURL(bundle).href);
await globalThis.__SMOKE_DONE__;
const results = globalThis.__RESULTS__ || [];

console.log('\n=== i18n switch smoke results ===');
let failed = 0;
for (const r of results) {
  console.log(`  ${r.pass ? 'PASS' : 'FAIL'}  ${r.name}${r.detail ? `  [${r.detail}]` : ''}`);
  if (!r.pass) failed++;
}
console.log('');
if (results.length === 0) {
  console.error('SMOKE FAILED: no assertions ran.');
  process.exit(1);
}
if (failed > 0) {
  console.error(`SMOKE FAILED: ${failed}/${results.length} assertions failed.`);
  process.exit(1);
}
console.log(`SMOKE OK: ${results.length}/${results.length} assertions passed.`);
