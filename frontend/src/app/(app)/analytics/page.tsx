'use client';

/**
 * Strategy performance (SRS 10.4, 18.1–18.2).
 *
 * One row per (segment, scenario, action) combination the Intervention Planner has
 * actually tried, with a sample size next to every rate. `sufficient` is the flag
 * that matters most on this screen: a 100% success rate on one attempt is not a
 * strategy that works, it is a strategy that has been tried once, and treating the
 * two the same is exactly the kind of overclaim SRS 25.2 exists to prevent.
 */

import { useMemo, useState } from 'react';

import { ActionTypeBadge, SegmentBadge, SourceBadge } from '@/components/badges';
import {
  Card,
  CardHeader,
  DataLabel,
  EmptyState,
  ErrorBanner,
  KPICard,
  PageHeader,
  Select,
  SkeletonRows,
  TableShell,
  Td,
  Th,
} from '@/components/ui';
import { api } from '@/lib/api';
import { formatCount, formatMoneyKPI, formatPercent, humanize } from '@/lib/format';
import { useApi } from '@/lib/hooks';
import { ACTION_TYPES, SEGMENTS, SOURCE_TYPES } from '@/lib/types';
import type { ActionType, Segment, SourceType, StrategyMetric } from '@/lib/types';

const POLL_MS = 30_000;

export default function AnalyticsPage() {
  const [segment, setSegment] = useState<Segment | ''>('');
  const [source, setSource] = useState<SourceType | ''>('');
  const [action, setAction] = useState<ActionType | ''>('');

  const strategies = useApi('strategies', (signal) => api.strategies(signal), { pollMs: POLL_MS });

  const rows = useMemo(() => {
    const all = strategies.data?.strategies ?? [];
    return all
      .filter((r) => !segment || r.segment === segment)
      .filter((r) => !source || r.source_type === source)
      .filter((r) => !action || r.action_type === action)
      .sort((a, b) => b.recovered_amount - a.recovered_amount);
  }, [strategies.data, segment, source, action]);

  const totals = strategies.data?.totals;

  return (
    <>
      <PageHeader
        title="Strategy performance"
        description="What the Intervention Planner has actually tried, broken down by customer segment, scenario and action — the evidence a recommendation is grounded in rather than decorative (SRS 6.5, 18.2)."
        right={<DataLabel label={strategies.data?.data_label} />}
      />

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <KPICard
          label="Total attempts"
          value={totals ? formatCount(totals.attempts) : '—'}
          loading={strategies.loading}
        />
        <KPICard
          label="Successful"
          value={totals ? formatCount(totals.successes) : '—'}
          tone="recovered"
          loading={strategies.loading}
        />
        <KPICard
          label="Recovered amount"
          value={totals ? formatMoneyKPI(totals.recovered_amount) : '—'}
          tone="recovered"
          loading={strategies.loading}
        />
        <KPICard
          label="Overall success rate"
          value={totals ? formatPercent(totals.success_rate) : '—'}
          tone="accent"
          loading={strategies.loading}
          footnote={
            totals
              ? `A segment/scenario/action cell needs at least ${formatCount(totals.min_attempts_signal)} attempts before its rate is treated as a signal.`
              : undefined
          }
        />
      </div>

      <ErrorBanner error={strategies.error} onRetry={strategies.reload} />

      <Card>
        <CardHeader
          title="By segment, scenario and action"
          subtitle="Sorted by recovered amount. A grey rate means the sample is too small to trust yet, not that the strategy failed."
        />
        <div className="grid grid-cols-1 gap-3 border-b border-line px-4 py-3 sm:grid-cols-3 sm:px-5">
          <Select
            label="Segment"
            value={segment}
            onChange={setSegment}
            options={SEGMENTS.map((s) => ({ value: s, label: humanize(s) }))}
            includeAll
            allLabel="All segments"
          />
          <Select
            label="Scenario"
            value={source}
            onChange={setSource}
            options={SOURCE_TYPES.map((s) => ({ value: s, label: humanize(s) }))}
            includeAll
            allLabel="All scenarios"
          />
          <Select
            label="Action"
            value={action}
            onChange={setAction}
            options={ACTION_TYPES.map((a) => ({ value: a, label: humanize(a) }))}
            includeAll
            allLabel="Any action"
          />
        </div>

        {strategies.loading ? (
          <SkeletonRows rows={6} />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No intervention outcomes match these filters."
            detail="Run the demo checkout, sync test payments, or run the simulation lab to generate outcomes."
          />
        ) : (
          <TableShell
            head={
              <>
                <Th>Segment</Th>
                <Th>Scenario</Th>
                <Th>Action</Th>
                <Th align="right">Attempts</Th>
                <Th align="right">Successes</Th>
                <Th align="right">Success rate</Th>
                <Th align="right">Recovered</Th>
              </>
            }
          >
            {rows.map((row) => (
              <StrategyRow key={`${row.segment}-${row.source_type}-${row.action_type}`} row={row} />
            ))}
          </TableShell>
        )}
      </Card>
    </>
  );
}

function StrategyRow({ row }: { row: StrategyMetric }) {
  return (
    <tr className="hover:bg-ink-700/40">
      <Td>
        <SegmentBadge segment={row.segment} />
      </Td>
      <Td>
        <SourceBadge source={row.source_type} />
      </Td>
      <Td>
        <ActionTypeBadge action={row.action_type} />
      </Td>
      <Td align="right">{formatCount(row.attempts)}</Td>
      <Td align="right">{formatCount(row.successes)}</Td>
      <Td
        align="right"
        className={row.sufficient ? 'text-body' : 'text-dim'}
        title={row.sufficient ? undefined : 'Sample too small to be a reliable signal yet.'}
      >
        {row.success_rate === null ? '—' : formatPercent(row.success_rate)}
        {!row.sufficient && row.success_rate !== null ? ' *' : ''}
      </Td>
      <Td align="right">{formatMoneyKPI(row.recovered_amount)}</Td>
    </tr>
  );
}
