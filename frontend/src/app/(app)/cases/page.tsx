'use client';

/**
 * Case queue (SRS 16.2 entry point).
 *
 * The working surface: every at-risk case, filterable by scenario, segment, risk,
 * action type and status, searchable, sortable, paginated. Rows link to the case
 * detail, which is where the decision and its justification live.
 *
 * Filter state is local rather than in the URL. That is a deliberate limitation — a
 * shareable filtered view would be better — but the alternative here means a
 * Suspense boundary around useSearchParams and a prerender path this project has not
 * verified, and a filter that silently drops on reload would be worse than one that
 * plainly does not persist.
 */

import { useMemo, useState } from 'react';

import {
  CaseFilterBar,
  EMPTY_FILTERS,
  filtersActive,
  filtersToQuery,
} from '@/components/case-filters';
import type { CaseFilters } from '@/components/case-filters';
import { CaseTable } from '@/components/case-table';
import {
  Button,
  Card,
  CardHeader,
  DataLabel,
  EmptyState,
  ErrorBanner,
  PageHeader,
  Pagination,
  RefreshingDot,
  Select,
  SkeletonRows,
} from '@/components/ui';
import { api, buildQuery } from '@/lib/api';
import { formatCount } from '@/lib/format';
import { useApi, useDebounced } from '@/lib/hooks';

const PAGE_SIZES = [25, 50, 100, 200] as const;

export default function CasesPage() {
  const [filters, setFilters] = useState<CaseFilters>(EMPTY_FILTERS);
  const [limit, setLimit] = useState<number>(25);
  const [offset, setOffset] = useState(0);

  const debounced = useDebounced(filters, 350);

  const query = useMemo(
    () => ({ ...filtersToQuery(debounced), limit, offset }),
    [debounced, limit, offset],
  );

  const cases = useApi(`cases${buildQuery(query)}`, (signal) => api.cases(query, signal), {
    pollMs: 30_000,
  });

  const page = cases.data?.page;
  const items = page?.items ?? [];

  // Any change to what is being filtered has to reset the offset, or page 4 of the
  // old result set silently becomes an empty page 4 of the new one.
  const applyFilters = (next: CaseFilters) => {
    setFilters(next);
    setOffset(0);
  };

  return (
    <>
      <PageHeader
        title="Cases"
        description="Every case the detection agent opened. Risk score, diagnosis confidence and the planned intervention are shown together, because a high-risk case with a low-confidence diagnosis is a different problem from a high-risk case with a clear one."
        right={
          <>
            <DataLabel label={cases.data?.data_label} />
            <RefreshingDot active={cases.refreshing} />
          </>
        }
      />

      <ErrorBanner error={cases.error} onRetry={cases.reload} />

      <Card>
        <CardHeader
          title={page ? `${formatCount(page.total)} cases` : 'Cases'}
          subtitle="Sorted by expected recovery unless changed"
          right={
            <div className="flex items-end gap-2">
              {filtersActive(filters) ? (
                <Button onClick={() => applyFilters(EMPTY_FILTERS)} className="px-2.5 py-1.5">
                  Clear filters
                </Button>
              ) : null}
              <Select
                label="Per page"
                value={String(limit)}
                onChange={(v) => {
                  setLimit(Number(v) || 25);
                  setOffset(0);
                }}
                options={PAGE_SIZES.map((n) => ({ value: String(n), label: String(n) }))}
                className="w-24"
              />
            </div>
          }
        />

        <CaseFilterBar
          value={filters}
          onChange={applyFilters}
          showStatus
          showSearch
          showSort
        />

        {cases.loading ? (
          <SkeletonRows rows={8} />
        ) : items.length > 0 ? (
          <>
            <CaseTable items={items} />
            <Pagination
              offset={page?.offset ?? 0}
              limit={page?.limit ?? limit}
              total={page?.total ?? 0}
              onOffset={setOffset}
            />
          </>
        ) : (
          <EmptyState
            title="No cases match."
            detail={
              filtersActive(filters)
                ? 'Try clearing a filter. A minimum-risk filter above 0.35 excludes cases detection opened at the threshold.'
                : 'Nothing is at risk yet. Generate a checkout abandonment from the demo checkout, or run the pipeline against seeded data.'
            }
            action={
              filtersActive(filters) ? (
                <Button onClick={() => applyFilters(EMPTY_FILTERS)}>Clear filters</Button>
              ) : null
            }
          />
        )}
      </Card>
    </>
  );
}
