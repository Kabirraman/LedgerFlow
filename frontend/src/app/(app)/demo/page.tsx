'use client';

/**
 * Demo checkout (SRS 11.2, Workflow B).
 *
 * Razorpay has no "abandoned checkout" event, so LEDGERFLOW generates that signal
 * from a controlled demo checkout instead of pretending to infer it from something
 * that does not exist. This screen is that checkout: start a cart, optionally record
 * activity to keep it alive, then either abandon it (which opens a risk case through
 * the same Detection → Diagnosis → Planner → Policy pipeline as every other
 * workflow) or convert it (which closes it as a non-event).
 *
 * The amount is entered in rupees and converted to paise once, here, before it ever
 * reaches the API — every value downstream of this form is paise, per the Money
 * convention in lib/types.
 */

import { useState } from 'react';

import {
  Button,
  Card,
  CardHeader,
  Detail,
  ErrorBanner,
  Mono,
  MoneyText,
  PageHeader,
  TextField,
} from '@/components/ui';
import { api } from '@/lib/api';
import { cn, formatCount, formatDateTime } from '@/lib/format';
import { useMutation } from '@/lib/hooks';
import type { CheckoutAbandonResponse, CheckoutStartResponse } from '@/lib/types';

export default function DemoCheckoutPage() {
  const [email, setEmail] = useState('');
  const [name, setName] = useState('');
  const [contact, setContact] = useState('');
  const [cartRupees, setCartRupees] = useState('');
  const [itemCount, setItemCount] = useState('2');

  const [session, setSession] = useState<CheckoutStartResponse | undefined>(undefined);
  const [abandonResult, setAbandonResult] = useState<CheckoutAbandonResponse | undefined>(undefined);
  const [converted, setConverted] = useState(false);
  const [activityCount, setActivityCount] = useState(0);

  const start = useMutation(api.startCheckout);
  const activity = useMutation(api.checkoutActivity);
  const abandon = useMutation(api.abandonCheckout);
  const convert = useMutation(api.convertCheckout);

  const rupees = Number.parseFloat(cartRupees);
  const validAmount = Number.isFinite(rupees) && rupees > 0;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!validAmount) return;
    const result = await start.run({
      email: email.trim(),
      name: name.trim() || undefined,
      contact: contact.trim() || undefined,
      cart_amount: Math.round(rupees * 100),
      item_count: Number.parseInt(itemCount, 10) || undefined,
    });
    if (result) {
      setSession(result);
      setAbandonResult(undefined);
      setConverted(false);
      setActivityCount(0);
    }
  };

  const recordActivity = async () => {
    if (!session) return;
    const result = await activity.run(session.session.id);
    if (result) {
      setSession({ ...session, session: result.session });
      setActivityCount((n) => n + 1);
    }
  };

  const doAbandon = async () => {
    if (!session) return;
    const result = await abandon.run(session.session.id);
    if (result) setAbandonResult(result);
  };

  const doConvert = async () => {
    if (!session) return;
    const result = await convert.run(session.session.id);
    if (result) setConverted(true);
  };

  const sessionClosed = Boolean(abandonResult) || converted;

  return (
    <>
      <PageHeader
        title="Demo checkout"
        description="A controlled cart used to generate a checkout-abandonment signal, since Razorpay does not emit one. Nothing here creates a payment — it only opens a case through the normal pipeline once you choose to abandon it."
      />

      <Card>
        <CardHeader title="Start a cart" subtitle="Creates or reuses a customer by email, then a checkout session." />
        <form onSubmit={submit} className="grid grid-cols-1 gap-3 p-4 sm:grid-cols-2 sm:p-5 lg:grid-cols-4">
          <TextField
            label="Customer email"
            type="email"
            value={email}
            onChange={setEmail}
            placeholder="customer@example.com"
            required
          />
          <TextField label="Name" value={name} onChange={setName} placeholder="Optional" />
          <TextField label="Contact" value={contact} onChange={setContact} placeholder="Optional phone" />
          <TextField
            label="Cart amount (₹)"
            type="number"
            min={1}
            step="0.01"
            value={cartRupees}
            onChange={setCartRupees}
            placeholder="1499.00"
            required
          />
          <TextField
            label="Item count"
            type="number"
            min={1}
            value={itemCount}
            onChange={setItemCount}
            className="lg:col-span-1"
          />
          <div className="sm:col-span-2 lg:col-span-4">
            <Button type="submit" variant="primary" pending={start.pending} disabled={!email.trim() || !validAmount}>
              Start checkout
            </Button>
          </div>
        </form>
        <ErrorBanner error={start.error} className="mx-4 mb-4 sm:mx-5" />
      </Card>

      {session ? (
        <Card>
          <CardHeader
            title="Active cart"
            subtitle={`Customer: ${session.customer.name || session.customer.email || session.customer.id}`}
            right={
              <span
                className={cn(
                  'chip',
                  sessionClosed
                    ? converted
                      ? 'border-pass/30 bg-pass-soft text-pass'
                      : 'border-escalate/30 bg-escalate-soft text-escalate'
                    : 'border-line-strong bg-ink-700 text-muted',
                )}
              >
                {sessionClosed ? (converted ? 'converted' : 'abandoned') : 'active'}
              </span>
            }
          />

          <div className="grid grid-cols-2 gap-4 p-4 sm:grid-cols-4 sm:p-5">
            <Detail label="Cart amount">
              <MoneyText paise={session.session.cart_amount} />
            </Detail>
            <Detail label="Items">{formatCount(session.session.item_count)}</Detail>
            <Detail label="Page views">{formatCount(session.session.page_views)}</Detail>
            <Detail label="Started">{formatDateTime(session.session.started_at)}</Detail>
            <Detail label="Last activity">{formatDateTime(session.session.last_activity_at)}</Detail>
            <Detail label="Session id">
              <Mono title={session.session.id}>{session.session.id}</Mono>
            </Detail>
          </div>

          <div className="flex flex-wrap items-center gap-2 border-t border-line px-4 py-3 sm:px-5">
            <Button onClick={() => void recordActivity()} pending={activity.pending} disabled={sessionClosed}>
              Record activity{activityCount > 0 ? ` (${activityCount})` : ''}
            </Button>
            <Button
              variant="reject"
              onClick={() => void doAbandon()}
              pending={abandon.pending}
              disabled={sessionClosed}
              title="Marks the cart abandoned and opens a risk case through the normal pipeline."
            >
              Abandon cart
            </Button>
            <Button
              variant="approve"
              onClick={() => void doConvert()}
              pending={convert.pending}
              disabled={sessionClosed}
              title="Marks the cart converted. No case is opened."
            >
              Convert (customer paid)
            </Button>
          </div>
          <ErrorBanner error={activity.error || abandon.error || convert.error} className="mx-4 mb-4 sm:mx-5" />
        </Card>
      ) : null}

      {abandonResult ? (
        <Card>
          <CardHeader title="Abandonment recorded" />
          <div className="grid grid-cols-2 gap-4 p-4 sm:grid-cols-3 sm:p-5">
            <Detail label="Case created">{abandonResult.case_created ? 'Yes' : 'No — one already existed'}</Detail>
            <Detail label="Case reference">
              {abandonResult.case_id ? (
                <a href={`/cases/${abandonResult.case_id}`} className="font-mono text-xs hover:underline">
                  {abandonResult.case_reference}
                </a>
              ) : (
                '—'
              )}
            </Detail>
            <Detail label="Reason">{abandonResult.reason || '—'}</Detail>
          </div>
        </Card>
      ) : null}
    </>
  );
}
