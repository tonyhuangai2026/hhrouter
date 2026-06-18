// Shared helpers for resource API modules (T10).
//
// The backend envelope for list endpoints is not pinned down precisely in the
// Tech Design §8 (it only names the endpoints). To be resilient to common
// shapes, we normalize list responses into { items, total } here. Supported
// inbound shapes:
//   - [ ...items ]                                  (bare array)
//   - { data: [ ...items ], total }                 (data envelope)
//   - { items: [ ...items ], total }                (items envelope)
//   - { list: [ ...items ], total }                 (list envelope)
//   - { data: { items: [...], total } }             (nested)
//   - { records / rows: [...], total / count }      (misc)

export function normalizeList(payload) {
  if (Array.isArray(payload)) {
    return { items: payload, total: payload.length };
  }
  if (!payload || typeof payload !== 'object') {
    return { items: [], total: 0 };
  }

  // Unwrap a single { data: {...} } nesting layer if present.
  const root = payload.data && !Array.isArray(payload.data) ? payload.data : payload;

  const items =
    (Array.isArray(payload.data) && payload.data) ||
    root.items ||
    root.list ||
    root.records ||
    root.rows ||
    root.data ||
    [];

  const total =
    root.total ??
    root.count ??
    root.totalCount ??
    payload.total ??
    payload.count ??
    (Array.isArray(items) ? items.length : 0);

  return { items: Array.isArray(items) ? items : [], total: Number(total) || 0 };
}

// Unwrap a single object response that may be wrapped in { data: {...} }.
export function unwrap(payload) {
  if (payload && typeof payload === 'object' && payload.data && !Array.isArray(payload.data)) {
    return payload.data;
  }
  return payload;
}

import i18n from '../i18n';

// Extract a human-readable error message from an axios error.
export function errMessage(error, fallback = 'Request failed') {
  const resp = error?.response?.data;
  if (resp) {
    if (typeof resp === 'string') return resp;
    // OpenAI-style { error: { message } } / Anthropic { error: { message } }
    if (resp.error && (resp.error.message || typeof resp.error === 'string')) {
      return resp.error.message || resp.error;
    }
    if (resp.message) return resp.message;
    if (resp.msg) return resp.msg;
  }
  return error?.message || fallback;
}

// Substring patterns (lowercased) on common backend english error messages,
// mapped to keys in the `errors` namespace. First match wins, so order matters
// (more specific phrases before generic ones).
const MESSAGE_PATTERNS = [
  ['invalid credentials', 'invalidCredentials'],
  ['incorrect password', 'invalidCredentials'],
  ['wrong password', 'invalidCredentials'],
  ['invalid username or password', 'invalidCredentials'],
  ['invalid password', 'invalidCredentials'],
  ['authentication failed', 'invalidCredentials'],
  ['session expired', 'sessionExpired'],
  ['token expired', 'sessionExpired'],
  ['token has expired', 'sessionExpired'],
  ['unauthorized', 'unauthorized'],
  ['not authenticated', 'unauthorized'],
  ['invalid token', 'unauthorized'],
  ['forbidden', 'forbidden'],
  ['permission denied', 'forbidden'],
  ['access denied', 'forbidden'],
  ['not allowed', 'forbidden'],
  ['not found', 'notFound'],
  ['does not exist', 'notFound'],
  ['quota exceeded', 'quotaExceeded'],
  ['insufficient quota', 'quotaExceeded'],
  ['out of quota', 'quotaExceeded'],
  ['rate limit', 'rateLimited'],
  ['too many requests', 'rateLimited'],
  ['timeout', 'timeout'],
  ['timed out', 'timeout'],
  ['network error', 'network'],
  ['conflict', 'conflict'],
  ['already exists', 'conflict'],
  ['validation', 'validation'],
  ['invalid request', 'badRequest'],
  ['bad request', 'badRequest'],
  ['service unavailable', 'serviceUnavailable'],
  ['internal server error', 'serverError'],
  ['server error', 'serverError'],
];

// HTTP status -> errors namespace key (fallback when message text doesn't match).
const STATUS_KEYS = {
  400: 'badRequest',
  401: 'unauthorized',
  403: 'forbidden',
  404: 'notFound',
  409: 'conflict',
  422: 'validation',
  429: 'rateLimited',
  500: 'serverError',
  502: 'serverError',
  503: 'serviceUnavailable',
  504: 'timeout',
};

// Localize a backend/axios error for display. Strategy:
//   1. Match the raw message text against known english patterns -> errors key.
//   2. Else map by HTTP status code -> errors key.
//   3. Else, if it's an axios network/timeout error with no response, use those.
//   4. On miss, fall through to the raw message extracted by errMessage().
// Always returns a string. Never throws.
export function mapApiError(error) {
  const t = (key) => i18n.t(`errors:${key}`);

  // Network / no-response axios errors (server unreachable, CORS, DNS, abort).
  if (error && !error.response) {
    const code = error.code;
    if (code === 'ECONNABORTED' || /timeout/i.test(error.message || '')) {
      return t('timeout');
    }
    if (error.request || code === 'ERR_NETWORK' || /network/i.test(error.message || '')) {
      return t('network');
    }
  }

  // 1. Message-text pattern match.
  const raw = errMessage(error, '');
  const lower = String(raw || '').toLowerCase();
  if (lower) {
    for (const [needle, key] of MESSAGE_PATTERNS) {
      if (lower.includes(needle)) {
        return t(key);
      }
    }
  }

  // 2. HTTP status fallback.
  const status = error?.response?.status;
  if (status && STATUS_KEYS[status]) {
    return t(STATUS_KEYS[status]);
  }

  // 3. Pass through the raw extracted message, or a generic fallback.
  return raw || t('generic');
}
