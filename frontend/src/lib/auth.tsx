'use client';

/**
 * Session context.
 *
 * One place holds "who is signed in and what may they do", because the answer gates
 * navigation, buttons and whole screens. The role check is duplicated deliberately:
 * the Go API enforces it on every route (internal/httpapi/middleware.go) and this is
 * only about not showing an operator a button that would 403. Hiding a control is a
 * courtesy; refusing the request is the control.
 */

import { useRouter } from 'next/navigation';
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import { fetchSession, login as postLogin, logout as postLogout } from './api';
import type { Role, SessionUser } from './types';
import { permits } from './types';

export interface Permissions {
  can_operate: boolean;
  can_review: boolean;
  can_admin: boolean;
}

interface AuthState {
  user: SessionUser | null;
  permissions: Permissions;
  /** True until the first session lookup resolves. Renders neither the app nor the login form. */
  loading: boolean;
  signIn: (email: string, password: string) => Promise<SessionUser>;
  signOut: () => Promise<void>;
  /** Called when a request comes back 401, to drop local state and send the reader to /login. */
  onUnauthenticated: () => void;
  /** Role gate for conditional UI. Never the only gate on an action. */
  can: (role: Role) => boolean;
}

const NO_PERMISSIONS: Permissions = { can_operate: false, can_review: false, can_admin: false };

const AuthContext = createContext<AuthState | null>(null);

/**
 * Derives permissions from the role when the API did not supply them.
 *
 * /api/auth/me returns its own permissions block, which is authoritative and used
 * when present. This fallback covers the login response, which returns the user
 * only. Both agree because both come from the same role ordering
 * (operator < reviewer < admin, SRS 15.1).
 */
function permissionsFor(user: SessionUser | null): Permissions {
  if (!user) return NO_PERMISSIONS;
  return {
    can_operate: permits(user.role, 'operator'),
    can_review: permits(user.role, 'reviewer'),
    can_admin: permits(user.role, 'admin'),
  };
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const router = useRouter();
  const [user, setUser] = useState<SessionUser | null>(null);
  const [permissions, setPermissions] = useState<Permissions>(NO_PERMISSIONS);
  const [loading, setLoading] = useState(true);

  // Guards against a burst of 401s — one page can have three panels in flight —
  // turning into three redirects.
  const redirecting = useRef(false);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;

    void (async () => {
      try {
        const res = await fetchSession(controller.signal);
        if (cancelled) return;
        setUser(res.user);
        setPermissions(res.permissions ?? permissionsFor(res.user));
      } catch {
        // A failed session lookup means "not signed in as far as this app can
        // tell". The login screen is the honest response; pretending to be signed
        // in would produce a dashboard of errors.
        if (!cancelled) {
          setUser(null);
          setPermissions(NO_PERMISSIONS);
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, []);

  const signIn = useCallback(async (email: string, password: string) => {
    const next = await postLogin(email, password);
    setUser(next);
    setPermissions(permissionsFor(next));
    redirecting.current = false;
    return next;
  }, []);

  const signOut = useCallback(async () => {
    await postLogout();
    setUser(null);
    setPermissions(NO_PERMISSIONS);
    router.replace('/login');
  }, [router]);

  const onUnauthenticated = useCallback(() => {
    if (redirecting.current) return;
    redirecting.current = true;
    setUser(null);
    setPermissions(NO_PERMISSIONS);
    router.replace('/login');
  }, [router]);

  const can = useCallback((role: Role) => permits(user?.role, role), [user]);

  const value = useMemo<AuthState>(
    () => ({ user, permissions, loading, signIn, signOut, onUnauthenticated, can }),
    [user, permissions, loading, signIn, signOut, onUnauthenticated, can],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used inside <AuthProvider>');
  return ctx;
}
