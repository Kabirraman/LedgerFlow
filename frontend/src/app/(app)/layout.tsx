'use client';

/**
 * The console shell.
 *
 * Holds the navigation, the identity badge and the deployment banner. Three things
 * are deliberate here:
 *
 *   1. Nav links are filtered by role. That is a courtesy, not a control — the API
 *      gates every route (SRS 15.2) — but showing a reviewer an admin link they can
 *      only bounce off is a worse experience than not showing it.
 *   2. The banner states the environment and whether Razorpay and the model are
 *      actually configured. A demo that silently ran without a gateway, or without a
 *      model, would look identical to one that did not, and SRS 25.2 is about not
 *      letting that ambiguity stand.
 *   3. Nothing renders until the session resolves. A flash of the shell followed by a
 *      redirect to /login reads as a bug.
 */

import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { useEffect } from 'react';
import type { ReactNode } from 'react';

import { RoleBadge } from '@/components/badges';
import { Button, Spinner } from '@/components/ui';
import { api } from '@/lib/api';
import { useAuth } from '@/lib/auth';
import { cn } from '@/lib/format';
import { useApi } from '@/lib/hooks';
import type { Role } from '@/lib/types';

interface NavItem {
  href: string;
  label: string;
  minRole: Role;
  /** What the screen is for, one line. Shown as a tooltip. */
  hint: string;
  /** The SRS section this screen satisfies, so a reviewer can trace it. */
  srs: string;
}

const NAV: NavItem[] = [
  {
    href: '/dashboard',
    label: 'Dashboard',
    minRole: 'operator',
    hint: 'Revenue at risk, recovered, recovery rate and the recovery funnel.',
    srs: '16.1',
  },
  {
    href: '/cases',
    label: 'Cases',
    minRole: 'operator',
    hint: 'Every at-risk case, filterable by scenario, segment, risk and action.',
    srs: '16.2',
  },
  {
    href: '/approvals',
    label: 'Approvals',
    minRole: 'reviewer',
    hint: 'Cases awaiting human approval, highest value and lowest confidence first.',
    srs: '16.3',
  },
  {
    href: '/simulations',
    label: 'Simulation Lab',
    minRole: 'operator',
    hint: 'Run the versioned benchmark and compare LEDGERFLOW against a baseline.',
    srs: '16.4',
  },
  {
    href: '/analytics',
    label: 'Strategy performance',
    minRole: 'operator',
    hint: 'Which action works for which segment, with sample sizes shown.',
    srs: '10.4',
  },
  {
    href: '/demo',
    label: 'Demo checkout',
    minRole: 'operator',
    hint: 'Generate a checkout-abandonment event from a controlled demo checkout.',
    srs: '11.2',
  },
  {
    href: '/ops',
    label: 'Operations',
    minRole: 'operator',
    hint: 'Webhook intake, duplicate suppression, agent fallbacks and latencies.',
    srs: '18.3',
  },
  {
    href: '/policies',
    label: 'Policies',
    minRole: 'admin',
    hint: 'The guardrails every action is checked against, and their version history.',
    srs: '15.2',
  },
];

export default function AppLayout({ children }: { children: ReactNode }) {
  const { user, loading, signOut, can } = useAuth();
  const router = useRouter();
  const pathname = usePathname();

  useEffect(() => {
    if (!loading && !user) router.replace('/login');
  }, [loading, user, router]);

  if (loading || !user) {
    return (
      <div className="flex min-h-screen items-center justify-center gap-2 text-muted">
        <Spinner />
        <span className="text-xs">Checking your session…</span>
      </div>
    );
  }

  const items = NAV.filter((item) => can(item.minRole));

  return (
    <div className="min-h-screen lg:grid lg:grid-cols-[15rem_1fr]">
      <aside className="border-b border-line bg-ink-800 lg:sticky lg:top-0 lg:h-screen lg:overflow-y-auto lg:border-b-0 lg:border-r">
        <div className="flex items-center gap-2 px-4 py-4">
          <Mark />
          <div className="min-w-0">
            <p className="text-sm font-semibold tracking-tight text-body">LEDGERFLOW</p>
            <p className="text-2xs text-dim">Revenue recovery OS</p>
          </div>
        </div>

        <nav className="px-2 pb-3" aria-label="Console">
          <ul className="space-y-0.5">
            {items.map((item) => {
              const active = pathname === item.href || pathname.startsWith(`${item.href}/`);
              return (
                <li key={item.href}>
                  <Link
                    href={item.href}
                    title={item.hint}
                    aria-current={active ? 'page' : undefined}
                    className={cn(
                      'flex items-center justify-between gap-2 rounded-lg px-3 py-2 text-sm transition-colors',
                      active
                        ? 'bg-accent-soft text-accent-text'
                        : 'text-muted hover:bg-ink-700 hover:text-body',
                    )}
                  >
                    <span className="truncate">{item.label}</span>
                    <span className="shrink-0 font-mono text-2xs text-dim">{item.srs}</span>
                  </Link>
                </li>
              );
            })}
          </ul>
        </nav>

        <div className="border-t border-line px-4 py-3">
          <div className="flex items-center justify-between gap-2">
            <div className="min-w-0">
              <p className="truncate text-xs font-medium text-body">{user.name || user.email}</p>
              <p className="truncate text-2xs text-dim">{user.email}</p>
            </div>
            <RoleBadge role={user.role} />
          </div>
          <Button onClick={() => void signOut()} className="mt-2 w-full px-2 py-1.5 text-xs">
            Sign out
          </Button>
        </div>

        <DeploymentPanel />
      </aside>

      <main className="min-w-0 px-4 py-6 sm:px-6 lg:px-8">
        <div className="mx-auto max-w-[92rem] space-y-6">{children}</div>
      </main>
    </div>
  );
}

function Mark() {
  return (
    <span
      aria-hidden
      className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border border-accent/30 bg-accent-soft"
    >
      <svg viewBox="0 0 20 20" className="h-4 w-4 text-accent-text" fill="none">
        <path
          d="M3 14.5 7.5 9l3.5 3.5L17 5.5"
          stroke="currentColor"
          strokeWidth="1.8"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <circle cx="17" cy="5.5" r="1.6" fill="currentColor" />
      </svg>
    </span>
  );
}

/**
 * What this deployment actually is.
 *
 * Reads /api/version, which reports the environment, the Razorpay mode, whether a
 * gateway and a model are configured, and `live_mode_supported: false`. Rendering it
 * in the shell means the answer to "was this a real test-mode run?" is always on
 * screen instead of being something a demo viewer has to take on trust.
 */
function DeploymentPanel() {
  const { data, error } = useApi('version', (signal) => api.version(signal), {
    pollMs: 120_000,
  });

  if (error) {
    return (
      <div className="border-t border-line px-4 py-3">
        <p className="text-2xs text-block">Deployment details unavailable.</p>
      </div>
    );
  }

  return (
    <div className="space-y-2 border-t border-line px-4 py-3">
      <p className="label">Deployment</p>
      {!data ? (
        <div className="skeleton h-16 w-full" />
      ) : (
        <dl className="space-y-1 text-2xs">
          <Row label="Build" value={data.version} mono />
          <Row label="Environment" value={data.environment} />
          <Row
            label="Razorpay"
            value={
              data.razorpay_configured
                ? `${data.razorpay_mode} mode${data.gateway_external ? '' : ' · no external calls'}`
                : 'not configured'
            }
            tone={data.razorpay_configured ? 'ok' : 'warn'}
          />
          <Row
            label="Model"
            value={data.model_configured ? data.model : 'not configured — deterministic only'}
            tone={data.model_configured ? 'ok' : 'warn'}
          />
          <Row
            label="Auto-execute"
            value={data.auto_execute ? 'on' : 'off — approved actions wait'}
          />
          <Row
            label="Live mode"
            value={data.live_mode_supported ? 'supported' : 'out of scope'}
            tone="ok"
          />
        </dl>
      )}
      <p className="pt-1 text-2xs leading-relaxed text-dim">
        Live-mode monetary transactions are out of scope. This build refuses to boot with a live
        Razorpay key (SRS 5.2).
      </p>
    </div>
  );
}

function Row({
  label,
  value,
  mono,
  tone,
}: {
  label: string;
  value: string;
  mono?: boolean;
  tone?: 'ok' | 'warn';
}) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <dt className="shrink-0 text-dim">{label}</dt>
      <dd
        className={cn(
          'min-w-0 truncate text-right',
          mono && 'font-mono',
          tone === 'warn' ? 'text-escalate' : tone === 'ok' ? 'text-pass' : 'text-muted',
        )}
        title={value}
      >
        {value}
      </dd>
    </div>
  );
}
