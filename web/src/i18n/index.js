// i18n foundation (Tech Design §2-4). Owned exclusively by T1.
//
// - Auto-collects every namespace JSON under src/locales/**/*.json via Vite's
//   import.meta.glob (eager) so adding a new namespace file requires ZERO config.
// - Language detection order: localStorage('arp_lang') -> navigator -> 'en'.
// - Normalizes any zh-* tag to 'zh'; everything else to 'en'.
// - fallbackLng: 'en', defaultNS: 'common'.
// - On 'languageChanged': persist to localStorage('arp_lang') and set <html lang>.
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import LanguageDetector from 'i18next-browser-languagedetector';

export const SUPPORTED_LANGS = ['zh', 'en'];
export const DEFAULT_LANG = 'en';
export const LANG_STORAGE_KEY = 'arp_lang';

// Collect all locale JSON files at build time. Paths look like
// '../locales/zh/common.json' -> resources['zh']['common'].
const modules = import.meta.glob('../locales/**/*.json', { eager: true });

const resources = {};
for (const filePath in modules) {
  // filePath e.g. '../locales/zh/common.json'
  const match = filePath.match(/\/locales\/([^/]+)\/([^/]+)\.json$/);
  if (!match) continue;
  const [, lng, ns] = match;
  const mod = modules[filePath];
  resources[lng] = resources[lng] || {};
  resources[lng][ns] = mod.default || mod;
}

// Derive namespace list from collected resources (union across languages).
const namespaces = Array.from(
  new Set(
    Object.values(resources).flatMap((nsMap) => Object.keys(nsMap))
  )
);

// Normalize an arbitrary BCP-47-ish tag to one of our supported short codes.
export function normalizeLang(raw) {
  if (!raw) return DEFAULT_LANG;
  const lower = String(raw).toLowerCase();
  if (lower === 'zh' || lower.startsWith('zh-') || lower.startsWith('zh_')) {
    return 'zh';
  }
  return 'en';
}

// Custom detector that applies our normalization on top of the standard
// localStorage -> navigator chain, guaranteeing the active lng is always
// one of SUPPORTED_LANGS.
const languageDetector = new LanguageDetector(null, {
  order: ['localStorage', 'navigator'],
  lookupLocalStorage: LANG_STORAGE_KEY,
  caches: [], // we persist ourselves in the languageChanged handler
});
const originalDetect = languageDetector.detect.bind(languageDetector);
languageDetector.detect = (...args) => {
  const detected = originalDetect(...args);
  const value = Array.isArray(detected) ? detected[0] : detected;
  return normalizeLang(value);
};

i18n
  .use(languageDetector)
  .use(initReactI18next)
  .init({
    resources,
    ns: namespaces,
    defaultNS: 'common',
    fallbackLng: DEFAULT_LANG,
    supportedLngs: SUPPORTED_LANGS,
    nonExplicitSupportedLngs: false,
    load: 'languageOnly',
    interpolation: { escapeValue: false }, // React already escapes
    saveMissing: false,
    debug: import.meta.env?.DEV ?? false,
  });

// Persist + reflect language on the <html> element whenever it changes.
i18n.on('languageChanged', (lng) => {
  const normalized = normalizeLang(lng);
  try {
    localStorage.setItem(LANG_STORAGE_KEY, normalized);
  } catch {
    // ignore storage failures (private mode, etc.)
  }
  if (typeof document !== 'undefined' && document.documentElement) {
    document.documentElement.lang = normalized;
  }
});

// Reflect the initial language on first load (languageChanged may not fire for
// the initially-detected language).
if (typeof document !== 'undefined' && document.documentElement) {
  document.documentElement.lang = normalizeLang(i18n.language);
}

export default i18n;
