'use client';

/**
 * Approval queue (SRS 16.3).
 *
 * Three requirements, and each one is a deliberate design constraint rather than a
 * layout choice:
 *
 *   1. High-value and low-confidence first. The ordering is done in SQL
 *      (revenue_at_risk DESC, recovery_probability ASC), not here, so the first page
 *      is genuinely the most important page and not just the first page of an
 *      arbitrary order.
 *   2. One-click approve, reason required on reject. Enforced by ApprovalActions and,
 *      independently, by the API — a client-side-only rule is not a rule.
 *   3. No hidden or pre-executed actions before approval. The API hides requests whose
 *      action already ran, and this screen says so out loud rather than silently
 *      omitting rows. A reviewer who cannot see that something is being withheld
 *      cannot tell an empty queue from a filtered one.
 */

import { useState } from 'react';
import type { ReactNode } from 'react';

import { ApprovalActions } from '@/components/approval-actions';
import {
  ActionTypeBadge,
  SegmentBadge,
  SourceBadge,
  StatusBadge,
  UrgencyBadge,
} from '@/components/badges';
import {
  Button,
  Card,
  CardHeader,
  CaseLink,
  EmptyState,
  ErrorBanner,
  KPICard,
  Meter,
  Mono,
  MoneyText,
  PageHeader,
  RefreshingDot,
  Select,
  SkeletonRows,
} from '@/components/ui';
import { api } from '@/lib/api';
import { useAuth } from '@/lib/auth';
import { formatCount, formatMinutes, formatMoneyKPI, humanize } from '@/lib/format';
import { useApi } from '@/lib/hooks';
import type { ApprovalQueueItem } from '@/lib/types';

const QUEUE_POLL_MS = 15_000;
const LIMITS = ['25', '50', '100', '200'] as const;

export default function ApprovalsPage() {
  const { can, user } = useAuth();
  const [limit, setLimit] = useState<string>('50');
  const [includeExecuted, setIncludeExecuted] = useState(false);

  const query = { limit: Number(limit), include_executed: includeExecuted ? 'true' : undefined };
  const queue = useApi(
    `approvals-${limit}-${includeExecuted}`,
    (signal) => api.approvals(query, signal),
    { pollMs: QUEUE_POLL_MS },
  );

  const items = queue.data?.approvals ?? [];
  const totalPending = queue.data?.total_pending ?? 0;
  const hiding = queue.data?.hiding_executed ?? true;

  const atRisk = items.reduce((sum, i) => sum + i.revenue_at_risk, 0);
  const expected = items.reduce((sum, i) => sum + i.expected_recovery, 0);
  const oldest = items.reduce((max, i) => Math.max(max, i.waiting_minutes), 0);

  return (
    <>
      <PageHeader
        title="Approval queue"
        description="Review the decisions that need your judgment. The highest-impact requests appear first."
        right={
          <>
            <RefreshingDot active={queue.refreshing} />
            <Select
              label="Show"
              value={limit}
              onChange={(next) => setLimit(next || '50')}
              options={LIMITS.map((l) => ({ value: l, label: `${l} rows` }))}
            />
          </>
        }
      />

      {!can('reviewer') ? (
        <Card>
          <EmptyState
            title="Reviewer role required."
            detail={`Your account (${user?.email ?? 'unknown'}) has the ${user?.role ?? 'operator'} role. Approving a money-moving action requires reviewer or admin.`}
          />
        </Card>
      ) : (
        <>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
            <KPICard
              label="Awaiting decision"
              value={formatCount(totalPending)}
              tone={totalPending > 0 ? 'escalate' : 'neutral'}
              loading={queue.loading}
              footnote="Total requests waiting for a decision."
            />
            <KPICard
              label="Revenue at risk on this page"
              value={formatMoneyKPI(atRisk)}
              tone="risk"
              loading={queue.loading}
              footnote="Value linked to the requests in this queue."
            />
            <KPICard
              label="Expected recovery on this page"
              value={formatMoneyKPI(expected)}
              tone="accent"
              loading={queue.loading}
              footnote="Estimated recovery if these requests are approved."
            />
            <KPICard
              label="Longest wait"
              value={formatMinutes(oldest)}
              loading={queue.loading}
              footnote="Nothing runs while a request is waiting."
            />
          </div>

          <ErrorBanner error={queue.error} onRetry={queue.reload} />

          <Card>
            <CardHeader
              title="Pending requests"
              subtitle={
                hiding
                  ? 'Executed requests are hidden because they no longer need approval.'
                  : 'Executed requests are shown too. They are here for reference only.'
              }
              right={
                <Button
                  onClick={() => setIncludeExecuted((v) => !v)}
                  title="Reveal requests whose action has already executed. Off by default."
                >
                  {includeExecuted ? 'Hide executed' : 'Show executed'}
                </Button>
              }
            />

            {queue.loading ? (
              <SkeletonRows rows={4} />
            ) : items.length === 0 ? (
              <EmptyState
                title="Nothing waiting on a human."
                detail={
                  hiding
                    ? 'There are no decisions waiting right now. Turn on “Show executed” to review completed requests.'
                    : 'No pending approvals at all.'
                }
              />
            ) : (
              <ul className="divide-y divide-line">
                {items.map((item) => (
                  <ApprovalRow key={item.id} item={item} onDecided={() => queue.reload()} />
                ))}
              </ul>
            )}

            <p className="border-t border-line px-4 py-3 text-2xs leading-relaxed text-dim sm:px-5">
              Showing {formatCount(items.length)} of {formatCount(totalPending)} pending requests.
              Approval clears an escalation. It cannot override a block or add a new action type.
            </p>
          </Card>
        </>
      )}
    </>
  );
}

function ApprovalRow({
  item,
  onDecided,
}: {
  item: ApprovalQueueItem;
  onDecided: () => void;
}) {
  const lowConfidence = item.confidence < 0.7;

  return (
    <li className="p-4 sm:p-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <CaseLink id={item.case_id} reference={item.reference} className="text-sm text-body" />
            <UrgencyBadge urgency={item.urgency} />
            <SourceBadge source={item.source_type} />
            <SegmentBadge segment={item.customer_segment} />
            <StatusBadge status={item.case_status} />
            {item.already_executed ? (
              <span className="chip border-block/30 bg-block-soft text-block" title="This action has already run. There is nothing left to authorise.">
                already executed
              </span>
            ) : null}
          </div>

          <p className="text-sm text-body">{item.customer_name || 'unnamed customer'}</p>

          <p className="max-w-2xl text-xs leading-relaxed text-escalate">
            {item.reason || 'Escalated by the policy engine.'}
          </p>

          <div className="flex flex-wrap items-center gap-x-5 gap-y-2 pt-0.5">
            <Stat label="At risk">
              <MoneyText paise={item.revenue_at_risk} kpi className="text-sm text-block" />
            </Stat>
            <Stat label="Expected recovery">
              <MoneyText paise={item.expected_recovery} kpi className="text-sm text-accent-text" />
            </Stat>
            <Stat label="Risk">
              <Meter value={item.risk_score} tone="risk" />
            </Stat>
            <Stat label="Planner confidence">
              <Meter value={item.confidence} tone={lowConfidence ? 'escalate' : 'pass'} />
            </Stat>
            <Stat label="Proposed">
              <ActionTypeBadge action={item.recommended_action} />
            </Stat>
            <Stat label="Waiting">
              <span className="tnum text-xs text-muted">
                {formatMinutes(item.waiting_minutes)}
              </span>
            </Stat>
          </div>

          {(item.reason_codes ?? []).length > 0 ? (
            <div className="flex flex-wrap items-center gap-1.5 pt-0.5">
              <span className="label">Reason codes</span>
              {(item.reason_codes ?? []).map((code, i) => (
                <Mono key={`${code}-${i}`} className="text-dim" title={humanize(code)}>
                  {code}
                </Mono>
              ))}
            </div>
          ) : null}
        </div>

        <div className="shrink-0">
          {item.already_executed ? (
            <p className="max-w-[14rem] text-2xs leading-relaxed text-dim">
              Shown for inspection only. Deciding this would record a verdict on something that has
              already happened, so the controls are withheld.
            </p>
          ) : (
            <ApprovalActions caseId={item.case_id} onDecided={onDecided} />
          )}
        </div>
      </div>
    </li>
  );
}

function Stat({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <p className="label">{label}</p>
      <div className="mt-0.5">{children}</div>
    </div>
  );
}
