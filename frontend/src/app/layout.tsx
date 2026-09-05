import type { Metadata, Viewport } from 'next';
import './globals.css';
import { AuthProvider } from '@/lib/auth';
import { IBM_Plex_Sans } from 'next/font/google';
const ibmPlexSans = IBM_Plex_Sans({
  subsets: ['latin'],
  weight: ['400', '500', '600', '700'],
  variable: '--font-ibm-plex-sans',
});

export const metadata: Metadata = {
  title: 'Ledgerflow | Revenue recovery',
  description:
    'Autonomous revenue recovery operating system. Detects at-risk revenue, diagnoses root cause, plans a compliant intervention, and verifies recovery.',
  // This console shows merchant financial state. Nothing about it belongs in a search index or a link preview.
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
      <body className={ibmPlexSans.className}>
        <AuthProvider>{children}</AuthProvider>
      </body>
    </html>
  );
}
