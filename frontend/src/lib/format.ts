/**
 * Display formatting.
 *
 * The rule this file exists to enforce: paise become rupees in exactly one place.
 * Every amount that crosses the API is an integer number of paise (domain.Money),
 * and the moment two components each divide by 100 in their own way is the moment
 * the dashboard shows one total and the case list shows another (SRS 9.3, NFR-014).
 *
 * Grouping is Indian — 12,34,567.00, not 1,234,567.00 — matching formatRupees in
 * internal/httpapi/explain.go. A merchant reads the wrong grouping as a wrong
 * number, so this is a correctness concern rather than a locale preference.
 */

import type { Money } from './types';

const RUPEE_EXACT = new Intl.NumberFormat('en-IN', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

const RUPEE_WHOLE = new Intl.NumberFormat('en-IN', {
  minimumFractionDigits: 0,
  maximumFractionDigits: 0,
});

/** "12,34,567.00" — the exact amount, no symbol. Mirrors the Go formatter. */
export function formatRupees(paise: Money): string {
  return RUPEE_EXACT.format(paise / 100);
}

/** "₹12,34,567.00" — the canonical amount for tables and detail views. */
export function formatMoney(paise: Money): string {
  return `₹${formatRupees(paise)}`;
}

/**
 * "₹12,34,567" for a round amount, "₹12,34,567.50" when there are paise.
 *
 * For KPI headlines, where two trailing zeros on every card is noise — but never at
 * the cost of hiding a real fraction, which would make a total look tidier than it
 * is.
 */
export function formatMoneyKPI(paise: Money): string {
  if (paise % 100 === 0) return `₹${RUPEE_WHOLE.format(paise / 100)}`;
  return formatMoney(paise);
}

/** A 0..1 ratio as a percentage. One decimal: the samples are hundreds of cases, not millions. */
export function formatPercent(ratio: number, digits = 1): string {
  if (!Number.isFinite(ratio)) return 'Not available';
  return `${(ratio * 100).toFixed(digits)}%`;
}

/**
 * An already-percentage value with its sign kept visible.
 *
 * The sign matters: AC-008 asks for an honest benchmark, which means a negative
 * uplift has to be able to appear on screen rather than being formatted away.
 */
export function formatSignedPercent(pct: number, digits = 1): string {
  if (!Number.isFinite(pct)) return 'Not available';
  const s = pct.toFixed(digits);
  return `${pct > 0 ? '+' : ''}${s}%`;
}

/** A count, grouped. */
export function formatCount(n: number): string {
  return RUPEE_WHOLE.format(n);
}

/** A latency in milliseconds, scaled to whatever unit reads honestly. */
export function formatLatency(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return 'Not available';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  return `${(ms / 60_000).toFixed(1)} min`;
}

/** Minutes, scaled. Used for time-to-recovery and approval wait times. */
export function formatMinutes(minutes: number): string {
  if (!Number.isFinite(minutes) || minutes <= 0) return 'Not available';
  if (minutes < 60) return `${Math.round(minutes)} min`;
  const hours = minutes / 60;
  if (hours < 48) return `${hours.toFixed(1)} h`;
  return `${(hours / 24).toFixed(1)} d`;
}

/**
 * Absolute timestamp, in the reader's own timezone.
 *
 * Only ever called from a client component after mount. Formatting a date during
 * server render would produce the server's timezone, which then differs from the
 * browser's on hydration — and React resolves that by silently keeping one of them.
 */
export function formatDateTime(ts: string | undefined | null): string {
  if (!ts) return 'Not available';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return 'Not available';
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

/** Date only, for due dates and billing periods. */
export function formatDate(ts: string | undefined | null): string {
  if (!ts) return 'Not available';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return 'Not available';
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: '2-digit' });
}

/**
 * "4m ago" / "in 3d".
 *
 * Relative time is what an operator scanning a queue actually reads, but it hides
 * the absolute time — so every caller pairs it with a title attribute carrying
 * formatDateTime. A feed that only says "2h ago" cannot be cross-referenced against
 * a log.
 */
export function formatRelative(ts: string | undefined | null, now = Date.now()): string {
  if (!ts) return 'Not available';
  const then = new Date(ts).getTime();
  if (Number.isNaN(then)) return 'Not available';
  const deltaSec = Math.round((now - then) / 1000);
  const ago = deltaSec >= 0;
  const s = Math.abs(deltaSec);

  const unit =
    s < 45
      ? `${s}s`
      : s < 3600
        ? `${Math.round(s / 60)}m`
        : s < 86_400
          ? `${Math.round(s / 3600)}h`
          : s < 2_592_000
            ? `${Math.round(s / 86_400)}d`
            : `${Math.round(s / 2_592_000)}mo`;
  return ago ? `${unit} ago` : `in ${unit}`;
}

/**
 * Turns a snake_case enum value into a readable label: `payment_failure` becomes
 * "Payment failure".
 *
 * Deliberately mechanical rather than a lookup table. A lookup table would render a
 * blank for any value added to the Go enum before this file caught up, and a blank
 * cell in a case queue is indistinguishable from missing data. Showing
 * "Some New Status" is worse-looking and more honest.
 */
export function humanize(value: string | undefined | null): string {
  if (!value) return 'Not available';
  const spaced = value.replace(/_/g, ' ').toLowerCase();
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

/** Case statuses are already uppercase in the API; render them as-is but readable. */
export function humanizeStatus(value: string | undefined | null): string {
  if (!value) return 'Not available';
  const spaced = value.replace(/_/g, ' ').toLowerCase();
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

/**
 * Truncates an identifier for display while keeping both ends.
 *
 * Razorpay ids and idempotency keys are long and their distinguishing part is
 * rarely in the middle. Cutting the middle keeps two rows visibly different.
 */
export function shortID(id: string | undefined | null, head = 10, tail = 6): string {
  if (!id) return 'Not available';
  if (id.length <= head + tail + 1) return id;
  return `${id.slice(0, head)}…${id.slice(-tail)}`;
}

/** Joins class names, dropping falsy ones. A local `clsx` — one function, no dependency. */
export function cn(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(' ');
}
