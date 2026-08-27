'use client';

/**
 * Case detail (SRS 16.2), in the order the SRS lists it:
 *
 *   amount / risk / expected recovery / status → why-at-risk evidence and diagnosis
 *   confidence → planner decision and alternatives → policy checks with
 *   PASS/BLOCK/ESCALATE → action timeline and external Razorpay resource IDs →
 *   outcome and recovered amount → full audit log on demand.
 *
 * The external resource IDs matter more than they look. They are how a reviewer
 * confirms that a payment link this console claims to have created actually exists in
 * the Razorpay test dashboard — which is the difference between a demo that did
 * something and a demo that said it did.
 */

import Link from 'next/link';
import { useState } from 'react';

import {
  ActionStatusBadge,
  ActionTypeBadge,
  DecidedByBadge,
  ModeBadge,
  OutcomeBadge,
  PolicyResultBadge,
  SegmentBadge,
  SourceBadge,
  StatusBadge,
  UrgencyBadge,
  DecisionBadge,
} from '@/components/badges';
import { ApprovalActions } from '@/components/approval-actions';
import { AuditLog } from '@/components/audit-log';
import { ExplanationPanel } from '@/components/explanation';
import { Timeline } from '@/components/timeline';
import {
  Button,
  Card,
  CardHeader,
  DataLabel,
  Detail,
  EmptyState,
  ErrorBanner,
  KPICard,
  Meter,
  Mono,
  MoneyText,
  RefreshingDot,
  Spinner,
  TableShell,
  Td,
  Th,
} from '@/components/ui';
import { api } from '@/lib/api';
import { useAuth } from '@/lib/auth';
import {
  formatCount,
  formatDate,
  formatDateTime,
  formatLatency,
  formatMinutes,
  formatMoneyKPI,
  formatPercent,
  humanize,
  shortID,
} from '@/lib/format';
import { useApi, useMounted, useMutation } from '@/lib/hooks';

export function CaseView({ caseId }: { caseId: string }) {
  const { can } = useAuth();
  const mounted = useMounted();
  const [notice, setNotice] = useState<string | undefined>(undefined);

  const detail = useApi(`case-${caseId}`, (signal) => api.caseDetail(caseId, signal), {
    pollMs: 15_000,
  });

  const reanalyze = useMutation(() => api.reanalyze(caseId));
  const verify = useMutation(() => api.verifyCase(caseId));

  const d = detail.data?.case;
  const c = d?.case;

  const pendingApproval = (d?.approvals ?? []).find((a) => a.decision === 'pending');

  if (detail.loading) {
    return (
      <div className="flex items-center gap-2 py-16 text-muted">
        <Spinner />
        <span className="text-xs">Loading case…</span>
      </div>
    );
  }

  if (!d || !c) {
    return (
      <>
        <ErrorBanner error={detail.error} onRetry={detail.reload} />
        {!detail.error ? (
          <EmptyState
            title="Case not found."
            detail="It may have been created in a different environment, or the reference may be wrong."
            action={
              <Link href="/cases" className="btn btn-ghost">
                Back to cases
              </Link>
            }
          />
        ) : null}
      </>
    );
  }

  const explanation = detail.data?.explanation;

  return (
    <>
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <Link href="/cases" className="text-xs text-muted hover:text-body">
              ← Cases
            </Link>
          </div>
          <h1 className="mt-1 font-mono text-xl font-semibold tracking-tight text-body">
            {c.reference}
          </h1>
          <div className="mt-2 flex flex-wrap items-center gap-2">
            <StatusBadge status={c.status} />
            <UrgencyBadge urgency={c.urgency} />
            <SourceBadge source={c.source_type} />
            <ModeBadge mode={c.mode} />
            {d.customer ? <SegmentBadge segment={d.customer.segment} /> : null}
            <DataLabel label={detail.data?.data_label} />
            <RefreshingDot active={detail.refreshing} />
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <Button
            pending={reanalyze.pending}
            onClick={() =>
              void reanalyze.run().then((r) => {
                if (!r) return;
                setNotice(
                  r.restarted
                    ? 'Re-analysis started. The pipeline will re-diagnose and re-plan this case.'
                    : 'The case was already being analysed.',
                );
                detail.reload();
              })
            }
            title="Send the case back through detection, diagnosis, planning and policy. Blocked while a case is awaiting human review or already closed."
          >
            Re-analyse
          </Button>
          <Button
            pending={verify.pending}
            onClick={() =>
              void verify.run().then((r) => {
                if (!r) return;
                setNotice('Verification requested against the payment gateway.');
                detail.reload();
              })
            }
            title="Ask the verifier to check the gateway for payment against the latest executed action. Recovery is only banked from a verified payment."
          >
            Verify now
          </Button>
        </div>
      </div>

      <ErrorBanner error={detail.error} onRetry={detail.reload} />
      <ErrorBanner error={reanalyze.error ?? verify.error} />
      {notice ? (
        <p className="rounded-card border border-accent/30 bg-accent-soft px-4 py-2.5 text-xs text-accent-text">
          {notice}
        </p>
      ) : null}

      {/* SRS 16.2, first line: case amount, risk score, expected recovery and status. */}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <KPICard
          label="Revenue at risk"
          value={formatMoneyKPI(c.revenue_at_risk)}
          tone="risk"
          footnote="The trusted amount from the payment record — never a figure produced by a model."
        />
        <KPICard
          label="Risk score"
          value={c.risk_score.toFixed(2)}
          footnote="Weighted from failure severity, customer intent, payment reliability, amount, time sensitivity and recovery window (SRS 9.1)."
        />
        <KPICard
          label="Expected recovery"
          value={formatMoneyKPI(c.expected_recovery)}
          tone="accent"
          footnote="Revenue at risk × recovery probability × intervention feasibility (SRS 9.2)."
        />
        <KPICard
          label="Recovered"
          value={formatMoneyKPI(c.recovered_amount)}
          tone={c.recovered_amount > 0 ? 'recovered' : 'neutral'}
          footnote={
            c.recovered_amount > 0
              ? 'Banked from a verified payment.'
              : 'Nothing banked yet. An executed action is not a recovery.'
          }
        />
      </div>

      {pendingApproval ? (
        <Card className="border-escalate/40">
          <CardHeader
            title="Awaiting human approval"
            subtitle={pendingApproval.reason || 'The policy engine escalated this decision.'}
            right={<DecisionBadge decision={pendingApproval.decision} />}
          />
          <div className="p-4 sm:p-5">
            {can('reviewer') ? (
              <ApprovalActions caseId={caseId} onDecided={() => detail.reload()} />
            ) : (
              <p className="text-xs text-muted">
                A reviewer or admin must decide this. Nothing has executed — the action is planned,
                not pre-executed.
              </p>
            )}
          </div>
        </Card>
      ) : null}

      {c.stop_reason ? (
        <Card className="border-block/40">
          <CardHeader title="Stopped" subtitle="The system deliberately stopped pursuing this case." />
          <p className="px-4 py-3 text-sm text-block sm:px-5">{humanize(c.stop_reason)}</p>
        </Card>
      ) : null}

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-3">
        <div className="space-y-4 xl:col-span-2">
          {explanation ? (
            <Card>
              <CardHeader
                title="Why this action"
                subtitle="Reason codes, cited evidence and the controls that applied"
              />
              <ExplanationPanel explanation={explanation} />
            </Card>
          ) : null}

          {/* SRS 16.2: why-at-risk evidence and diagnosis confidence. */}
          <Card>
            <CardHeader
              title="Diagnosis"
              subtitle="Root cause, confidence and the evidence behind it"
              right={
                d.diagnosis ? (
                  <DecidedByBadge source={d.diagnosis.source} model={d.diagnosis.model_name} />
                ) : null
              }
            />
            {d.diagnosis ? (
              <div className="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2 sm:p-5">
                <Detail label="Root cause">
                  {humanize(d.diagnosis.root_cause)}
                  {d.diagnosis.root_cause === 'unknown' ? (
                    <p className="mt-1 text-2xs leading-relaxed text-dim">
                      UNKNOWN is a permitted answer. The agent is instructed to choose it rather than
                      invent a cause it cannot support (SRS 7.2).
                    </p>
                  ) : null}
                </Detail>
                <Detail label="Confidence">
                  <Meter
                    value={d.diagnosis.confidence}
                    tone={d.diagnosis.confidence >= 0.7 ? 'pass' : 'escalate'}
                    width="w-24"
                  />
                </Detail>
                <Detail label="Next step" className="sm:col-span-2">
                  {d.diagnosis.next_step || '—'}
                </Detail>
                <Detail label="Evidence" className="sm:col-span-2">
                  {(d.diagnosis.evidence ?? []).length > 0 ? (
                    <ul className="space-y-1">
                      {(d.diagnosis.evidence ?? []).map((e, i) => (
                        <li key={`${e}-${i}`} className="font-mono text-2xs text-muted">
                          {e}
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <span className="text-2xs text-dim">none cited</span>
                  )}
                </Detail>
                {(d.diagnosis.uncertainty_flags ?? []).length > 0 ? (
                  <Detail label="Uncertainty flags" className="sm:col-span-2">
                    <ul className="space-y-0.5">
                      {(d.diagnosis.uncertainty_flags ?? []).map((f, i) => (
                        <li key={`${f}-${i}`} className="text-xs text-escalate">
                          {f}
                        </li>
                      ))}
                    </ul>
                  </Detail>
                ) : null}
                <Detail label="Latency">{formatLatency(d.diagnosis.latency_ms)}</Detail>
                <Detail label="Recorded">
                  {mounted ? formatDateTime(d.diagnosis.created_at) : '—'}
                </Detail>
              </div>
            ) : (
              <EmptyState
                title="Not diagnosed yet."
                detail="Detection has opened the case; the diagnosis agent has not run or has not completed."
              />
            )}
          </Card>

          {/* SRS 16.2: planner decision and alternatives. */}
          <Card>
            <CardHeader
              title="Planned intervention"
              subtitle="What the planner chose, and what it did not"
              right={
                d.decision ? (
                  <DecidedByBadge source={d.decision.source} model={d.decision.model_name} />
                ) : null
              }
            />
            {d.decision ? (
              <div className="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2 sm:p-5">
                <Detail label="Recommended action">
                  <ActionTypeBadge action={d.decision.recommended_action} />
                </Detail>
                <Detail label="Recovery probability">
                  <Meter value={d.decision.recovery_probability} tone="accent" width="w-24" />
                </Detail>
                <Detail label="Expected recovery">
                  <MoneyText paise={d.decision.expected_recovery} className="text-accent-text" />
                </Detail>
                <Detail label="Policy version">
                  <Mono>{d.decision.policy_version}</Mono>
                </Detail>
                <Detail label="Alternatives considered" className="sm:col-span-2">
                  {(d.decision.alternatives ?? []).length > 0 ? (
                    <div className="flex flex-wrap gap-1.5">
                      {(d.decision.alternatives ?? []).map((a, i) => (
                        <span
                          key={`${a}-${i}`}
                          className="chip border-line-strong bg-ink-700 text-muted"
                        >
                          {humanize(a)}
                        </span>
                      ))}
                    </div>
                  ) : (
                    <span className="text-2xs text-dim">none recorded</span>
                  )}
                </Detail>
                <Detail label="Reason codes" className="sm:col-span-2">
                  {(d.decision.reason_codes ?? []).length > 0 ? (
                    <div className="flex flex-wrap gap-1.5">
                      {(d.decision.reason_codes ?? []).map((r, i) => (
                        <code key={`${r}-${i}`} className="font-mono text-2xs text-muted">
                          {r}
                        </code>
                      ))}
                    </div>
                  ) : (
                    <span className="text-2xs text-dim">none</span>
                  )}
                </Detail>
                <Detail label="Stop condition" className="sm:col-span-2">
                  {d.decision.stop_condition || '—'}
                </Detail>
                <Detail label="Latency">{formatLatency(d.decision.latency_ms)}</Detail>
                <Detail label="Recorded">
                  {mounted ? formatDateTime(d.decision.created_at) : '—'}
                </Detail>
              </div>
            ) : (
              <EmptyState title="No plan yet." />
            )}
          </Card>

          {/* SRS 16.2: policy checks with PASS/BLOCK/ESCALATE labels. */}
          <Card>
            <CardHeader
              title="Policy checks"
              subtitle="Every rule evaluated, in the order the engine applied them. BLOCK beats ESCALATE beats PASS."
            />
            {(d.policy_checks ?? []).length > 0 ? (
              <TableShell
                head={
                  <>
                    <Th>Rule</Th>
                    <Th>Result</Th>
                    <Th>Details</Th>
                    <Th>Version</Th>
                    <Th>When</Th>
                  </>
                }
              >
                {(d.policy_checks ?? []).map((check) => (
                  <tr key={check.id} className="row-hover">
                    <Td>
                      <Mono className="text-body">{check.rule}</Mono>
                    </Td>
                    <Td>
                      <PolicyResultBadge result={check.result} />
                    </Td>
                    <Td>
                      <span className="text-xs text-muted">{check.details || '—'}</span>
                    </Td>
                    <Td>
                      <Mono className="text-dim">{check.policy_version}</Mono>
                    </Td>
                    <Td className="whitespace-nowrap">
                      <Mono className="text-dim">
                        {mounted ? formatDateTime(check.created_at) : '—'}
                      </Mono>
                    </Td>
                  </tr>
                ))}
              </TableShell>
            ) : (
              <EmptyState
                title="No policy checks recorded."
                detail="The policy engine runs after the planner. A case that has not been planned has nothing to check."
              />
            )}
          </Card>

          {/* SRS 16.2: action timeline and external Razorpay resource IDs. */}
          <Card>
            <CardHeader
              title="Recovery actions"
              subtitle="External resource ids are shown so a reviewer can confirm each one in the Razorpay test dashboard"
            />
            {(d.actions ?? []).length > 0 ? (
              <TableShell
                head={
                  <>
                    <Th>Action</Th>
                    <Th>Status</Th>
                    <Th align="right">Amount</Th>
                    <Th>Razorpay resource</Th>
                    <Th>Idempotency key</Th>
                    <Th align="right">Attempts</Th>
                    <Th align="right">Latency</Th>
                    <Th>Executed</Th>
                  </>
                }
              >
                {(d.actions ?? []).map((a) => (
                  <tr key={a.id} className="row-hover align-top">
                    <Td>
                      <ActionTypeBadge action={a.action_type} />
                      <div className="mt-1">
                        <ModeBadge mode={a.mode} />
                      </div>
                    </Td>
                    <Td>
                      <ActionStatusBadge status={a.status} />
                      {a.error_code || a.error_message ? (
                        <p className="mt-1 max-w-[16rem] break-words text-2xs text-block">
                          {a.error_code ? <span className="font-mono">{a.error_code}</span> : null}
                          {a.error_code && a.error_message ? ' · ' : null}
                          {a.error_message}
                        </p>
                      ) : null}
                    </Td>
                    <Td align="right">
                      <MoneyText paise={a.amount} className="text-xs text-body" />
                    </Td>
                    <Td>
                      {a.external_id ? (
                        <>
                          <Mono className="text-body" title={a.external_id}>
                            {a.external_id}
                          </Mono>
                          {a.external_url ? (
                            <p className="mt-0.5">
                              <a
                                href={a.external_url}
                                target="_blank"
                                rel="noreferrer noopener"
                                className="text-2xs hover:underline"
                              >
                                open payment link ↗
                              </a>
                            </p>
                          ) : null}
                        </>
                      ) : (
                        <span className="text-2xs text-dim">
                          {a.status === 'pending' ? 'reserved, not yet called' : 'none'}
                        </span>
                      )}
                    </Td>
                    <Td>
                      <Mono
                        className="text-dim"
                        title={`${a.idempotency_key} — a unique index on this column is what makes a duplicate request a no-op`}
                      >
                        {shortID(a.idempotency_key, 14, 8)}
                      </Mono>
                    </Td>
                    <Td align="right">
                      <span className="tnum text-xs text-muted">{a.attempt_count}</span>
                    </Td>
                    <Td align="right">
                      <span className="tnum text-xs text-muted">{formatLatency(a.latency_ms)}</span>
                    </Td>
                    <Td className="whitespace-nowrap">
                      <Mono className="text-dim">
                        {mounted ? formatDateTime(a.executed_at ?? a.requested_at) : '—'}
                      </Mono>
                    </Td>
                  </tr>
                ))}
              </TableShell>
            ) : (
              <EmptyState
                title="No actions."
                detail="Either nothing has been approved yet, or the planner chose NO_ACTION — which is a decision, not a gap."
              />
            )}
          </Card>

          {/* SRS 16.2: outcome and recovered amount. */}
          <Card>
            <CardHeader
              title="Outcomes"
              subtitle="Recovery is banked only from a verified payment, and only once per action"
            />
            {(d.outcomes ?? []).length > 0 ? (
              <TableShell
                head={
                  <>
                    <Th>Outcome</Th>
                    <Th align="right">Recovered</Th>
                    <Th align="right">Time to recovery</Th>
                    <Th>Verified by</Th>
                    <Th>Action</Th>
                    <Th>Notes</Th>
                  </>
                }
              >
                {(d.outcomes ?? []).map((o) => (
                  <tr key={o.id} className="row-hover">
                    <Td>
                      <OutcomeBadge outcome={o.outcome} />
                    </Td>
                    <Td align="right">
                      <MoneyText
                        paise={o.recovered_amount}
                        className={o.recovered_amount > 0 ? 'text-xs text-recovered' : 'text-xs text-muted'}
                      />
                    </Td>
                    <Td align="right">
                      <span className="tnum text-xs text-muted">
                        {formatMinutes(o.time_to_recovery_seconds / 60)}
                      </span>
                    </Td>
                    <Td>
                      <Mono className="text-muted">{o.verification_source || '—'}</Mono>
                    </Td>
                    <Td>
                      <Mono className="text-dim" title={o.action_id}>
                        {shortID(o.action_id)}
                      </Mono>
                    </Td>
                    <Td>
                      <span className="text-2xs text-muted">{o.notes || '—'}</span>
                    </Td>
                  </tr>
                ))}
              </TableShell>
            ) : (
              <EmptyState title="No outcome yet." />
            )}
          </Card>

          {(d.approvals ?? []).length > 0 ? (
            <Card>
              <CardHeader
                title="Human decisions"
                subtitle="Approval downgrades an ESCALATE to a pass. It never overrides a BLOCK."
              />
              <TableShell
                head={
                  <>
                    <Th>Decision</Th>
                    <Th>Escalation reason</Th>
                    <Th>Reviewer</Th>
                    <Th>Note</Th>
                    <Th>Requested</Th>
                    <Th>Decided</Th>
                  </>
                }
              >
                {(d.approvals ?? []).map((a) => (
                  <tr key={a.id} className="row-hover align-top">
                    <Td>
                      <DecisionBadge decision={a.decision} />
                    </Td>
                    <Td>
                      <span className="text-xs text-muted">{a.reason || '—'}</span>
                    </Td>
                    <Td>
                      <span className="text-xs text-muted">{a.reviewer || '—'}</span>
                    </Td>
                    <Td>
                      <span className="text-xs text-muted">{a.decision_note || '—'}</span>
                    </Td>
                    <Td className="whitespace-nowrap">
                      <Mono className="text-dim">
                        {mounted ? formatDateTime(a.requested_at) : '—'}
                      </Mono>
                    </Td>
                    <Td className="whitespace-nowrap">
                      <Mono className="text-dim">
                        {mounted && a.decided_at ? formatDateTime(a.decided_at) : '—'}
                      </Mono>
                    </Td>
                  </tr>
                ))}
              </TableShell>
            </Card>
          ) : null}
        </div>

        <div className="space-y-4">
          <Card>
            <CardHeader title="Timeline" subtitle="Detection through outcome, in order" />
            {(d.timeline ?? []).length > 0 ? (
              <Timeline items={d.timeline ?? []} />
            ) : (
              <EmptyState title="Nothing recorded yet." />
            )}
          </Card>

          <Card>
            <CardHeader title="Context" subtitle="The records this case was opened against" />
            <div className="space-y-4 p-4 sm:p-5">
              {d.customer ? (
                <div className="space-y-2">
                  <p className="label">Customer</p>
                  <Detail label="Name">{d.customer.name || '—'}</Detail>
                  <Detail label="Email">
                    <span className="break-all text-xs">{d.customer.email || '—'}</span>
                  </Detail>
                  <Detail label="Segment">
                    <SegmentBadge segment={d.customer.segment} />
                  </Detail>
                  <Detail label="Lifetime value">
                    <MoneyText paise={d.customer.lifetime_value} kpi />
                  </Detail>
                  <Detail label="Historical success rate">
                    {formatPercent(d.customer.success_rate)} over{' '}
                    {formatCount(d.customer.total_payments)} payments
                  </Detail>
                </div>
              ) : null}

              {d.transaction ? (
                <div className="space-y-2 border-t border-line pt-4">
                  <p className="label">Transaction</p>
                  <Detail label="Amount">
                    <MoneyText paise={d.transaction.amount} />
                  </Detail>
                  <Detail label="Status">{d.transaction.status}</Detail>
                  {d.transaction.error_code ? (
                    <Detail label="Error code">
                      <Mono className="text-block">{d.transaction.error_code}</Mono>
                    </Detail>
                  ) : null}
                  {d.transaction.failure_reason ? (
                    <Detail label="Failure reason">
                      <span className="text-xs">{d.transaction.failure_reason}</span>
                    </Detail>
                  ) : null}
                  <Detail label="Method">{d.transaction.method || '—'}</Detail>
                  <Detail label="Attempts">{d.transaction.attempt_count}</Detail>
                  {d.transaction.razorpay_payment_id ? (
                    <Detail label="Razorpay payment">
                      <Mono title={d.transaction.razorpay_payment_id}>
                        {d.transaction.razorpay_payment_id}
                      </Mono>
                    </Detail>
                  ) : null}
                  {d.transaction.razorpay_order_id ? (
                    <Detail label="Razorpay order">
                      <Mono title={d.transaction.razorpay_order_id}>
                        {d.transaction.razorpay_order_id}
                      </Mono>
                    </Detail>
                  ) : null}
                </div>
              ) : null}

              {d.checkout_session ? (
                <div className="space-y-2 border-t border-line pt-4">
                  <p className="label">Checkout session</p>
                  <Detail label="Cart amount">
                    <MoneyText paise={d.checkout_session.cart_amount} />
                  </Detail>
                  <Detail label="Items">{d.checkout_session.item_count}</Detail>
                  <Detail label="Page views">{d.checkout_session.page_views}</Detail>
                  <Detail label="Status">{d.checkout_session.status}</Detail>
                  <Detail label="Last activity">
                    {mounted ? formatDateTime(d.checkout_session.last_activity_at) : '—'}
                  </Detail>
                  <p className="text-2xs leading-relaxed text-dim">
                    Abandonment is generated by this project&apos;s demo checkout, not inferred from
                    a Razorpay event that does not exist (SRS 11.2).
                  </p>
                </div>
              ) : null}

              {d.invoice ? (
                <div className="space-y-2 border-t border-line pt-4">
                  <p className="label">Invoice</p>
                  <Detail label="Number">
                    <Mono>{d.invoice.invoice_number}</Mono>
                  </Detail>
                  <Detail label="Amount">
                    <MoneyText paise={d.invoice.amount} />
                  </Detail>
                  <Detail label="Paid">
                    <MoneyText paise={d.invoice.amount_paid} />
                  </Detail>
                  <Detail label="Status">{d.invoice.status}</Detail>
                  <Detail label="Due">{formatDate(d.invoice.due_date)}</Detail>
                  <Detail label="Reminders sent">{d.invoice.reminder_count}</Detail>
                </div>
              ) : null}

              {d.subscription ? (
                <div className="space-y-2 border-t border-line pt-4">
                  <p className="label">Subscription</p>
                  <Detail label="Amount">
                    <MoneyText paise={d.subscription.amount} />
                  </Detail>
                  <Detail label="Status">{d.subscription.status}</Detail>
                  <Detail label="Failed charges">{d.subscription.failed_charge_count}</Detail>
                  <Detail label="Current period ends">
                    {formatDate(d.subscription.current_end)}
                  </Detail>
                </div>
              ) : null}
            </div>
          </Card>

          <Card>
            <CardHeader
              title="State"
              subtitle="Where the case is, and where it may legally go next (SRS 14.2)"
            />
            <div className="space-y-3 p-4 sm:p-5">
              <Detail label="Current">
                <StatusBadge status={c.status} />
              </Detail>
              <Detail label="Permitted next states">
                {(detail.data?.allowed_transitions ?? []).length > 0 ? (
                  <div className="flex flex-wrap gap-1.5">
                    {(detail.data?.allowed_transitions ?? []).map((s) => (
                      <StatusBadge key={s} status={s} />
                    ))}
                  </div>
                ) : (
                  <span className="text-2xs text-dim">
                    Terminal. Nothing further will happen to this case.
                  </span>
                )}
              </Detail>
              <Detail label="Actions taken">{c.action_count}</Detail>
              <Detail label="Case id">
                <Mono title={c.id}>{c.id}</Mono>
              </Detail>
              <Detail label="Created">{mounted ? formatDateTime(c.created_at) : '—'}</Detail>
              <Detail label="Updated">{mounted ? formatDateTime(c.updated_at) : '—'}</Detail>
              {c.closed_at ? (
                <Detail label="Closed">{mounted ? formatDateTime(c.closed_at) : '—'}</Detail>
              ) : null}
            </div>
          </Card>
        </div>
      </div>

      {/* SRS 16.2: full audit log accessible on demand. */}
      <Card>
        <AuditLog caseId={caseId} />
      </Card>
    </>
  );
}
