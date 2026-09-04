'use client';

/**
 * Policies (SRS 15.2 admin routes, 10.1 policy object, 10.3 mandatory stopping rules).
 *
 * Every field here is a hard limit the Policy Engine checks before an action can
 * execute — this is not a preferences screen, it is the guardrail configuration. Two
 * things follow from that:
 *
 *   1. Saving activates a new version rather than mutating the current one (PUT
 *      /api/policies always creates + activates). The history table exists so an
 *      admin can see exactly what the limits were at any past moment, which matters
 *      when explaining why an old case was or was not blocked.
 *   2. Money fields are edited in rupees and converted once, here, before the request
 *      leaves the browser — the API and everything downstream of it deals in paise
 *      only.
 */

import { useEffect, useState } from 'react';

import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBanner,
  Mono,
  MoneyText,
  PageHeader,
  SkeletonRows,
  TableShell,
  Td,
  TextField,
  Th,
} from '@/components/ui';
import { api } from '@/lib/api';
import { useAuth } from '@/lib/auth';
import { formatDateTime, formatPercent } from '@/lib/format';
import { useApi, useMutation } from '@/lib/hooks';
import type { Policy, PolicyBound, PolicyField } from '@/lib/types';

const FIELD_ORDER: Array<{
  field: PolicyField;
  label: string;
  hint: string;
  kind: 'int' | 'money' | 'ratio';
}> = [
  {
    field: 'max_retry_count',
    label: 'Max retry count',
    hint: 'Stop after this many retries on one case.',
    kind: 'int',
  },
  {
    field: 'max_reminders_per_case',
    label: 'Max reminders per case',
    hint: 'Caps repeated contact on one case, independent of retries.',
    kind: 'int',
  },
  {
    field: 'max_actions_per_case',
    label: 'Max actions per case',
    hint: 'Absolute ceiling on total actions for one case, across all types.',
    kind: 'int',
  },
  {
    field: 'max_actions_per_customer_per_day',
    label: 'Max actions per customer per day',
    hint: 'Stop after this many actions for one customer in a day.',
    kind: 'int',
  },
  {
    field: 'cooldown_minutes',
    label: 'Cooldown (minutes)',
    hint: 'Minimum gap between two actions on the same case.',
    kind: 'int',
  },
  {
    field: 'min_action_confidence',
    label: 'Minimum action confidence',
    hint: 'Below this level, a recommendation needs human approval.',
    kind: 'ratio',
  },
  {
    field: 'max_automated_amount',
    label: 'Max automated amount (\u20b9)',
    hint: 'Above this amount, an action requires human approval even at high confidence.',
    kind: 'money',
  },
  {
    field: 'require_human_approval_above',
    label: 'Require approval above (\u20b9)',
    hint: 'A second approval threshold. The default is ₹1,00,000.',
    kind: 'money',
  },
];

type FormState = Record<PolicyField, string>;

function toForm(p: Policy): FormState {
  return {
    max_retry_count: String(p.max_retry_count),
    max_reminders_per_case: String(p.max_reminders_per_case),
    max_actions_per_case: String(p.max_actions_per_case),
    max_actions_per_customer_per_day: String(p.max_actions_per_customer_per_day),
    cooldown_minutes: String(p.cooldown_minutes),
    min_action_confidence: String(p.min_action_confidence),
    max_automated_amount: (p.max_automated_amount / 100).toString(),
    require_human_approval_above: (p.require_human_approval_above / 100).toString(),
  };
}

export default function PoliciesPage() {
  const { can, user } = useAuth();
  const policies = useApi('policies', (signal) => api.policies(signal));
  const update = useMutation(api.updatePolicy);

  const [form, setForm] = useState<FormState | undefined>(undefined);
  const [versionLabel, setVersionLabel] = useState('');

  useEffect(() => {
    if (policies.data && !form) setForm(toForm(policies.data.active));
  }, [policies.data, form]);

  if (!can('admin')) {
    return (
      <>
        <PageHeader title="Policies" />
        <Card>
          <EmptyState
            title="Admin role required."
            detail={`Your account (${user?.email ?? 'unknown'}) has the ${user?.role ?? 'operator'} role. Editing the guardrails every action is checked against requires admin.`}
          />
        </Card>
      </>
    );
  }

  const limits = policies.data?.limits ?? {};
  const active = policies.data?.active;
  const trimmedVersion = versionLabel.trim();
  const versionError =
    trimmedVersion.length > 32 ? 'A version label must be 32 characters or fewer.' : undefined;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!form || !trimmedVersion || versionError) return;
    const body: Record<string, number | string | boolean> = {
      version: trimmedVersion,
      // The button reads "Save and activate" — this makes that literally true.
      // Without it the API stages the version without switching the policy
      // engine to it, which would look identical on screen but silently change
      // nothing about what gets enforced.
      activate: true,
    };
    for (const { field, kind } of FIELD_ORDER) {
      const raw = form[field];
      const num = Number.parseFloat(raw);
      if (!Number.isFinite(num)) continue;
      body[field] = kind === 'money' ? Math.round(num * 100) : num;
    }
    const result = await update.run(body);
    if (result) {
      policies.reload();
      setForm(toForm(result.policy));
      setVersionLabel('');
    }
  };

  const resetToDefault = () => {
    if (policies.data) setForm(toForm(policies.data.default));
  };
  const resetToActive = () => {
    if (policies.data) setForm(toForm(policies.data.active));
  };

  return (
    <>
      <PageHeader
        title="Policies"
        description="Set the guardrails that every recovery action must pass. Each save creates a new version, so your history stays intact."
        right={active ? <span className="chip border-line-strong bg-ink-700 text-muted">active {active.version}</span> : null}
      />

      <ErrorBanner error={policies.error} onRetry={policies.reload} />

      <Card>
        <CardHeader
          title="Current limits"
          subtitle={active ? `Updated ${formatDateTime(active.updated_at)}${active.updated_by ? ` by ${active.updated_by}` : ''}` : undefined}
        />
        {policies.loading || !form ? (
          <SkeletonRows rows={4} />
        ) : (
          <form onSubmit={submit} className="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2 sm:p-5 lg:grid-cols-4">
            <TextField
              label="New version label"
              value={versionLabel}
              onChange={setVersionLabel}
              placeholder="e.g. v2 or tighter-cooldown"
              hint={versionError ? undefined : 'Required. Each save creates a new version with this name.'}
              error={versionError}
              className="sm:col-span-2 lg:col-span-4"
              required
            />
            {FIELD_ORDER.map(({ field, label, hint }) => (
              <PolicyInput
                key={field}
                label={label}
                hint={hint}
                value={form[field]}
                bound={limits[field]}
                onChange={(v) => setForm({ ...form, [field]: v })}
              />
            ))}
            <div className="flex flex-wrap items-center gap-2 sm:col-span-2 lg:col-span-4">
              <Button
                type="submit"
                variant="primary"
                pending={update.pending}
                disabled={!trimmedVersion || Boolean(versionError)}
                title={!trimmedVersion ? 'Enter a version label above first.' : undefined}
              >
                Save and activate
              </Button>
              <Button type="button" onClick={resetToActive}>
                Reset to active
              </Button>
              <Button type="button" onClick={resetToDefault} title="Loads the standard values into the form. Changes are not saved until you activate them.">
                Load defaults
              </Button>
            </div>
          </form>
        )}
        <ErrorBanner error={update.error} className="mx-4 mb-4 sm:mx-5" />
        {update.result ? (
          <p className="mx-4 mb-4 text-xs text-recovered sm:mx-5">
            Activated policy version {update.result.policy.version}.
          </p>
        ) : null}
      </Card>

      <Card>
        <CardHeader title="Version history" subtitle="Every past version, most recent first." />
        {policies.loading ? (
          <SkeletonRows rows={3} />
        ) : (policies.data?.history ?? []).length === 0 ? (
          <EmptyState title="No prior versions." />
        ) : (
          <TableShell
            head={
              <>
                <Th>Version</Th>
                <Th>Updated</Th>
                <Th>By</Th>
                <Th align="right">Max retries</Th>
                <Th align="right">Min confidence</Th>
                <Th align="right">Max automated</Th>
                <Th align="right">Approval above</Th>
              </>
            }
          >
            {(policies.data?.history ?? []).map((p) => (
              <tr key={p.version} className="hover:bg-ink-700/40">
                <Td>
                  <Mono>{p.version}</Mono>
                </Td>
                <Td>{formatDateTime(p.updated_at)}</Td>
                <Td>{p.updated_by || 'Not available'}</Td>
                <Td align="right">{p.max_retry_count}</Td>
                <Td align="right">{formatPercent(p.min_action_confidence)}</Td>
                <Td align="right">
                  <MoneyText paise={p.max_automated_amount} kpi />
                </Td>
                <Td align="right">
                  <MoneyText paise={p.require_human_approval_above} kpi />
                </Td>
              </tr>
            ))}
          </TableShell>
        )}
      </Card>
    </>
  );
}

function PolicyInput({
  label,
  hint,
  value,
  bound,
  onChange,
}: {
  label: string;
  hint: string;
  value: string;
  bound?: PolicyBound;
  onChange: (next: string) => void;
}) {
  const num = Number.parseFloat(value);
  const outOfRange =
    Number.isFinite(num) && bound ? num < bound.min || (bound.max !== null && num > bound.max) : false;
  const range = bound ? `Allowed: ${bound.min} \u2013 ${bound.max === null ? 'unbounded' : bound.max}.` : undefined;

  return (
    <TextField
      label={label}
      type="number"
      value={value}
      onChange={onChange}
      min={bound?.min}
      max={bound?.max ?? undefined}
      step="any"
      hint={outOfRange ? undefined : [hint, range].filter(Boolean).join(' ')}
      error={outOfRange ? `Out of range. ${range ?? ''}` : undefined}
    />
  );
}
