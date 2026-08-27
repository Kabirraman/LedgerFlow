import { LoginForm } from './login-form';

/**
 * Sign in.
 *
 * A server component so the `next` parameter can be read and sanitised before any
 * client code sees it. Reading it with useSearchParams in a client page would work
 * too, but it would mean either a Suspense boundary or an unsanitised value reaching
 * a router call — and an unvalidated redirect target is how a login page becomes an
 * open redirect.
 */
export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<{ next?: string | string[] }>;
}) {
  const params = await searchParams;
  const raw = Array.isArray(params.next) ? params.next[0] : params.next;
  return <LoginForm next={safeNext(raw)} />;
}

/**
 * Only same-origin absolute paths survive. `//evil.example` is a protocol-relative
 * URL that a browser treats as another origin, so a leading double slash is rejected
 * along with anything that does not start with a single slash.
 */
function safeNext(value: string | undefined): string {
  if (!value) return '/dashboard';
  if (!value.startsWith('/')) return '/dashboard';
  if (value.startsWith('//')) return '/dashboard';
  if (value.startsWith('/login')) return '/dashboard';
  return value;
}
