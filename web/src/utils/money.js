// money.js — USD formatting for the micro-USD integers used by the billing
// backend (1 USD = 1_000_000 micro-USD). Quota fields use -1 as the "unlimited"
// sentinel.

const MICRO_PER_USD = 1_000_000;

// formatUSD renders a micro-USD amount as a USD string:
//   - null/undefined        → "-"
//   - -1 (unlimited)        → "∞"
//   - 0                     → "$0"
//   - >= $0.0001            → "$X.XXXX" (4 decimals, trailing-zero trimmed to
//                             at least 2 where it reads naturally)
//   - tiny but non-zero     → more decimals so a sub-$0.0001 cost is still shown
export function formatUSD(microUSD) {
  if (microUSD == null) return '-';
  const n = Number(microUSD);
  if (Number.isNaN(n)) return '-';
  if (n === -1) return '∞';
  if (n === 0) return '$0';

  const usd = n / MICRO_PER_USD;
  const abs = Math.abs(usd);
  // Pick decimal places so small values aren't rounded to $0.0000.
  let decimals = 4;
  if (abs < 0.0001) decimals = 8;
  else if (abs < 0.01) decimals = 6;
  return '$' + usd.toFixed(decimals);
}

// usdToMicro converts a USD number (e.g. from a form input like 3.00) to a
// micro-USD integer, rounding to the nearest micro-USD. Empty/invalid → 0.
export function usdToMicro(usd) {
  const n = Number(usd);
  if (!Number.isFinite(n) || n < 0) return 0;
  return Math.round(n * MICRO_PER_USD);
}

// microToUSD converts a micro-USD integer to a plain USD number for form inputs
// (no currency symbol). null/0 → '' so the field renders empty rather than "0".
export function microToUSD(microUSD) {
  if (microUSD == null || Number(microUSD) === 0) return '';
  return Number(microUSD) / MICRO_PER_USD;
}
