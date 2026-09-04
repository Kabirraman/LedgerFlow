'use client';

/**
 * The audit log (SRS 16.2: full audit log accessible on demand).
 *
 * Fetched only when opened. "On demand" is meant literally — an audit log that loaded
 * with every case detail would be several hundred rows nobody asked for, and the
 * endpoint is deliberately unpaginated because a partial audit trail is not an audit
 * trail.
 *
 * The case id and action id columns are the point of the table, not decoration:
 * AC-005 requires every external side effect to be traceable to both, and this is
 * where that linkage is visible.
 */

import { useState } from 'react';

import { Button, EmptyState, ErrorBanner, Mono, SkeletonRows, TableShell, Td, Th } from '@/components/ui';
import { api } from '@/lib/api';
import { formatCount, formatDateTime, humanize, shortID } from '@/lib/format';
import { useApi, useMounted } from '@/lib/hooks';

export function AuditLog({ caseId }: { caseId: string }) {
  const [open, setOpen] = useState(false);
  const mounted = useMounted();

  // key === null means "do not fetch". The request is issued the first time the panel
  // is opened and not before.
  const audit = useApi(open ? `audit-${caseId}` : null, (signal) => api.audit(caseId, signal));

  const rows = audit.data?.audit ?? [];

  return (
    <div>
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3 sm:px-5">
        <div className="min-w-0">
          <h2 className="text-sm font-semibold text-body">Audit log</h2>
          <p className="mt-0.5 text-xs text-muted">
            Every recorded event for this case, including its related actions.
          </p>
        </div>
        <Button onClick={() => setOpen((v) => !v)} pending={open && audit.loading}>
          {open ? 'Hide' : 'Show audit log'}
        </Button>
      </div>

      {!open ? null : (
        <>
          <ErrorBanner error={audit.error} onRetry={audit.reload} className="m-4 sm:m-5" />
          {audit.loading ? (
            <SkeletonRows rows={6} />
          ) : rows.length > 0 ? (
            <>
              <TableShell
                head={
                  <>
                    <Th>When</Th>
                    <Th>Event</Th>
                    <Th>Actor</Th>
                    <Th>Entity</Th>
                    <Th>Case</Th>
                    <Th>Action</Th>
                  </>
                }
              >
                {rows.map((row) => (
                  <tr key={row.id} className="row-hover align-top">
                    <Td className="whitespace-nowrap">
                      <Mono>{mounted ? formatDateTime(row.timestamp) : 'Loading'}</Mono>
                    </Td>
                    <Td>
                      <p className="text-xs text-body">{humanize(row.event_type)}</p>
                      <Mono className="text-dim">{row.event_type}</Mono>
                    </Td>
                    <Td>
                      <span className="text-xs text-muted">{row.actor || 'System'}</span>
                    </Td>
                    <Td>
                      <p className="text-xs text-muted">{row.entity_type}</p>
                      <Mono className="text-dim">{shortID(row.entity_id)}</Mono>
                    </Td>
                    <Td>
                      <Mono title={row.case_id}>{shortID(row.case_id)}</Mono>
                    </Td>
                    <Td>
                      <Mono title={row.action_id}>{shortID(row.action_id)}</Mono>
                    </Td>
                  </tr>
                ))}
              </TableShell>
              <p className="border-t border-line px-4 py-3 text-2xs text-dim sm:px-5">
                {formatCount(audit.data?.count ?? rows.length)} entries. This endpoint is not
                paginated: a truncated audit trail would be misleading.
              </p>
            </>
          ) : (
            <EmptyState
              title="No audit entries."
              detail="Audit entries appear as the case moves through recovery. Check back after the next action."
            />
          )}
        </>
      )}
    </div>
  );
}
