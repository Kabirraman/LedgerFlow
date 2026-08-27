/**
 * Edge middleware: keeps signed-out readers off the console shell.
 *
 * This checks only that a session cookie is *present*. It does not validate the JWT,
 * because doing that here would mean either a round trip to the API on every
 * navigation or a second copy of the signing secret at the edge — and neither buys
 * anything: the API validates the token on every request it serves, so a forged or
 * expired cookie gets a reader to an empty shell that immediately 401s and bounces
 * back to /login.
 *
 * So this is a redirect for the common case (no cookie → go sign in), not a security
 * boundary. The boundary is internal/httpapi/middleware.go.
 */

import { NextResponse } from 'next/server';
import type { NextRequest } from 'next/server';

const SESSION_COOKIE = 'lf_session';

/** Reachable without a session. Everything else under / requires one. */
const PUBLIC_PATHS = ['/login'];

export function middleware(req: NextRequest) {
  const { pathname, search } = req.nextUrl;

  const hasSession = Boolean(req.cookies.get(SESSION_COOKIE)?.value);
  const isPublic = PUBLIC_PATHS.some((p) => pathname === p || pathname.startsWith(`${p}/`));

  if (!hasSession && !isPublic) {
    const url = req.nextUrl.clone();
    url.pathname = '/login';
    // Carry the intended destination so a reader who followed a link to a specific
    // case lands on that case after signing in, rather than on the dashboard.
    url.search = pathname === '/' ? '' : `?next=${encodeURIComponent(pathname + search)}`;
    return NextResponse.redirect(url);
  }

  if (hasSession && isPublic) {
    const url = req.nextUrl.clone();
    url.pathname = '/dashboard';
    url.search = '';
    return NextResponse.redirect(url);
  }

  return NextResponse.next();
}

export const config = {
  /**
   * Skips /api (the session and proxy routes answer for themselves — a redirect to
   * an HTML login page in response to a fetch is how a JSON client ends up parsing
   * markup), Next's own assets, and static files.
   */
  matcher: ['/((?!api|_next/static|_next/image|favicon.ico|robots.txt|.*\\.(?:svg|png|jpg|ico)$).*)'],
};
