'use client';

/**
 * The case table, shared by the dashboard and the case queue.
 *
 * Column order follows the question an operator asks in order: what is it, whose is
 * it, how much is at stake, how sure are we, what are we going to do, did the policy
 * engine allow it, where has it got to. `compact` drops the middle of that for the
 * dashboard, where the table is a preview rather than the working surface.
 */

import {
  ActionTypeBadge,
  PolicyResultBadge,
  SegmentBadge,
  SourceBadge,
  StatusBadge,
  UrgencyBadge,
} from '@/components/badges';
import { CaseLink, Meter, MoneyText, TableShell, Td, Th } from '@/components/ui';
import { humanize } from '@/lib/format';
import type { CaseListItem } from '@/lib/types';

export function CaseTable({
  items,
  compact = false,
}: {
  items: CaseListItem[];
  compact?: boolean;
}) {
  return (
    <TableShell
      head={
        <>
          <Th>Case</Th>
          <Th>Customer</Th>
          {!compact ? <Th>Scenario</Th> : null}
          <Th align="right">At risk</Th>
          <Th>Risk</Th>
          {!compact ? <Th>Diagnosis</Th> : null}
          <Th>Planned</Th>
          {!compact ? <Th>Policy</Th> : null}
          <Th align="right">Expected</Th>
          {!compact ? <Th align="right">Recovered</Th> : null}
          <Th>Status</Th>
        </>
      }
    >
      {items.map((c) => (
        <tr key={c.id} className="row-hover">
          <Td>
            <CaseLink id={c.id} reference={c.reference} />
            {!compact ? (
              <div className="mt-1">
                <UrgencyBadge urgency={c.urgency} />
              </div>
            ) : null}
          </Td>

          <Td>
            <p className="truncate text-xs text-body" title={c.customer_name}>
              {c.customer_name || '—'}
            </p>
            <div className="mt-1">
              <SegmentBadge segment={c.customer_segment} />
            </div>
          </Td>

          {!compact ? (
            <Td>
              <SourceBadge source={c.source_type} />
            </Td>
          ) : null}

          <Td align="right">
            <MoneyText paise={c.revenue_at_risk} className="text-xs text-body" />
          </Td>

          <Td>
            <Meter value={c.risk_score} tone="risk" />
          </Td>

          {!compact ? (
            <Td>
              {c.root_cause ? (
                <>
                  <p className="text-xs text-body">{humanize(c.root_cause)}</p>
                  {typeof c.confidence === 'number' ? (
                    <div className="mt-1">
                      <Meter value={c.confidence} tone="accent" width="w-12" />
                    </div>
                  ) : null}
                </>
              ) : (
                <span className="text-2xs text-dim">not yet diagnosed</span>
              )}
            </Td>
          ) : null}

          <Td>
            {c.recommended_action ? (
              <ActionTypeBadge action={c.recommended_action} />
            ) : (
              <span className="text-2xs text-dim">—</span>
            )}
          </Td>

          {!compact ? (
            <Td>
              {c.policy_result ? (
                <PolicyResultBadge result={c.policy_result} />
              ) : (
                <span className="text-2xs text-dim">not evaluated</span>
              )}
            </Td>
          ) : null}

          <Td align="right">
            <MoneyText paise={c.expected_recovery} className="text-xs text-accent-text" />
          </Td>

          {!compact ? (
            <Td align="right">
              {c.recovered_amount > 0 ? (
                <MoneyText paise={c.recovered_amount} className="text-xs text-recovered" />
              ) : (
                <span className="text-2xs text-dim">—</span>
              )}
            </Td>
          ) : null}

          <Td>
            <StatusBadge status={c.status} />
          </Td>
        </tr>
      ))}
    </TableShell>
  );
}
