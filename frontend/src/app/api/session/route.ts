/**
 * Session routes: log in, log out, read the current identity.
 *
 * This is the only place the JWT is handled. It arrives from the API's
 * /api/auth/login, goes straight into an httpOnly cookie, and is never returned to
 * the browser in a response body — so no client component can read it, log it, or
 * put it in a URL (SRS 19.5).
 */

import { NextRequest } from 'next/server';

import {
  SESSION_COOKIE,
  callBackend,
  gatewayError,
  sessionCookieOptions,
  sessionToken,
} from '@/lib/server/backend';

export const dynamic = 'force-dynamic';

interface LoginBody {
  email?: unknown;
  password?: unknown;
}

/**
 * POST /api/session — exchange credentials for a session cookie.
 *
 * The response body is deliberately just the user. Every failure is passed through
 * with the API's own status and message, unmodified: the Go handler is careful to
 * make "wrong password" and "no such account" byte-identical so login is not an
 * account-enumeration oracle, and rewriting the message here would undo that.
 */
export async function POST(req: NextRequest) {
  let body: LoginBody;
  try {
    body = (await req.json()) as LoginBody;
  } catch {
    return Response.json(
      { error: { code: 'invalid_body', message: 'expected a JSON object with email and password' } },
      { status: 400 },
    );
  }

  const email = typeof body.email === 'string' ? body.email.trim() : '';
  const password = typeof body.password === 'string' ? body.password : '';
  if (!email || !password) {
    return Response.json(
      {
        error: {
          code: 'validation_failed',
          message: 'email and password are required',
          details: {
            ...(email ? {} : { email: 'an email address is required' }),
            ...(password ? {} : { password: 'a password is required' }),
          },
        },
      },
      { status: 400 },
    );
  }

  let upstream: Response;
  try {
    upstream = await callBackend('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password }),
    });
  } catch {
    return gatewayError('the LEDGERFLOW API is not reachable from the web server');
  }

  const text = await upstream.text();
  if (!upstream.ok) {
    // Pass the API's verdict through untouched, including its status code.
    return new Response(text, {
      status: upstream.status,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  let parsed: { token?: { access_token?: string; expires_in?: number }; user?: unknown };
  try {
    parsed = JSON.parse(text);
  } catch {
    return gatewayError('the API returned a login response this app could not read', 'bad_upstream');
  }

  const accessToken = parsed.token?.access_token;
  if (!accessToken) {
    return gatewayError('the API returned a login response with no access token', 'bad_upstream');
  }

  const res = Response.json({ user: parsed.user ?? null });
  // Set-Cookie via the header rather than cookies().set(): a Response built by
  // Response.json is not the mutable NextResponse the cookie helper expects.
  const opts = sessionCookieOptions(parsed.token?.expires_in);
  res.headers.append('Set-Cookie', serializeCookie(SESSION_COOKIE, accessToken, opts));
  return res;
}

/**
 * GET /api/session — who am I?
 *
 * Answered by asking the API, not by decoding the JWT here. A token this server
 * can decode is not the same as a token the API still accepts: the user may have
 * been deleted, the signing secret rotated, or the role changed. Asking is one
 * round trip and it cannot be wrong.
 */
export async function GET() {
  const token = await sessionToken();
  if (!token) {
    return Response.json({ user: null }, { status: 200 });
  }

  let upstream: Response;
  try {
    upstream = await callBackend('/api/auth/me', { token });
  } catch {
    return gatewayError('the LEDGERFLOW API is not reachable from the web server');
  }

  if (upstream.status === 401 || upstream.status === 403) {
    // The cookie exists but the API rejects it. Clear it, so the client stops
    // presenting a token that will fail on every subsequent request.
    const res = Response.json({ user: null }, { status: 200 });
    res.headers.append('Set-Cookie', clearedCookie());
    return res;
  }
  if (!upstream.ok) {
    const text = await upstream.text();
    return new Response(text, {
      status: upstream.status,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  const body = (await upstream.json()) as { user?: unknown; permissions?: unknown };
  return Response.json({ user: body.user ?? null, permissions: body.permissions ?? null });
}

/**
 * DELETE /api/session — log out.
 *
 * Clearing the cookie is the whole of it. The JWT itself stays valid until it
 * expires, which is a real limitation of stateless tokens and the reason the API
 * issues short-lived ones (SRS 15.1). A server-side deny-list would close that
 * window and is the right next step if these tokens ever get long-lived.
 */
export async function DELETE() {
  const res = Response.json({ ok: true });
  res.headers.append('Set-Cookie', clearedCookie());
  return res;
}

function clearedCookie(): string {
  return serializeCookie(SESSION_COOKIE, '', { ...sessionCookieOptions(60), maxAge: 0 });
}

/**
 * Serialises a Set-Cookie header.
 *
 * Hand-rolled because the value is a JWT — `[A-Za-z0-9._-]` and nothing else — so
 * there is no escaping to get wrong, and pulling in a cookie library to join five
 * strings would be the larger risk.
 */
function serializeCookie(
  name: string,
  value: string,
  opts: { httpOnly: boolean; secure: boolean; sameSite: 'lax'; path: string; maxAge: number },
): string {
  const parts = [
    `${name}=${value}`,
    `Path=${opts.path}`,
    `Max-Age=${opts.maxAge}`,
    `SameSite=${opts.sameSite === 'lax' ? 'Lax' : 'Strict'}`,
  ];
  if (opts.httpOnly) parts.push('HttpOnly');
  if (opts.secure) parts.push('Secure');
  return parts.join('; ');
}
