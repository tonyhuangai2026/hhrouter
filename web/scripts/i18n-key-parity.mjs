// i18n key parity check (Tech Design §9).
// For every namespace, assert the zh and en JSON have identical, recursively
// flattened key sets. Prints any diffs. Exit code 1 on any mismatch.
import { readdirSync, readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const localesDir = join(__dirname, '..', 'src', 'locales');
const langs = ['zh', 'en'];

// Recursively flatten an object into dotted key paths (leaves only).
function flatten(obj, prefix = '', out = new Set()) {
  for (const [k, v] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (v && typeof v === 'object' && !Array.isArray(v)) {
      flatten(v, path, out);
    } else {
      out.add(path);
    }
  }
  return out;
}

function loadNs(lang, ns) {
  const file = join(localesDir, lang, `${ns}.json`);
  return JSON.parse(readFileSync(file, 'utf8'));
}

// Namespaces = union of *.json file basenames present under each lang dir.
const nsSet = new Set();
for (const lang of langs) {
  for (const f of readdirSync(join(localesDir, lang))) {
    if (f.endsWith('.json')) nsSet.add(f.replace(/\.json$/, ''));
  }
}
const namespaces = [...nsSet].sort();

let failures = 0;
console.log(`Checking key parity for ${namespaces.length} namespaces: ${namespaces.join(', ')}\n`);

for (const ns of namespaces) {
  const keys = {};
  let missingFile = false;
  for (const lang of langs) {
    try {
      keys[lang] = flatten(loadNs(lang, ns));
    } catch (e) {
      console.error(`  [${ns}] MISSING/INVALID ${lang}/${ns}.json: ${e.message}`);
      missingFile = true;
    }
  }
  if (missingFile) { failures++; continue; }

  const onlyZh = [...keys.zh].filter((k) => !keys.en.has(k)).sort();
  const onlyEn = [...keys.en].filter((k) => !keys.zh.has(k)).sort();

  if (onlyZh.length === 0 && onlyEn.length === 0) {
    console.log(`  OK   ${ns.padEnd(10)} ${keys.zh.size} keys`);
  } else {
    failures++;
    console.log(`  FAIL ${ns.padEnd(10)} zh=${keys.zh.size} en=${keys.en.size}`);
    if (onlyZh.length) console.log(`         only in zh: ${onlyZh.join(', ')}`);
    if (onlyEn.length) console.log(`         only in en: ${onlyEn.join(', ')}`);
  }
}

console.log('');
if (failures > 0) {
  console.error(`KEY PARITY FAILED: ${failures} namespace(s) mismatched.`);
  process.exit(1);
} else {
  console.log(`KEY PARITY OK: all ${namespaces.length} namespaces have identical zh/en key sets.`);
}
