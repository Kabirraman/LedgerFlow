import type { Metadata, Viewport } from 'next';

import { AuthProvider } from '@/lib/auth';

import './globals.css';

export const metadata: Metadata = {
  title: 'LEDGERFLOW — Revenue Recovery Console',
  description:
    'Autonomous revenue recovery operating system. Detects at-risk revenue, diagnoses root cause, plans a compliant intervention, and verifies recovery.',
  // This console shows merchant financial state. Nothing about it belongs in a
  // search index or a link preview.
  robots: { index: false, follow: false },
  applicationName: 'LEDGERFLOW',
};

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  themeColor: '#080b12',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className="h-full">
      <body className="min-h-full bg-ink-900 font-sans text-body antialiased">
        <AuthProvider>{children}</AuthProvider>
      </body>
    </html>
  );
}
