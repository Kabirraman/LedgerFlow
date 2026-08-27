'use client';

/**
 * Presentational primitives.
 *
 * Nothing here fetches or decides. The reason they live in one file is that the
 * decisions they encode — how a loading state looks, how an error looks, how a
 * "this deployment does not have that wired" looks — have to be the same on all nine
 * screens, or the reader learns nine different vocabularies for the same condition.
 */

import Link from 'next/link';
import type { ReactNode } from 'react';

import { ApiError } from '@/lib/api';
import { cn, formatCount, formatMoney, formatMoneyKPI } from '@/lib/format';

// --- layout ---

export function Card({ children, className }: { children: ReactNode; className?: string }) {
  return <section className={cn('card', className)}>{children}</section>;
}

export function CardHeader({
  title,
  subtitle,
  right,
  className,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  right?: ReactNode;
  className?: string;
}) {
  return (
    <header
      className={cn(
        'flex flex-wrap items-start justify-between gap-3 border-b border-line px-4 py-3 sm:px-5',
        className,
      )}
    >
      <div className="min-w-0">
        <h2 className="text-sm font-semibold text-body">{title}</h2>
        {subtitle ? <p className="mt-0.5 text-xs text-muted">{subtitle}</p> : null}
      </div>
      {right ? <div className="flex shrink-0 items-center gap-2">{right}</div> : null}
    </header>
  );
}

export function PageHeader({
  title,
  description,
  right,
}: {
  title: string;
  description?: ReactNode;
  right?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-xl font-semibold tracking-tight text-body">{title}</h1>
        {description ? <p className="mt-1 max-w-3xl text-xs text-muted">{description}</p> : null}
      </div>
      {right ? <div className="flex shrink-0 flex-wrap items-center gap-2">{right}</div> : null}
    </div>
  );
}

// --- provenance ---

/**
 * The data-provenance label.
 *
 * SRS 25.2 forbids presenting synthetic recovery amounts as live merchant revenue,
 * so the API attaches a `data_label` to every figure-bearing response and this
 * renders it verbatim. Not paraphrased, not abbreviated: whatever the server says
 * these numbers are, that is what appears next to them.
 */
export function DataLabel({ label, className }: { label: string | undefined; className?: string }) {
  if (!label) return null;
  const synthetic = label.includes('synthetic');
  return (
    <span
      className={cn(
        'chip',
        synthetic
          ? 'border-escalate/30 bg-escalate-soft text-escalate'
          : 'border-line-strong bg-ink-700 text-muted',
        className,
      )}
      title={
        synthetic
          ? 'These figures come from the versioned synthetic benchmark, not from live merchant revenue.'
          : 'These figures come from Razorpay test mode.'
      }
    >
      <Dot />
      {label}
    </span>
  );
}

function Dot() {
  return <span aria-hidden className="inline-block h-1.5 w-1.5 rounded-full bg-current" />;
}

// --- states ---

export function Skeleton({ className }: { className?: string }) {
  return <div className={cn('skeleton', className)} aria-hidden />;
}

export function SkeletonRows({ rows = 5, className }: { rows?: number; className?: string }) {
  return (
    <div className={cn('space-y-2 p-4 sm:p-5', className)}>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-9 w-full" />
      ))}
      <span className="sr-only">Loading</span>
    </div>
  );
}

export function EmptyState({
  title,
  detail,
  action,
}: {
  title: string;
  detail?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 px-6 py-12 text-center">
      <p className="text-sm font-medium text-muted">{title}</p>
      {detail ? <p className="max-w-md text-xs text-dim">{detail}</p> : null}
      {action ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}

/**
 * Renders an API failure.
 *
 * Three cases are distinguished because they need three different reactions from the
 * reader: a capability this deployment does not have (nothing to do), a permission
 * they lack (ask someone else), and an actual failure (retry, and quote the request
 * id if it persists).
 */
export function ErrorBanner({
  error,
  onRetry,
  className,
}: {
  error: ApiError | undefined;
  onRetry?: () => void;
  className?: string;
}) {
  if (!error) return null;

  const notConfigured = error.isNotConfigured;
  const forbidden = error.isForbidden;
  const tone = notConfigured
    ? 'border-escalate/30 bg-escalate-soft text-escalate'
    : 'border-block/30 bg-block-soft text-block';

  return (
    <div className={cn('rounded-card border px-4 py-3', tone, className)} role="alert">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="text-xs font-semibold uppercase tracking-wider opacity-80">
            {notConfigured
              ? 'not enabled in this deployment'
              : forbidden
                ? 'not permitted for your role'
                : 'request failed'}
          </p>
          <p className="mt-1 text-sm">{error.message}</p>
          {Object.keys(error.details).length > 0 ? (
            <ul className="mt-1.5 space-y-0.5 text-xs opacity-90">
              {Object.entries(error.details).map(([field, message]) => (
                <li key={field}>
                  <span className="font-mono">{field}</span>: {message}
                </li>
              ))}
            </ul>
          ) : null}
          {error.requestId ? (
            <p className="mt-1.5 font-mono text-2xs opacity-70">request {error.requestId}</p>
          ) : null}
        </div>
        {onRetry && !notConfigured && !forbidden ? (
          <button type="button" className="btn btn-ghost shrink-0" onClick={onRetry}>
            Retry
          </button>
        ) : null}
      </div>
    </div>
  );
}

/** A small spinner for inline pending states. */
export function Spinner({ className }: { className?: string }) {
  return (
    <svg
      className={cn('h-4 w-4 animate-spin', className)}
      viewBox="0 0 24 24"
      fill="none"
      aria-hidden
    >
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity="0.25" strokeWidth="3" />
      <path
        d="M21 12a9 9 0 0 0-9-9"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
      />
    </svg>
  );
}

/** A quiet marker that a background poll is in flight. Never blocks reading. */
export function RefreshingDot({ active }: { active: boolean }) {
  return (
    <span
      className={cn(
        'inline-block h-1.5 w-1.5 rounded-full transition-opacity',
        active ? 'bg-accent opacity-100' : 'bg-transparent opacity-0',
      )}
      title={active ? 'refreshing' : undefined}
      aria-hidden
    />
  );
}

// --- numbers ---

export type KPITone = 'neutral' | 'risk' | 'recovered' | 'accent' | 'escalate';

const KPI_TONES: Record<KPITone, string> = {
  neutral: 'text-body',
  risk: 'text-block',
  recovered: 'text-recovered',
  accent: 'text-accent-text',
  escalate: 'text-escalate',
};

/**
 * A headline figure.
 *
 * The four the SRS names as primary — revenue at risk, recovered, recovery rate,
 * automated actions (SRS 16.1) — plus the secondary ones the same shape serves.
 * `footnote` exists because a bare number on a card invites a wrong reading, and the
 * cheapest fix is one line saying what it counts.
 */
export function KPICard({
  label,
  value,
  footnote,
  tone = 'neutral',
  loading,
}: {
  label: string;
  value: ReactNode;
  footnote?: ReactNode;
  tone?: KPITone;
  loading?: boolean;
}) {
  return (
    <div className="card card-pad">
      <p className="label">{label}</p>
      {loading ? (
        <Skeleton className="mt-2 h-7 w-28" />
      ) : (
        <p className={cn('tnum mt-1.5 text-2xl font-semibold tracking-tight', KPI_TONES[tone])}>
          {value}
        </p>
      )}
      {footnote ? <p className="mt-1 text-2xs leading-relaxed text-dim">{footnote}</p> : null}
    </div>
  );
}

/**
 * An amount, always via the one paise→rupee conversion.
 *
 * `kpi` drops the trailing `.00` on a whole-rupee figure for headline cards; it never
 * drops a real fraction, so a total is never made to look tidier than it is.
 */
export function MoneyText({
  paise,
  className,
  kpi,
}: {
  paise: number;
  className?: string;
  kpi?: boolean;
}) {
  return (
    <span className={cn('tnum font-mono', className)}>
      {kpi ? formatMoneyKPI(paise) : formatMoney(paise)}
    </span>
  );
}

/** A count with tabular digits, so columns of counts align. */
export function CountText({ n, className }: { n: number; className?: string }) {
  return <span className={cn('tnum', className)}>{formatCount(n)}</span>;
}

/**
 * A 0..1 value as a bar.
 *
 * Used for risk score and model confidence. The numeric value is always printed
 * beside it: a bar alone cannot be compared across rows accurately, and these are
 * numbers people make decisions on.
 */
export function Meter({
  value,
  tone = 'accent',
  className,
  width = 'w-16',
}: {
  value: number;
  tone?: 'accent' | 'risk' | 'pass' | 'escalate';
  className?: string;
  width?: string;
}) {
  const clamped = Number.isFinite(value) ? Math.min(1, Math.max(0, value)) : 0;
  const fill =
    tone === 'risk'
      ? 'bg-block'
      : tone === 'pass'
        ? 'bg-pass'
        : tone === 'escalate'
          ? 'bg-escalate'
          : 'bg-accent';
  return (
    <span
      className={cn('inline-flex items-center gap-2', className)}
      role="img"
      aria-label={`${(clamped * 100).toFixed(0)} percent`}
    >
      <span className={cn('h-1.5 overflow-hidden rounded-full bg-ink-600', width)}>
        <span
          className={cn('block h-full rounded-full', fill)}
          style={{ width: `${clamped * 100}%` }}
        />
      </span>
      <span className="tnum text-2xs text-muted">{clamped.toFixed(2)}</span>
    </span>
  );
}

// --- controls ---

export function Button({
  children,
  onClick,
  variant = 'ghost',
  disabled,
  pending,
  type = 'button',
  className,
  title,
}: {
  children: ReactNode;
  onClick?: () => void;
  variant?: 'primary' | 'ghost' | 'approve' | 'reject';
  disabled?: boolean;
  pending?: boolean;
  type?: 'button' | 'submit';
  className?: string;
  title?: string;
}) {
  const variantClass =
    variant === 'primary'
      ? 'btn-primary'
      : variant === 'approve'
        ? 'btn-approve'
        : variant === 'reject'
          ? 'btn-reject'
          : 'btn-ghost';
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled || pending}
      title={title}
      className={cn('btn', variantClass, className)}
    >
      {pending ? <Spinner /> : null}
      {children}
    </button>
  );
}

export function Select<T extends string>({
  label,
  value,
  onChange,
  options,
  includeAll,
  allLabel = 'All',
  className,
  id,
}: {
  label: string;
  value: T | '';
  onChange: (next: T | '') => void;
  options: ReadonlyArray<{ value: T; label: string }>;
  includeAll?: boolean;
  allLabel?: string;
  className?: string;
  id?: string;
}) {
  const selectId = id ?? `sel-${label.replace(/\s+/g, '-').toLowerCase()}`;
  return (
    <div className={cn('min-w-0', className)}>
      <label className="label mb-1 block" htmlFor={selectId}>
        {label}
      </label>
      <select
        id={selectId}
        className="field"
        value={value}
        onChange={(e) => onChange(e.target.value as T | '')}
      >
        {includeAll ? <option value="">{allLabel}</option> : null}
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </div>
  );
}

export function TextField({
  label,
  value,
  onChange,
  placeholder,
  type = 'text',
  className,
  id,
  hint,
  error,
  min,
  max,
  step,
  required,
  autoComplete,
}: {
  label: string;
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  type?: 'text' | 'email' | 'password' | 'number' | 'search';
  className?: string;
  id?: string;
  hint?: ReactNode;
  error?: string;
  min?: number;
  max?: number;
  step?: number | string;
  required?: boolean;
  autoComplete?: string;
}) {
  const fieldId = id ?? `f-${label.replace(/\s+/g, '-').toLowerCase()}`;
  return (
    <div className={cn('min-w-0', className)}>
      <label className="label mb-1 block" htmlFor={fieldId}>
        {label}
      </label>
      <input
        id={fieldId}
        type={type}
        className={cn('field', error && 'border-block/60', type === 'number' && 'tnum font-mono')}
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        min={min}
        max={max}
        step={step}
        required={required}
        autoComplete={autoComplete}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${fieldId}-err` : undefined}
      />
      {error ? (
        <p id={`${fieldId}-err`} className="mt-1 text-2xs text-block">
          {error}
        </p>
      ) : hint ? (
        <p className="mt-1 text-2xs text-dim">{hint}</p>
      ) : null}
    </div>
  );
}

// --- tables ---

export function TableShell({
  head,
  children,
  className,
}: {
  head: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn('overflow-x-auto', className)}>
      <table className="w-full min-w-[52rem] border-collapse text-sm">
        <thead>
          <tr className="table-head">{head}</tr>
        </thead>
        <tbody className="divide-y divide-line/70">{children}</tbody>
      </table>
    </div>
  );
}

export function Th({
  children,
  align = 'left',
  className,
}: {
  children?: ReactNode;
  align?: 'left' | 'right' | 'center';
  className?: string;
}) {
  return (
    <th
      scope="col"
      className={cn(
        'whitespace-nowrap px-3 py-2 font-medium',
        align === 'right' && 'text-right',
        align === 'center' && 'text-center',
        className,
      )}
    >
      {children}
    </th>
  );
}

export function Td({
  children,
  align = 'left',
  className,
  title,
}: {
  children?: ReactNode;
  align?: 'left' | 'right' | 'center';
  className?: string;
  title?: string;
}) {
  return (
    <td
      title={title}
      className={cn(
        'px-3 py-2.5 align-middle',
        align === 'right' && 'text-right',
        align === 'center' && 'text-center',
        className,
      )}
    >
      {children}
    </td>
  );
}

/**
 * Offset pagination controls.
 *
 * Shows the absolute range and total rather than just page numbers, because "1–25 of
 * 312" answers the question an operator triaging a queue is actually asking.
 */
export function Pagination({
  offset,
  limit,
  total,
  onOffset,
}: {
  offset: number;
  limit: number;
  total: number;
  onOffset: (next: number) => void;
}) {
  const from = total === 0 ? 0 : offset + 1;
  const to = Math.min(offset + limit, total);
  const canPrev = offset > 0;
  const canNext = offset + limit < total;
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line px-4 py-3 sm:px-5">
      <p className="tnum text-xs text-muted">
        {from}–{to} of {formatCount(total)}
      </p>
      <div className="flex items-center gap-2">
        <Button
          onClick={() => onOffset(Math.max(0, offset - limit))}
          disabled={!canPrev}
          className="px-2.5 py-1.5"
        >
          Previous
        </Button>
        <Button
          onClick={() => onOffset(offset + limit)}
          disabled={!canNext}
          className="px-2.5 py-1.5"
        >
          Next
        </Button>
      </div>
    </div>
  );
}

// --- misc ---

/** A monospaced identifier. `title` carries the untruncated value for cross-referencing. */
export function Mono({
  children,
  className,
  title,
}: {
  children: ReactNode;
  className?: string;
  title?: string;
}) {
  return (
    <span className={cn('font-mono text-xs text-muted', className)} title={title}>
      {children}
    </span>
  );
}

export function CaseLink({
  id,
  reference,
  className,
}: {
  id: string;
  reference: string;
  className?: string;
}) {
  return (
    <Link href={`/cases/${id}`} className={cn('font-mono text-xs hover:underline', className)}>
      {reference}
    </Link>
  );
}

/** A labelled key/value pair, for detail panels. */
export function Detail({
  label,
  children,
  className,
}: {
  label: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn('min-w-0', className)}>
      <p className="label">{label}</p>
      <div className="mt-1 text-sm text-body">{children}</div>
    </div>
  );
}
