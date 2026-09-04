'use client';

/**
 * The "why this action" panel (AC-010).
 *
 * Everything shown here is assembled server-side from persisted reason codes, policy
 * check rows and trusted amounts (internal/httpapi/explain.go). None of it is model
 * prose, and none of it is the model's reasoning trace — AC-010 asks for an
 * explanation of the decision, explicitly not for private chain-of-thought, and those
 * are different artefacts. A reason code with a plain-English reading beside it is
 * auditable; a paragraph the model wrote about its own thinking is not.
 *
 * The panel says so on its face, because a reviewer needs to know what kind of
 * explanation they are reading before they decide how much to trust it.
 */

import { DecidedByBadge, PolicyResultBadge } from '@/components/badges';
import { Meter, MoneyText } from '@/components/ui';
import { cn } from '@/lib/format';
import type { Explanation } from '@/lib/types';

export function ExplanationPanel({ explanation }: { explanation: Explanation }) {
  const because = explanation.because ?? [];
  const evidence = explanation.evidence ?? [];
  const considered = explanation.considered ?? [];
  const controls = explanation.controls ?? [];
  const uncertainty = explanation.uncertainty ?? [];

  const objections = controls.filter((c) => c.result !== 'PASS');

  return (
    <div className="space-y-5 p-4 sm:p-5">
      <div>
        <p className="text-sm font-medium leading-relaxed text-body">{explanation.headline}</p>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <DecidedByBadge source={explanation.decided_by} model={explanation.model_name} />
          <span className="chip border-line-strong bg-ink-700 text-muted">
            Confidence
            <Meter
              value={explanation.confidence}
              tone={explanation.confidence >= 0.7 ? 'pass' : 'escalate'}
              width="w-10"
            />
          </span>
          <span className="chip border-accent/30 bg-accent-soft text-accent-text">
            Expected recovery <MoneyText paise={explanation.expected_recovery} kpi />
          </span>
        </div>
      </div>

      {because.length > 0 ? (
        <Block title="Because">
          <ul className="space-y-1.5">
            {because.map((item) => (
              <li key={item.code} className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                <span className="text-sm text-body">{item.reading}</span>
                {/* The raw code stays visible. It is what appears in the audit log and
                    in the database, so a reviewer comparing the two needs it. */}
                <code className="font-mono text-2xs text-dim">{item.code}</code>
              </li>
            ))}
          </ul>
        </Block>
      ) : null}

      {evidence.length > 0 ? (
        <Block
          title="Evidence used"
          hint="These are the records used to support the recommendation."
        >
          <ul className="space-y-1">
            {evidence.map((e, i) => (
              <li key={`${e}-${i}`} className="font-mono text-2xs leading-relaxed text-muted">
                {e}
              </li>
            ))}
          </ul>
        </Block>
      ) : null}

      {considered.length > 0 ? (
        <Block
          title="Alternatives considered"
          hint="Actions the planner weighed and did not select. Recorded so a rejected option is visible rather than absent."
        >
          <div className="flex flex-wrap gap-1.5">
            {considered.map((a, i) => (
              <span key={`${a}-${i}`} className="chip border-line-strong bg-ink-700 text-muted">
                {a}
              </span>
            ))}
          </div>
        </Block>
      ) : null}

      {controls.length > 0 ? (
        <Block
          title="Controls applied"
          hint={
            objections.length > 0
              ? 'Rules that need attention appear first. A block remains final, even after review.'
              : 'Every rule the policy engine evaluated for this decision.'
          }
        >
          <ul className="space-y-2">
            {controls.map((c, i) => (
              <li
                key={`${c.rule}-${i}`}
                className={cn(
                  'rounded-lg border px-3 py-2',
                  c.result === 'BLOCK'
                    ? 'border-block/30 bg-block-soft/40'
                    : c.result === 'ESCALATE'
                      ? 'border-escalate/30 bg-escalate-soft/40'
                      : 'border-line bg-ink-700/50',
                )}
              >
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <span className="text-xs text-body">{c.reading}</span>
                  <PolicyResultBadge result={c.result} />
                </div>
                <p className="mt-1 font-mono text-2xs text-dim">{c.rule}</p>
                {c.details ? <p className="mt-1 text-2xs text-muted">{c.details}</p> : null}
              </li>
            ))}
          </ul>
        </Block>
      ) : null}

      {uncertainty.length > 0 ? (
        <Block
          title="Flagged uncertainty"
          hint="These are the parts of the diagnosis that need more confidence."
        >
          <ul className="space-y-1">
            {uncertainty.map((u, i) => (
              <li key={`${u}-${i}`} className="text-xs text-escalate">
                {u}
              </li>
            ))}
          </ul>
        </Block>
      ) : null}

      {explanation.stop_condition ? (
        <Block
          title="Stop condition"
          hint="What would make the system stop pursuing this case. Declared before the action runs, not after."
        >
          <p className="text-xs text-body">{explanation.stop_condition}</p>
        </Block>
      ) : null}

      <p className="border-t border-line pt-3 text-2xs leading-relaxed text-dim">
        This explanation is built from stored reason codes, policy checks and trusted amounts. It
        records what was decided and which rules applied. It does not include private model reasoning.
      </p>
    </div>
  );
}

function Block({
  title,
  hint,
  children,
}: {
  title: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <section>
      <h3 className="label" title={hint}>
        {title}
      </h3>
      {hint ? <p className="mb-2 mt-0.5 text-2xs leading-relaxed text-dim">{hint}</p> : null}
      <div className={hint ? '' : 'mt-2'}>{children}</div>
    </section>
  );
}
