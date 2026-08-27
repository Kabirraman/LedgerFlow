/**
 * The API proxy.
 *
 * Every call the browser makes goes through here: `/api/proxy/cases` becomes
 * `GET {LEDGERFLOW_API_URL}/api/cases` with the session cookie's bearer token
 * attached. The browser therefore needs no token, no API address and no CORS
 * grant — every request it makes is same-origin.
 *
 * This proxy grants no privilege of its own. It attaches the caller's token and
 * nothing else, and the Go API enforces role gating on every route it serves
 * (internal/httpapi/httpapi.go), so a reviewer cannot reach an admin route by
 * asking this proxy nicely. That property is worth stating explicitly, because a
 * proxy that held its own service credential would be exactly the confused deputy
 * this shape is often mistaken for.
 */

import { NextRequest } from 'next/server';

import { SESSION_COOKIE, backendURL, gatewayError, sessionToken } from '@/lib/server/backend';

export const dynamic = 'force-dynamic';

/**
 * A simulation over 200 synthetic cases with the model enabled can take a while,
 * and the run is not resumable — a proxy that gave up early would leave the
 * operator watching a spinner for work that completed.
 */
export const maxDuration = 300;

/**
 * Paths the browser may not reach through this proxy.
 *
 * The webhook endpoint authenticates by HMAC over the raw body, not by bearer
 * token, so proxying it would achieve nothing except to offer a browser a route to
 * a signature-checked endpoint. Refusing it here keeps the reachable surface equal
 * to the surface this console actually uses.
 */
const DENIED_PREFIXES = ['webhooks'];

async function forward(req: NextRequest, segments: string[]): Promise<Response> {
  if (segments.length === 0) {
    return Response.json(
      { error: { code: 'not_found', message: 'no such endpoint' } },
      { status: 404 },
    );
  }
  if (segments.some((s) => s === '..' || s === '.')) {
    // Path traversal cannot escape the API's own routing, but a request shaped like
    // an attempt is not a request this console makes.
    return Response.json(
      { error: { code: 'invalid_path', message: 'that path is not well formed' } },
      { status: 400 },
    );
  }
  if (DENIED_PREFIXES.includes(segments[0]!)) {
    return Response.json(
      {
        error: {
          code: 'forbidden',
          message: 'that endpoint is not reachable from the web console',
        },
      },
      { status: 403 },
    );
  }

  const token = await sessionToken();
  const search = req.nextUrl.search;
  const target = `${backendURL()}/api/${segments.map(encodeURIComponent).join('/')}${search}`;

  const headers = new Headers();
  headers.set('Accept', 'application/json');
  if (token) headers.set('Authorization', `Bearer ${token}`);

  const contentType = req.headers.get('content-type');
  let body: string | undefined;
  if (req.method !== 'GET' && req.method !== 'HEAD') {
    // Buffered rather than streamed. Every payload this console sends is a small
    // JSON object, and a streamed request body in Node's fetch needs duplex
    // negotiation that buys nothing at this size.
    body = await req.text();
    if (body && contentType) headers.set('Content-Type', contentType);
  }

  let upstream: Response;
  try {
    upstream = await fetch(target, {
      method: req.method,
      headers,
      body,
      cache: 'no-store',
      redirect: 'manual',
    });
  } catch {
    return gatewayError('the LEDGERFLOW API is not reachable from the web server');
  }

  const text = await upstream.text();
  const res = new Response(text, {
    status: upstream.status,
    headers: {
      'Content-Type': upstream.headers.get('content-type') ?? 'application/json',
      // No caching layer between an operator and a revenue figure.
      'Cache-Control': 'no-store',
    },
  });

  // The API's request id is how a UI error is matched to a server log line, so it
  // is worth carrying back out.
  const requestID = upstream.headers.get('x-request-id');
  if (requestID) res.headers.set('X-Request-Id', requestID);

  if (upstream.status === 401) {
    // The token is gone or no longer accepted. Drop the cookie in the same response
    // that reports the failure, so the client does not keep retrying with it.
    res.headers.append(
      'Set-Cookie',
      `${SESSION_COOKIE}=; Path=/; Max-Age=0; SameSite=Lax; HttpOnly${
        process.env.LEDGERFLOW_COOKIE_SECURE === '1' ? '; Secure' : ''
      }`,
    );
  }
  return res;
}

type RouteContext = { params: Promise<{ path?: string[] }> };

async function handler(req: NextRequest, ctx: RouteContext): Promise<Response> {
  const { path } = await ctx.params;
  return forward(req, path ?? []);
}

export const GET = handler;
export const POST = handler;
export const PUT = handler;
export const PATCH = handler;
export const DELETE = handler;
