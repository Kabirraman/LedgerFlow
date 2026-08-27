/**
 * Server-only plumbing between this app and the LEDGERFLOW API.
 *
 * The design decision that shapes this whole file: the browser never holds the
 * session token and never learns the API's address. It talks to this Next.js
 * server, which reads an httpOnly cookie and attaches the bearer token itself.
 *
 * That is more moving parts than putting the JWT in localStorage, and it buys two
 * things worth the cost. A token in localStorage is readable by any script that
 * gets into the page, and this console can approve money-moving actions — so the
 * blast radius of one XSS is "an attacker approves payments" rather than "an
 * attacker reads a dashboard". And keeping the API address server-side means the
 * deployment topology is not baked into the browser bundle, so the same build runs
 * behind Compose, behind a reverse proxy, or against localhost (SRS 15.1, 19.5).
 *
 * Nothing in this file may be imported from a client component. It reads
 * process.env and cookies, neither of which exists in the browser.
 */

import { cookies } from 'next/headers';

/** The session cookie. Holds the JWT minted by the Go API and nothing else. */
export const SESSION_COOKIE = 'lf_session';

/**
 * How long the cookie lives when the API does not say.
 *
 * The API's login response carries expires_in, and that is what is normally used.
 * This fallback exists so a malformed response produces a short session rather
 * than a session cookie with no expiry at all.
 */
const FALLBACK_SESSION_SECONDS = 60 * 60;

/**
 * The API base URL as seen from this server process.
 *
 * Trailing slashes are stripped so path joining is unambiguous: the difference
 * between one and two slashes is a 404 that looks like a routing bug.
 */
export function backendURL(): string {
  const raw = process.env.LEDGERFLOW_API_URL?.trim();
  if (!raw) {
    // A default rather than a throw. The failure mode of a missing API URL should
    // be "cannot reach the API at localhost:8080", which names the problem, not a
    // 500 during render that names only this file.
    return 'http://localhost:8080';
  }
  return raw.replace(/\/+$/, '');
}

/**
 * Cookie attributes.
 *
 * `secure` is opt-in via LEDGERFLOW_COOKIE_SECURE rather than derived from
 * NODE_ENV. A production build served over plain HTTP behind a local reverse
 * proxy is a real deployment shape, and in that shape a Secure cookie is silently
 * dropped by the browser — presenting as "login succeeds, every page says logged
 * out", which is an expensive hour to debug. Making it explicit means the operator
 * chooses it knowingly.
 *
 * `sameSite: 'lax'` rather than 'strict': strict would drop the cookie on a
 * top-level navigation from an external link, so following a link to a case would
 * bounce the reviewer to the login screen. Lax still withholds the cookie from
 * cross-site POSTs, which is the CSRF vector that matters here.
 */
export function sessionCookieOptions(maxAgeSeconds?: number) {
  return {
    httpOnly: true,
    secure: process.env.LEDGERFLOW_COOKIE_SECURE === '1',
    sameSite: 'lax' as const,
    path: '/',
    maxAge: Math.max(60, Math.floor(maxAgeSeconds ?? FALLBACK_SESSION_SECONDS)),
  };
}

/** Reads the bearer token out of the request's session cookie, if present. */
export async function sessionToken(): Promise<string | undefined> {
  const store = await cookies();
  const value = store.get(SESSION_COOKIE)?.value?.trim();
  return value ? value : undefined;
}

/**
 * Calls the API from the server, attaching the session token when there is one.
 *
 * `cache: 'no-store'` on every call is deliberate. This is an operations console
 * over live financial state; a cached revenue figure is a wrong revenue figure,
 * and the one place a stale read would be least visible is the dashboard, where
 * the numbers look plausible whatever they say.
 */
export async function callBackend(
  path: string,
  init: RequestInit & { token?: string } = {},
): Promise<Response> {
  const { token, headers, ...rest } = init;
  const merged = new Headers(headers);
  if (token) merged.set('Authorization', `Bearer ${token}`);
  if (!merged.has('Accept')) merged.set('Accept', 'application/json');

  return fetch(`${backendURL()}${path}`, {
    ...rest,
    headers: merged,
    cache: 'no-store',
    redirect: 'manual',
  });
}

/**
 * The error envelope this app produces when it cannot reach the API at all.
 *
 * It has the same shape as the Go API's error body (internal/httpapi/respond.go)
 * so that client code has exactly one error shape to handle. A second shape would
 * mean every call site needs two unwrapping paths, and the one that gets forgotten
 * is the one that renders "[object Object]" to an operator.
 */
export function gatewayError(message: string, code = 'api_unreachable') {
  return Response.json({ error: { code, message } }, { status: 502 });
}
