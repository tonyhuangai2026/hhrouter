// LocaleBridge — the UNIQUE global Semi LocaleProvider wrap point (Tech Design §4).
//
// Picks Semi's zh_CN / en_US locale based on the active i18next language and
// renders <LocaleProvider locale={...}>{children}</LocaleProvider>. This makes
// Semi's built-in component text (Pagination, DatePicker, Popconfirm, Table empty
// state, etc.) follow the selected language.
//
// This component is mounted exactly once, in src/main.jsx, wrapping <App/>.
// T2+ must NOT add another LocaleProvider.
import React from 'react';
import { LocaleProvider } from '@douyinfe/semi-ui';
import zh_CN from '@douyinfe/semi-ui/lib/es/locale/source/zh_CN';
import en_US from '@douyinfe/semi-ui/lib/es/locale/source/en_US';
import { useTranslation } from 'react-i18next';
import { normalizeLang } from './index';

const SEMI_LOCALES = {
  zh: zh_CN,
  en: en_US,
};

export default function LocaleBridge({ children }) {
  const { i18n } = useTranslation();
  const lang = normalizeLang(i18n.language);
  const locale = SEMI_LOCALES[lang] || en_US;

  return <LocaleProvider locale={locale}>{children}</LocaleProvider>;
}
