'use client';

/**
 * The case filter bar (SRS 16.1: filters by scenario, customer segment, risk and
 * action type).
 *
 * Shared by the dashboard and the case queue so the two cannot disagree about what
 * "high value" or "checkout abandonment" selects. Every option value here is a
 * literal from the Go enums — the API rejects an unknown enum with a 400 naming the
 * field, so a typo in this file would surface as a validation error rather than as a
 * silently empty table, but keeping the lists derived from the shared `as const`
 * arrays means there is nothing to typo.
 */

import { Select, TextField } from '@/components/ui';
import { humanize } from '@/lib/format';
import type { ActionType, CaseStatus, Segment, SourceType } from '@/lib/types';
import { ACTION_TYPES, CASE_STATUSES, SEGMENTS, SOURCE_TYPES } from '@/lib/types';

export type CaseSortBy = 'expected_recovery' | 'risk_score' | 'created_at' | 'revenue_at_risk';

export interface CaseFilters {
  source_type: SourceType | '';
  segment: Segment | '';
  action_type: ActionType | '';
  status: CaseStatus | '';
  /** Kept as a string so the field can be genuinely empty rather than defaulting to 0. */
  min_risk: string;
  search: string;
  sort_by: CaseSortBy;
}

export const EMPTY_FILTERS: CaseFilters = {
  source_type: '',
  segment: '',
  action_type: '',
  status: '',
  min_risk: '',
  search: '',
  sort_by: 'expected_recovery',
};

const SORT_OPTIONS: ReadonlyArray<{ value: CaseSortBy; label: string }> = [
  { value: 'expected_recovery', label: 'Expected recovery' },
  { value: 'revenue_at_risk', label: 'Revenue at risk' },
  { value: 'risk_score', label: 'Risk score' },
  { value: 'created_at', label: 'Newest first' },
];

const asOptions = <T extends string>(values: readonly T[]) =>
  values.map((value) => ({ value, label: humanize(value) }));

/** True when any filter is set, so the "clear" control can be hidden when it would be a no-op. */
export function filtersActive(f: CaseFilters): boolean {
  return Boolean(f.source_type || f.segment || f.action_type || f.status || f.min_risk || f.search);
}

/**
 * Turns filter state into query parameters.
 *
 * `min_risk` is only sent when it parses to a number in range, so a half-typed "0."
 * does not produce a 400 mid-keystroke.
 */
export function filtersToQuery(f: CaseFilters): Record<string, string | number | undefined> {
  const minRisk = Number.parseFloat(f.min_risk);
  return {
    source_type: f.source_type || undefined,
    segment: f.segment || undefined,
    action_type: f.action_type || undefined,
    status: f.status || undefined,
    search: f.search.trim() || undefined,
    sort_by: f.sort_by,
    min_risk:
      f.min_risk !== '' && Number.isFinite(minRisk) && minRisk >= 0 && minRisk <= 1
        ? minRisk
        : undefined,
  };
}

export function CaseFilterBar({
  value,
  onChange,
  showStatus = false,
  showSearch = false,
  showSort = false,
}: {
  value: CaseFilters;
  onChange: (next: CaseFilters) => void;
  showStatus?: boolean;
  showSearch?: boolean;
  showSort?: boolean;
}) {
  const patch = (part: Partial<CaseFilters>) => onChange({ ...value, ...part });

  return (
    <div className="grid grid-cols-1 gap-3 border-b border-line px-4 py-3 sm:grid-cols-2 sm:px-5 lg:grid-cols-4">
      <Select
        label="Scenario"
        value={value.source_type}
        onChange={(v) => patch({ source_type: v })}
        options={asOptions(SOURCE_TYPES)}
        includeAll
        allLabel="All scenarios"
      />
      <Select
        label="Customer segment"
        value={value.segment}
        onChange={(v) => patch({ segment: v })}
        options={asOptions(SEGMENTS)}
        includeAll
        allLabel="All segments"
      />
      <Select
        label="Action type"
        value={value.action_type}
        onChange={(v) => patch({ action_type: v })}
        options={asOptions(ACTION_TYPES)}
        includeAll
        allLabel="Any action"
      />
      <TextField
        label="Minimum risk"
        type="number"
        min={0}
        max={1}
        step={0.05}
        value={value.min_risk}
        onChange={(v) => patch({ min_risk: v })}
        placeholder="0.35"
        hint="Detection opens a case at 0.35."
      />

      {showStatus ? (
        <Select
          label="Status"
          value={value.status}
          onChange={(v) => patch({ status: v })}
          options={CASE_STATUSES.map((s) => ({ value: s, label: s.replace(/_/g, ' ') }))}
          includeAll
          allLabel="Any status"
        />
      ) : null}

      {showSort ? (
        <Select
          label="Sort by"
          value={value.sort_by}
          onChange={(v) => patch({ sort_by: (v || 'expected_recovery') as CaseSortBy })}
          options={SORT_OPTIONS}
        />
      ) : null}

      {showSearch ? (
        <TextField
          label="Search"
          type="search"
          value={value.search}
          onChange={(v) => patch({ search: v })}
          placeholder="Reference, customer, email"
          className="sm:col-span-2"
        />
      ) : null}
    </div>
  );
}
