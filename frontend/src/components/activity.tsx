'use client';

/**
 * The live activity feed (SRS 16.1).
 *
 * Executed, blocked, escalated and recovered, newest first. All four appear because a
 * feed that showed only successes would misrepresent the system: the blocks and
 * escalations are the guardrails working, and hiding them would make a demo look
 * cleaner while removing the evidence that the policy engine does anything.
 */

import { ActionTypeBadge } from '@/components/badges';
import { CaseLink, MoneyText } from '@/components/ui';
import { cn, formatDateTime, formatRelative } from '@/lib/format';
import { useMounted, useNow } from '@/lib/hooks';
import type { ActivityItem } from '@/lib/types';

type Kind = 'executed' | 'recovered' | 'blocked' | 'escalated';

const KIND_STYLE: Record<Kind, { dot: string; label: string; text: string }> = {
  executed: { dot: 'bg-accent', label: 'Executed', text: 'text-accent-text' },
  recovered: { dot: 'bg-recovered', label: 'Recovered', text: 'text-recovered' },
  blocked: { dot: 'bg-block', label: 'Blocked', text: 'text-block' },
  escalated: { dot: 'bg-escalate', label: 'Escalated', text: 'text-escalate' },
};

function styleFor(kind: string) {
  return (
    KIND_STYLE[kind as Kind] ?? {
      dot: 'bg-muted',
      label: kind,
      text: 'text-muted',
    }
  );
}

export function ActivityFeed({ items }: { items: ActivityItem[] }) {
  const now = useNow(20_000);
  const mounted = useMounted();

  return (
    <ul className="divide-y divide-line/70">
      {items.map((item, i) => {
        const style = styleFor(item.kind);
        return (
          <li
            key={`${item.case_id}-${item.kind}-${item.at}-${i}`}
            className="flex items-start gap-3 px-4 py-3 sm:px-5"
          >
            <span
              aria-hidden
              className={cn('mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full', style.dot)}
            />
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                <span className={cn('text-xs font-medium', style.text)}>{style.label}</span>
                <CaseLink id={item.case_id} reference={item.reference} />
                {item.action_type ? <ActionTypeBadge action={item.action_type} /> : null}
              </div>
              <p className="mt-0.5 break-words font-mono text-2xs text-dim">{item.detail}</p>
            </div>
            <div className="shrink-0 text-right">
              <MoneyText paise={item.amount} className="text-xs text-muted" kpi />
              {/* Relative time only after mount: the server has no idea what the
                  reader's clock says, and a hydration mismatch on a timestamp is
                  resolved silently. */}
              <p className="mt-0.5 text-2xs text-dim" title={formatDateTime(item.at)}>
                {mounted ? formatRelative(item.at, now) : '—'}
              </p>
            </div>
          </li>
        );
      })}
    </ul>
  );
}
