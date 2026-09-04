'use client';

/**
 * Operations (SRS 18.3, FR-002, FR-003, FR-005).
 *
 * This screen is for the person debugging the pipeline, not the person tracking
 * revenue: raw counters (webhook intake, dedupe, agent latency and fallback) and the
 * underlying event log, signature result and rejection reason included. A rejected
 * webhook is shown rather than hidden — a webhook endpoint that only ever shows
 * successes cannot be debugged when Razorpay's signature stops matching.
 */

import { useState } from 'react';

import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBanner,
  Mono,
  PageHeader,
  Select,
  SkeletonRows,
  TableShell,
  Td,
  Th,
  TextField,
} from '@/components/ui';
import { api } from '@/lib/api';
import { useAuth } from '@/lib/auth';
import { cn, formatCount, formatDateTime, humanize } from '@/lib/format';
import { useApi, useMutation } from '@/lib/hooks';
import type { CounterValue } from '@/lib/types';

const METRICS_POLL_MS = 15_000;
const EVENTS_POLL_MS = 15_000;
const LIMITS = ['25', '50', '100'] as const;

export default function OpsPage() {
  const { can } = useAuth();
  const [limit, setLimit] = useState<string>('50');

  const metrics = useApi('ops-metrics', (signal) => api.opsMetrics(signal), {
    pollMs: METRICS_POLL_MS,
  });
  const events = useApi(`ops-events-${limit}`, (signal) => api.opsEvents(Number(limit), signal), {
    pollMs: EVENTS_POLL_MS,
  });

  const counters = Object.entries(metrics.data?.counters ?? {}).sort(([a], [b]) => a.localeCompare(b));

  return (
    <>
      <PageHeader
        title="Operations"
        description="Keep an eye on system activity, delivery health, and the events behind each result."
        right={
          metrics.data ? (
            <span className="text-2xs text-dim">as of {formatDateTime(metrics.data.as_of)}</span>
          ) : null
        }
      />

      <ErrorBanner error={metrics.error} onRetry={metrics.reload} />

      <Card>
        <CardHeader title="Counters" subtitle="A quick view of the activity this environment has recorded." />
        {metrics.loading ? (
          <SkeletonRows rows={4} />
        ) : counters.length === 0 ? (
          <EmptyState title="No counters recorded yet." />
        ) : (
          <div className="grid grid-cols-2 gap-x-6 gap-y-4 p-4 sm:grid-cols-3 sm:p-5 lg:grid-cols-4">
            {counters.map(([key, value]) => (
              <CounterStat key={key} label={humanize(key)} value={value} />
            ))}
          </div>
        )}
      </Card>

      {can('admin') ? <SyncPanel /> : null}

      <Card>
        <CardHeader
          title="Event log"
          subtitle="Webhook and backfill events, with the newest first."
          right={
            <Select
              label="Show"
              value={limit}
              onChange={(next) => setLimit(next || '50')}
              options={LIMITS.map((l) => ({ value: l, label: `${l} rows` }))}
            />
          }
        />
        <ErrorBanner error={events.error} onRetry={events.reload} className="m-4 sm:m-5" />
        {events.loading ? (
          <SkeletonRows rows={6} />
        ) : (events.data?.events ?? []).length === 0 ? (
          <EmptyState
            title="No events received yet."
            detail="Generate one from the demo checkout, or trigger a Razorpay test-mode webhook."
          />
        ) : (
          <TableShell
            head={
              <>
                <Th>Received</Th>
                <Th>Source</Th>
                <Th>Event type</Th>
                <Th>Signature</Th>
                <Th>Entity</Th>
                <Th>Processed</Th>
                <Th>Rejection reason</Th>
              </>
            }
          >
            {(events.data?.events ?? []).map((ev) => (
              <tr key={ev.id} className="hover:bg-ink-700/40">
                <Td title={ev.id}>{formatDateTime(ev.received_at)}</Td>
                <Td>{ev.source}</Td>
                <Td className="font-mono text-xs">{ev.event_type}</Td>
                <Td>
                  <span
                    className={cn(
                      'chip',
                      ev.signature_valid
                        ? 'border-pass/30 bg-pass-soft text-pass'
                        : 'border-block/30 bg-block-soft text-block',
                    )}
                  >
                    {ev.signature_valid ? 'valid' : 'invalid'}
                  </span>
                </Td>
                <Td>{ev.entity_id ? <Mono title={ev.entity_id}>{ev.entity_id}</Mono> : 'Not available'}</Td>
                <Td>{ev.processed_at ? formatDateTime(ev.processed_at) : 'pending'}</Td>
                <Td className="max-w-xs truncate text-block" title={ev.rejection_reason}>
                  {ev.rejection_reason || 'None'}
                </Td>
              </tr>
            ))}
          </TableShell>
        )}
        <p className="border-t border-line px-4 py-3 text-2xs leading-relaxed text-dim sm:px-5">
          Showing {formatCount((events.data?.events ?? []).length)} of the most recent events. Deduplication is
          enforced by a unique index on the external event id, not by anything on this screen.
        </p>
      </Card>
    </>
  );
}

function CounterStat({ label, value }: { label: string; value: CounterValue }) {
  return (
    <div className="min-w-0">
      <p className="label">{label}</p>
      <p className="tnum mt-1 text-lg font-semibold text-body">{formatCount(value.count)}</p>
      <p className="text-2xs text-dim">
        Sum {value.sum.toFixed(2)}, average {value.mean.toFixed(2)}
      </p>
    </div>
  );
}

/** Admin-only backfill trigger. Read-only observation data, not an executor route. */
function SyncPanel() {
  const [hours, setHours] = useState('24');
  const [count, setCount] = useState('50');
  const sync = useMutation(api.syncPayments);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    await sync.run(Number.parseInt(hours, 10) || 24, Number.parseInt(count, 10) || 50);
  };

  return (
    <Card>
      <CardHeader
        title="Backfill payments"
        subtitle="Sync test-mode payment records for a selected time window. Admin access required."
      />
      <form onSubmit={submit} className="flex flex-wrap items-end gap-3 p-4 sm:p-5">
        <TextField label="Window (hours)" type="number" min={1} value={hours} onChange={setHours} className="w-32" />
        <TextField label="Max records" type="number" min={1} value={count} onChange={setCount} className="w-32" />
        <Button type="submit" pending={sync.pending}>
          Sync now
        </Button>
      </form>
      <ErrorBanner error={sync.error} className="mx-4 mb-4 sm:mx-5" />
      {sync.result ? (
        <pre className="mx-4 mb-4 overflow-x-auto rounded-card border border-line bg-ink-800 p-3 text-2xs text-muted sm:mx-5">
          {JSON.stringify(sync.result.report, null, 2)}
        </pre>
      ) : null}
    </Card>
  );
}
