'use client';

/**
 * The case timeline (SRS 16.2: action timeline).
 *
 * One ordered narrative from detection to outcome, assembled server-side from the
 * case, diagnosis, decision, policy checks, approvals, actions, outcomes and audit
 * rows (internal/store/detail.go). Reading it top to bottom is meant to answer "what
 * did this system do, in what order, and on whose authority" without opening a
 * database.
 *
 * Absolute timestamps, not relative ones. This is the artefact someone
 * cross-references against a server log, and "2h ago" cannot be cross-referenced.
 */

import { cn, formatDateTime, humanize } from '@/lib/format';
import { useMounted } from '@/lib/hooks';
import type { TimelineItem } from '@/lib/types';

const KIND_DOT: Record<string, string> = {
  detected: 'bg-block',
  diagnosed: 'bg-accent',
  planned: 'bg-accent',
  policy: 'bg-escalate',
  escalated: 'bg-escalate',
  approval_decision: 'bg-accent',
  action: 'bg-accent',
  outcome: 'bg-recovered',
  audit: 'bg-ink-500',
};

/** Results that should read as a refusal or a stop rather than as progress. */
const NEGATIVE_RESULTS = new Set([
  'BLOCK',
  'rejected',
  'failed',
  'not_recovered',
  'stopped',
  'ambiguous',
]);

const POSITIVE_RESULTS = new Set(['PASS', 'approved', 'executed', 'recovered']);

export function Timeline({ items }: { items: TimelineItem[] }) {
  const mounted = useMounted();

  return (
    <ol className="relative space-y-0 p-4 sm:p-5">
      {/* The rail. Drawn behind the dots rather than as a border on each row, so the
          last item does not trail a line into empty space. */}
      <span aria-hidden className="absolute bottom-6 left-[1.4375rem] top-6 w-px bg-line" />
      {items.map((item, i) => (
        <li key={`${item.at}-${item.kind}-${i}`} className="relative flex gap-3 py-2.5">
          <span
            aria-hidden
            className={cn(
              'relative z-10 mt-1 h-3 w-3 shrink-0 rounded-full ring-4 ring-ink-800',
              KIND_DOT[item.kind] ?? 'bg-ink-500',
            )}
          />
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
              <p className="text-sm text-body">{item.title}</p>
              <p className="shrink-0 font-mono text-2xs text-dim">
                {mounted ? formatDateTime(item.at) : '—'}
              </p>
            </div>
            {item.detail ? (
              <p className="mt-0.5 break-words text-xs leading-relaxed text-muted">{item.detail}</p>
            ) : null}
            <div className="mt-1 flex flex-wrap items-center gap-2">
              <span className="font-mono text-2xs text-dim">{item.kind}</span>
              {item.result ? (
                <span
                  className={cn(
                    'text-2xs font-medium',
                    NEGATIVE_RESULTS.has(item.result)
                      ? 'text-block'
                      : POSITIVE_RESULTS.has(item.result)
                        ? 'text-pass'
                        : 'text-muted',
                  )}
                >
                  {item.result}
                </span>
              ) : null}
              {item.actor ? (
                <span className="text-2xs text-dim" title="Who or what performed this step">
                  by {humanize(item.actor)}
                </span>
              ) : null}
            </div>
          </div>
        </li>
      ))}
    </ol>
  );
}
