'use client';

/**
 * Dashboard (SRS 16.1).
 *
 * Four primary KPI cards, the recovery funnel, the live activity feed, and a filtered
 * view of the case queue. The dashboard polls; it is the screen people leave open.
 *
 * Two honesty constraints shape this screen:
 *
 *   - Every figure carries the API's own `data_label` (SRS 25.2). These are Razorpay
 *     test-mode numbers and the screen says so, next to the numbers, not in a footer.
 *   - Recovered amount comes from verified outcomes only. An action that executed
 *     successfully is not recovery, and the funnel makes the gap between "actioned"
 *     and "recovered" visible rather than smoothing it over.
 */

import Link from 'next/link';
import { useMemo, useState } from 'react';

import { ActivityFeed } from '@/components/activity';
import { CaseFilterBar, EMPTY_FILTERS, filtersToQuery } from '@/components/case-filters';
import type { CaseFilters } from '@/components/case-filters';
import { CaseTable } from '@/components/case-table';
import { FunnelChart } from '@/components/funnel';
import {
  Card,
  CardHeader,
  CountText,
  DataLabel,
  EmptyState,
  ErrorBanner,
  KPICard,
  MoneyText,
  PageHeader,
  RefreshingDot,
  SkeletonRows,
} from '@/components/ui';
import { api, buildQuery } from '@/lib/api';
import {
  formatCount,
  formatDateTime,
  formatLatency,
  formatMinutes,
  formatMoneyKPI,
  formatPercent,
} from '@/lib/format';
import { useApi, useDebounced, useMounted } from '@/lib/hooks';

const DASHBOARD_POLL_MS = 20_000;
const PREVIEW_LIMIT = 8;

export default function DashboardPage() {
  const [filters, setFilters] = useState<CaseFilters>(EMPTY_FILTERS);
  const debouncedFilters = useDebounced(filters, 350);
  const mounted = useMounted();

  const summary = useApi('dashboard', (signal) => api.dashboard(signal), {
    pollMs: DASHBOARD_POLL_MS,
  });

  // The filtered preview reuses /api/cases rather than adding a second aggregate, so
  // the numbers in this table and the numbers on the Cases screen come from one query.
  const caseQuery = useMemo(
    () => ({ ...filtersToQuery(debouncedFilters), limit: PREVIEW_LIMIT, offset: 0 }),
    [debouncedFilters],
  );
  const cases = useApi(
    `dashboard-cases${buildQuery(caseQuery)}`,
    (signal) => api.cases(caseQuery, signal),
    { pollMs: DASHBOARD_POLL_MS },
  );

  const s = summary.data?.summary;
  const items = cases.data?.page.items ?? [];

  return (
    <>
      <PageHeader
        title="Revenue recovery"
        description="Detected, diagnosed, actioned and verified. Recovered amounts are banked only from a verified payment — an action that executed is not yet a recovery."
        right={
          <>
            <DataLabel label={summary.data?.data_label} />
            <RefreshingDot active={summary.refreshing} />
          </>
        }
      />

      <ErrorBanner error={summary.error} onRetry={summary.reload} />

      {/* SRS 16.1: Revenue at Risk, Recovered, Recovery Rate, Automated Actions. */}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <KPICard
          label="Revenue at risk"
          value={s ? formatMoneyKPI(s.revenue_at_risk) : '—'}
          tone="risk"
          loading={summary.loading}
          footnote={
            s ? (
              <>
                {formatCount(s.open_cases)} open · {formatMoneyKPI(s.unresolved_revenue)} still
                unresolved
              </>
            ) : null
          }
        />
        <KPICard
          label="Recovered"
          value={s ? formatMoneyKPI(s.recovered_amount) : '—'}
          tone="recovered"
          loading={summary.loading}
          footnote={
            s ? <>{formatMoneyKPI(s.recovered_per_intervention)} per intervention</> : null
          }
        />
        <KPICard
          label="Recovery rate"
          value={s ? formatPercent(s.recovery_rate) : '—'}
          tone="accent"
          loading={summary.loading}
          footnote="Recovered amount as a share of revenue at risk."
        />
        <KPICard
          label="Automated actions"
          value={s ? formatCount(s.automated_actions) : '—'}
          loading={summary.loading}
          footnote={
            s ? (
              <>
                {formatCount(s.escalated_cases)} escalated · {formatCount(s.blocked_actions)} blocked
                by policy
              </>
            ) : null
          }
        />
      </div>

      {/* Secondary figures. Separated from the four the SRS names as primary so the
          headline row stays the headline row. */}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <KPICard
          label="Expected recovery"
          value={s ? formatMoneyKPI(s.expected_recovery) : '—'}
          loading={summary.loading}
          footnote="Revenue at risk × recovery probability × intervention feasibility, summed over open cases."
        />
        <KPICard
          label="Avg time to recovery"
          value={s ? formatMinutes(s.avg_recovery_minutes) : '—'}
          loading={summary.loading}
          footnote="From case creation to verified payment."
        />
        <KPICard
          label="Escalation rate"
          value={s ? formatPercent(s.escalation_rate) : '—'}
          tone="escalate"
          loading={summary.loading}
          footnote="Share of cases handed to a human rather than actioned autonomously."
        />
        <KPICard
          label="Policy violations"
          value={s ? formatCount(s.policy_violations) : '—'}
          tone={s && s.policy_violations > 0 ? 'risk' : 'recovered'}
          loading={summary.loading}
          footnote="Actions that executed without passing policy. This must be zero (SRS 17.4)."
        />
      </div>

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-3">
        <Card>
          <CardHeader
            title="Recovery funnel"
            subtitle="identified → diagnosed → actioned → recovered"
          />
          {summary.loading ? (
            <SkeletonRows rows={4} />
          ) : s ? (
            <FunnelChart funnel={s.funnel} />
          ) : (
            <EmptyState title="No funnel data yet." />
          )}
        </Card>

        <Card className="xl:col-span-2">
          <CardHeader
            title="Activity"
            subtitle="Executed, recovered, blocked and escalated — newest first"
            right={<RefreshingDot active={summary.refreshing} />}
          />
          {summary.loading ? (
            <SkeletonRows rows={6} />
          ) : s?.activity && s.activity.length > 0 ? (
            <ActivityFeed items={s.activity} />
          ) : (
            <EmptyState
              title="Nothing has happened yet."
              detail="Actions, blocks and escalations appear here as the pipeline runs. Generate a case from the demo checkout to see the loop end to end."
            />
          )}
        </Card>
      </div>

      <Card>
        <CardHeader
          title="By scenario"
          subtitle="Where the at-risk revenue is, and how much of it comes back"
        />
        {summary.loading ? (
          <SkeletonRows rows={4} />
        ) : s?.by_source && s.by_source.length > 0 ? (
          <div className="grid grid-cols-1 divide-y divide-line/70 sm:grid-cols-2 sm:divide-y-0 lg:grid-cols-4">
            {s.by_source.map((row) => (
              <div key={row.source_type} className="p-4 sm:p-5">
                <p className="text-xs font-medium text-body">
                  {row.source_type.replace(/_/g, ' ')}
                </p>
                <p className="mt-2 flex items-baseline justify-between gap-2 text-xs">
                  <span className="text-dim">Cases</span>
                  <CountText n={row.cases} className="text-body" />
                </p>
                <p className="mt-1 flex items-baseline justify-between gap-2 text-xs">
                  <span className="text-dim">At risk</span>
                  <MoneyText paise={row.revenue_at_risk} className="text-body" kpi />
                </p>
                <p className="mt-1 flex items-baseline justify-between gap-2 text-xs">
                  <span className="text-dim">Recovered</span>
                  <MoneyText paise={row.recovered} className="text-recovered" kpi />
                </p>
                <p className="mt-1 flex items-baseline justify-between gap-2 text-xs">
                  <span className="text-dim">Rate</span>
                  <span className="tnum text-accent-text">{formatPercent(row.recovery_rate)}</span>
                </p>
              </div>
            ))}
          </div>
        ) : (
          <EmptyState title="No cases by scenario yet." />
        )}
      </Card>

      <Card>
        <CardHeader
          title="At-risk cases"
          subtitle={`Highest expected recovery first · top ${PREVIEW_LIMIT}`}
          right={
            <>
              <DataLabel label={cases.data?.data_label} />
              <RefreshingDot active={cases.refreshing} />
            </>
          }
        />
        <CaseFilterBar value={filters} onChange={setFilters} />
        <ErrorBanner error={cases.error} onRetry={cases.reload} className="m-4 sm:m-5" />
        {cases.loading ? (
          <SkeletonRows rows={6} />
        ) : items.length > 0 ? (
          <>
            <CaseTable items={items} compact />
            <div className="border-t border-line px-4 py-3 text-xs text-muted sm:px-5">
              Showing {items.length} of {formatCount(cases.data?.page.total ?? 0)} matching cases.{' '}
              <Link href="/cases" className="hover:underline">
                Open the full queue
              </Link>
              .
            </div>
          </>
        ) : (
          <EmptyState
            title="No cases match these filters."
            detail="Clear a filter, or generate a case from the demo checkout."
          />
        )}
      </Card>

      <Card>
        <CardHeader
          title="Operational health"
          subtitle="Webhook intake, duplicate suppression and agent behaviour (SRS 18.3)"
          right={
            mounted && summary.data ? (
              <span className="text-2xs text-dim">as of {formatDateTime(summary.data.as_of)}</span>
            ) : null
          }
        />
        {summary.loading ? (
          <SkeletonRows rows={3} />
        ) : s ? (
          <dl className="grid grid-cols-2 gap-x-6 gap-y-3 p-4 sm:grid-cols-4 sm:p-5">
            <OpsStat label="Webhooks received" value={formatCount(s.operational.webhooks_received)} />
            <OpsStat
              label="Signature failures"
              value={formatCount(s.operational.webhook_signature_failures)}
              tone={s.operational.webhook_signature_failures > 0 ? 'warn' : 'ok'}
              hint="Rejected before parsing, verified against the raw request body."
            />
            <OpsStat
              label="Duplicate events"
              value={`${formatCount(s.operational.duplicate_events)} · ${formatPercent(
                s.operational.duplicate_event_rate,
              )}`}
              hint="Suppressed by a unique index on the external event id, not by application logic."
            />
            <OpsStat
              label="Action API failures"
              value={formatCount(s.operational.action_api_failures)}
              tone={s.operational.action_api_failures > 0 ? 'warn' : 'ok'}
            />
            <OpsStat
              label="Avg action latency"
              value={formatLatency(s.operational.avg_action_latency_ms)}
            />
            <OpsStat
              label="Avg agent latency"
              value={formatLatency(s.operational.avg_agent_latency_ms)}
            />
            <OpsStat
              label="Agent fallbacks"
              value={formatCount(s.operational.agent_fallback_count)}
              tone={s.operational.agent_fallback_count > 0 ? 'warn' : 'ok'}
              hint="Times an agent timed out, returned invalid JSON or fell below the confidence floor, and the deterministic path took over (SRS 20.4)."
            />
            <OpsStat
              label="Blocked actions"
              value={formatCount(s.blocked_actions)}
              hint="The policy engine refusing an action is the guardrail working, not a failure."
            />
          </dl>
        ) : (
          <EmptyState title="No operational metrics yet." />
        )}
      </Card>
    </>
  );
}

function OpsStat({
  label,
  value,
  hint,
  tone,
}: {
  label: string;
  value: string;
  hint?: string;
  tone?: 'ok' | 'warn';
}) {
  return (
    <div className="min-w-0" title={hint}>
      <dt className="label">{label}</dt>
      <dd
        className={
          tone === 'warn'
            ? 'tnum mt-1 text-sm font-medium text-escalate'
            : tone === 'ok'
              ? 'tnum mt-1 text-sm font-medium text-pass'
              : 'tnum mt-1 text-sm font-medium text-body'
        }
      >
        {value}
      </dd>
    </div>
  );
}
