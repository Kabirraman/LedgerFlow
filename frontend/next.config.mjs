/**
 * Next.js configuration for the LEDGERFLOW operator console.
 *
 * Two things here are load-bearing for deployment rather than cosmetic:
 *
 *   `output: 'standalone'` makes `next build` emit a self-contained server bundle,
 *   which is what lets the Docker image ship without node_modules.
 *
 *   The header block is a second, independent copy of the browser-facing security
 *   headers. The Go API sets its own (internal/httpapi/middleware.go), but the
 *   HTML this app serves never passes through the Go process, so headers set
 *   there would not protect a single page of it (SRS 19.5, 23.4).
 */

/**
 * The browser in this app never talks to anything but its own origin: every API
 * call goes through the server-side proxy in src/app/api/proxy, which is what
 * keeps the session token out of JavaScript and the Razorpay/Gemini credentials
 * out of the bundle entirely (SRS 19.1, 23.4). That makes `connect-src 'self'`
 * an accurate description of the app rather than an aspiration.
 *
 * `script-src` carries 'unsafe-inline' because the App Router serves its
 * hydration payload as inline <script> tags. The alternative is a per-request
 * nonce, which would work but only if it is threaded correctly through every
 * response — and a nonce that is wrong in one code path fails closed, taking the
 * page with it. An honest 'unsafe-inline' with everything else locked down is
 * the better trade for a prototype than a nonce scheme that is not verified end
 * to end.
 */
const contentSecurityPolicy = [
  "default-src 'self'",
  "script-src 'self' 'unsafe-inline'",
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data:",
  "font-src 'self' data:",
  "connect-src 'self'",
  "frame-src 'none'",
  "frame-ancestors 'none'",
  "object-src 'none'",
  "base-uri 'self'",
  "form-action 'self'",
].join('; ');

/** @type {import('next').NextConfig} */
const nextConfig = {
  output: 'standalone',
  reactStrictMode: true,
  // The version banner is served from the API's /api/version instead, which
  // reports the build of the thing that actually moves money.
  poweredByHeader: false,
  eslint: {
    // A lint error should fail `npm run lint` in CI, not silently pass a build.
    ignoreDuringBuilds: false,
  },
  typescript: {
    ignoreBuildErrors: false,
  },
  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          { key: 'Content-Security-Policy', value: contentSecurityPolicy },
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'X-Frame-Options', value: 'DENY' },
          { key: 'Referrer-Policy', value: 'no-referrer' },
          { key: 'Permissions-Policy', value: 'camera=(), microphone=(), geolocation=(), payment=()' },
          // Cross-origin isolation is not needed, but an operator console has no
          // reason to be embeddable or to leak its window reference.
          { key: 'Cross-Origin-Opener-Policy', value: 'same-origin' },
        ],
      },
    ];
  },
};

export default nextConfig;
