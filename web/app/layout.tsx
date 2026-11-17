import type { Metadata } from 'next';
import { Inter, JetBrains_Mono } from 'next/font/google';
import { Nav } from '@/components/Nav';
import { authConfig } from '@/lib/auth';
import './globals.css';

// Self-hosted by next/font at build time: no requests to Google at runtime.
const sans = Inter({ subsets: ['latin'], variable: '--font-sans', display: 'swap' });
const mono = JetBrains_Mono({ subsets: ['latin'], variable: '--font-mono', display: 'swap' });

export const metadata: Metadata = {
  title: 'Bindle',
  description: 'Observability for AI agents',
  robots: { index: false, follow: false },
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${sans.variable} ${mono.variable}`}>
      <body>
        <div className="shell">
          <Nav signOut={authConfig() !== null} />
          <main className="main">{children}</main>
        </div>
      </body>
    </html>
  );
}
