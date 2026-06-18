# Frontend i18n (Chinese / English)

This app uses [i18next](https://www.i18next.com/) + [react-i18next](https://react.i18next.com/)
with browser language detection, plus Semi UI's `LocaleProvider` so that built-in
component text (Pagination, DatePicker, Popconfirm, Table empty state, etc.) follows
the selected language.

Supported languages: `zh` (Chinese) and `en` (English).

## Files (owned by T1)

```
src/i18n/
  index.js          # i18next init: glob-loads locales, detection, normalize, persist
  LocaleBridge.jsx  # the UNIQUE global Semi <LocaleProvider> wrap point
  README.md         # this file
src/locales/
  zh/<namespace>.json
  en/<namespace>.json
```

## Wrap point (IMPORTANT)

The global Semi `<LocaleProvider>` is mounted in **exactly one place**:
`src/main.jsx`, which does `import './i18n'` (before render) and wraps `<App/>`
in `<LocaleBridge>`.

- `src/main.jsx` and `src/i18n/*` are owned exclusively by T1.
- Do **NOT** add another `LocaleProvider` anywhere (e.g. in `AppLayout`), and do
  **NOT** modify `main.jsx` / `App.jsx` to change the wrap. Page/component tasks
  only consume `useTranslation`.

## Namespaces

One namespace per feature area, so multiple tasks can edit different files without
conflict:

| namespace  | scope                                          |
|------------|------------------------------------------------|
| `common`   | shared UI text: save/cancel/delete/status/etc. |
| `layout`   | AppLayout nav, user menu, theme/lang switcher  |
| `errors`   | API error mapping                              |
| `login`    | Login page                                     |
| `register` | Register page                                  |
| `setup`    | Setup page                                     |
| `channels` | Channels page                                  |
| `rules`    | Routing Rules page                             |
| `tokens`   | Tokens (API Key) page                          |
| `users`    | Users page                                     |
| `dashboard`| Dashboard (analytics) page                     |
| `profile`  | Profile page (incl. language dropdown)         |

`defaultNS` is `common`. New JSON files under `src/locales/**/*.json` are picked up
automatically via `import.meta.glob` — **zero config** needed to add a namespace.

## Key naming convention

- Keys are grouped/nested by purpose, e.g. `actions.save`, `status.enabled`,
  `messages.confirmDelete`.
- The **zh and en file for a namespace must have identical key sets.**
- Reference a key as `t('key')` within its namespace, or `t('ns:key')` across
  namespaces (e.g. `t('common:actions.save')`).

## How to add a string

1. Add the key to **both** `src/locales/zh/<ns>.json` and `src/locales/en/<ns>.json`
   (same key path in each).
2. In the component: `const { t } = useTranslation('<ns>')` (or
   `useTranslation(['<ns>', 'common'])`), then use `t('your.key')`.

## How to switch language

```js
import { useTranslation } from 'react-i18next';
const { i18n } = useTranslation();
i18n.changeLanguage('zh'); // or 'en'
```

On change, the language is persisted to `localStorage['arp_lang']` and
`<html lang>` is updated automatically (see `index.js` `languageChanged` handler).
`LocaleBridge` re-renders to swap the Semi locale.

## Detection

Order: `localStorage('arp_lang')` → `navigator.language` → fallback `en`.
Any `zh-*` tag normalizes to `zh`; everything else to `en`.
