'use client';

import { useRouter } from 'next/navigation';
import { useState } from 'react';

import { Button, ErrorBanner, TextField } from '@/components/ui';
import { ApiError } from '@/lib/api';
import { useAuth } from '@/lib/auth';

/**
 * The credential form.
 *
 * The password never leaves this component except in the POST body to /api/session,
 * and the token that comes back never reaches the browser at all — it goes into an
 * httpOnly cookie on the server side (SRS 19.5). There is no local storage of
 * anything here.
 *
 * Failures are shown exactly as the API worded them. The Go handler makes "wrong
 * password" and "no such account" identical on purpose so login is not an
 * account-enumeration oracle, and helpfully distinguishing them here would undo that.
 */
export function LoginForm({ next }: { next: string }) {
  const { signIn } = useAuth();
  const router = useRouter();

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ApiError | undefined>(undefined);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setPending(true);
    setError(undefined);
    try {
      await signIn(email.trim(), password);
      router.replace(next);
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, 'error', String(err)));
      setPending(false);
    }
  };

  return (
    <main className="login-shell flex min-h-screen items-center justify-center px-4 py-10">
      <div className="page-enter w-full max-w-md space-y-7">
        <div className="space-y-3 text-center">
          <span
            aria-hidden
            className="mx-auto flex h-12 w-12 items-center justify-center rounded-2xl border border-accent/30 bg-accent-soft shadow-[0_14px_32px_-16px_rgba(103,200,208,0.55)]"
          >
            <svg viewBox="0 0 20 20" className="h-5 w-5 text-accent-text" fill="none">
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
          <p className="text-sm font-semibold tracking-[-0.03em] text-body">Ledgerflow</p>
          <h1 className="text-3xl font-semibold tracking-[-0.045em] text-body sm:text-4xl">
            Bring lost revenue back into view.
          </h1>
          <p className="mx-auto max-w-sm text-sm leading-relaxed text-muted">
            LedgerFlow finds failed payments and overdue invoices, then helps your team recover them with care.
          </p>
        </div>

        <form onSubmit={submit} className="card card-pad space-y-5 border-line-strong/80 bg-ink-800/95 shadow-[0_24px_60px_-30px_rgba(0,0,0,0.95)]">
          <TextField
            label="Email"
            type="email"
            value={email}
            onChange={setEmail}
            placeholder="operator@example.com"
            autoComplete="username"
            required
            error={error?.details['email']}
          />
          <TextField
            label="Password"
            type="password"
            value={password}
            onChange={setPassword}
            placeholder="••••••••"
            autoComplete="current-password"
            required
            error={error?.details['password']}
          />

          <ErrorBanner
            error={error && Object.keys(error.details).length === 0 ? error : undefined}
          />

          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!email || !password}
            className="w-full"
          >
            Sign in
          </Button>
        </form>

        <p className="text-center text-2xs leading-relaxed text-dim">Test environment. Live payments are disabled.</p>
      </div>
    </main>
  );
}
