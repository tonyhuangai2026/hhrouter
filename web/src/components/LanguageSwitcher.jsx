import React from 'react';
import { Select } from '@douyinfe/semi-ui';
import { IconLanguage } from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
import { SUPPORTED_LANGS, normalizeLang } from '../i18n';

// Global language switcher. Reflects the active i18n language and calls
// i18n.changeLanguage on selection; the change propagates app-wide (LocaleBridge
// re-renders Semi locale, <html lang> + persistence handled in i18n/index.js).
export default function LanguageSwitcher({ style }) {
  const { t, i18n } = useTranslation('layout');
  const current = normalizeLang(i18n.language);

  const options = SUPPORTED_LANGS.map((lng) => ({
    value: lng,
    label: t(`language.${lng}`),
  }));

  return (
    <Select
      aria-label={t('language.label')}
      prefix={<IconLanguage />}
      value={current}
      optionList={options}
      onChange={(value) => i18n.changeLanguage(value)}
      style={{ width: 120, ...style }}
    />
  );
}
