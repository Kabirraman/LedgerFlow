'use client';

/**
 * The recovery funnel: identified → diagnosed → actioned → recovered (SRS 16.1).
 *
 * Bars are scaled against the widest stage rather than against a fixed axis, and each
 * step carries its conversion from the step before. That step-to-step number is the
 * point of the chart: "1,200 identified, 40 recovered" says almost nothing about
 * where the loop is leaking, whereas seeing 96% → 71% → 18% says the drop is at
 * execution.
 *
 * Drawn with divs. A charting library would render this identically and add a
 * dependency whose behaviour would not be verified anywhere in this project.
 */

import { cn, formatCount, formatPercent } from '@/lib/format';
import type { RecoveryFunnel } from '@/lib/types';

interface Stage {
  key: keyof RecoveryFunnel;
  label: string;
  hint: string;
  fill: string;
  text: string;
}

const STAGES: Stage[] = [
  {
    key: 'identified',
    label: 'Identified',
    hint: 'Cases the detection agent opened because revenue was at risk.',
    fill: 'bg-accent/70',
    text: 'text-accent-text',
  },
  {
    key: 'diagnosed',
    label: 'Diagnosed',
    hint: 'Cases with a root cause on record. A case diagnosed UNKNOWN still counts — the agent is allowed to not know.',
    fill: 'bg-accent/50',
    text: 'text-accent-text',
  },
  {
    key: 'actioned',
    label: 'Actioned',
    hint: 'Cases where at least one recovery action executed against Razorpay test mode.',
    fill: 'bg-escalate/60',
    text: 'text-escalate',
  },
  {
    key: 'recovered',
    label: 'Recovered',
    hint: 'Cases closed by a verified payment. Nothing is counted here on the strength of an action succeeding.',
    fill: 'bg-recovered/70',
    text: 'text-recovered',
  },
];

export function FunnelChart({ funnel }: { funnel: RecoveryFunnel }) {
  const widest = Math.max(funnel.identified, funnel.diagnosed, funnel.actioned, funnel.recovered, 1);

  return (
    <ol className="space-y-3 p-4 sm:p-5">
      {STAGES.map((stage, i) => {
        const value = funnel[stage.key];
        const previousStage = i > 0 ? STAGES[i - 1] : undefined;
        const previous = previousStage ? funnel[previousStage.key] : undefined;
        // Undefined rather than 0 when there is nothing to divide by: "0.0%"
        // conversion from an empty stage would read as a failure rather than as an
        // absence of data.
        const conversion =
          previous === undefined || previous === 0 ? undefined : value / previous;

        return (
          <li key={stage.key}>
            <div className="flex items-baseline justify-between gap-3">
              <span className="text-xs font-medium text-body" title={stage.hint}>
                {stage.label}
              </span>
              <span className="flex items-baseline gap-2">
                <span className={cn('tnum text-sm font-semibold', stage.text)}>
                  {formatCount(value)}
                </span>
                {conversion !== undefined ? (
                  <span
                    className="tnum text-2xs text-dim"
                    title={`${formatPercent(conversion)} of the ${previousStage?.label.toLowerCase()} stage`}
                  >
                    {formatPercent(conversion)}
                  </span>
                ) : null}
              </span>
            </div>
            <div className="mt-1.5 h-2 overflow-hidden rounded-full bg-ink-600">
              <div
                className={cn('h-full rounded-full transition-[width] duration-500', stage.fill)}
                style={{ width: `${(value / widest) * 100}%` }}
              />
            </div>
          </li>
        );
      })}
    </ol>
  );
}
