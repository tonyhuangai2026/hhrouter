// i18n switch smoke test (Tech Design §9).
// Imports the REAL src/i18n module (so import.meta.glob resources, the custom
// LanguageDetector + normalize chain, the languageChanged persistence handler,
// and <html lang> sync are all the production code paths) plus the Semi locale
// selection used by LocaleBridge. Built by Vite, executed in Node with a tiny
// DOM/localStorage/navigator shim. Results are written to globalThis.__RESULTS__.
import i18n, { normalizeLang, LANG_STORAGE_KEY, SUPPORTED_LANGS } from '../../src/i18n/index.js';
import zh_CN from '@douyinfe/semi-ui/lib/es/locale/source/zh_CN';
import en_US from '@douyinfe/semi-ui/lib/es/locale/source/en_US';

// Mirror of LocaleBridge's selection logic (it reads i18n.language + normalize).
const SEMI_LOCALES = { zh: zh_CN, en: en_US };
function semiLocaleFor(lang) {
  return SEMI_LOCALES[normalizeLang(lang)] || en_US;
}

const results = [];
function check(name, cond, detail) {
  results.push({ name, pass: !!cond, detail });
}

async function run() {
  await i18n.init; // ensure init settled (i18n is sync here but be safe)

  // 1. Default follows browser language. navigator.language was set to zh-CN by
  //    the harness BEFORE this module loaded, and no arp_lang was stored, so the
  //    detector chain (localStorage -> navigator) should land on 'zh'.
  check(
    'default follows browser navigator.language (zh-CN -> zh)',
    normalizeLang(i18n.language) === 'zh',
    `i18n.language=${i18n.language}`
  );

  // sample keys to prove resources flip
  const titleZh = i18n.t('login:title');
  const semiEmptyZh = zh_CN.Table?.emptyText ?? '(n/a)';

  // 2. Switching via changeLanguage flips resources.
  await i18n.changeLanguage('en');
  const titleEn = i18n.t('login:title');
  check(
    'changeLanguage("en") flips translation resources',
    titleEn && titleZh && titleEn !== titleZh,
    `zh="${titleZh}" en="${titleEn}"`
  );

  // 3. localStorage(arp_lang) persists on change (handled by languageChanged).
  check(
    'localStorage(arp_lang) persists after switch',
    globalThis.localStorage.getItem(LANG_STORAGE_KEY) === 'en',
    `stored=${globalThis.localStorage.getItem(LANG_STORAGE_KEY)}`
  );

  // 4. document.documentElement.lang updates.
  check(
    'document.documentElement.lang updates to en',
    globalThis.document.documentElement.lang === 'en',
    `<html lang>=${globalThis.document.documentElement.lang}`
  );

  // 5. Semi LocaleProvider locale selected from i18n.language (LocaleBridge logic).
  check(
    'Semi locale = en_US when language is en',
    semiLocaleFor(i18n.language) === en_US,
    `selected=${semiLocaleFor(i18n.language) === en_US ? 'en_US' : 'other'}`
  );

  // switch back to zh and re-verify the bridge + persistence + html lang
  await i18n.changeLanguage('zh');
  check(
    'changeLanguage("zh") -> Semi locale zh_CN, lang persisted, <html lang>=zh',
    semiLocaleFor(i18n.language) === zh_CN &&
      globalThis.localStorage.getItem(LANG_STORAGE_KEY) === 'zh' &&
      globalThis.document.documentElement.lang === 'zh',
    `stored=${globalThis.localStorage.getItem(LANG_STORAGE_KEY)} htmlLang=${globalThis.document.documentElement.lang}`
  );

  // 6. normalize edge cases
  check(
    'normalizeLang: zh-TW->zh, en-US->en, fr->en, undefined->en',
    normalizeLang('zh-TW') === 'zh' &&
      normalizeLang('en-US') === 'en' &&
      normalizeLang('fr') === 'en' &&
      normalizeLang(undefined) === 'en',
    `supported=${SUPPORTED_LANGS.join(',')}`
  );

  // 7. Semi built-in text actually differs zh vs en (proves LocaleProvider payload differs)
  const semiEmptyEn = en_US.Table?.emptyText ?? '(n/a)';
  check(
    'Semi built-in text differs zh vs en (Table.emptyText)',
    semiEmptyZh !== semiEmptyEn,
    `zh="${semiEmptyZh}" en="${semiEmptyEn}"`
  );

  globalThis.__RESULTS__ = results;
}

globalThis.__SMOKE_DONE__ = run();
