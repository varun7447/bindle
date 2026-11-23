import type { Metadata } from 'next';
import { Geist, Geist_Mono } from 'next/font/google';
import { Sidebar } from '@/components/Sidebar';
import { authConfig } from '@/lib/auth';
import './globals.css';

// Self-hosted by next/font at build time: no requests to Google at runtime.
const sans = Geist({ subsets: ['latin'], variable: '--font-sans', display: 'swap' });
const mono = Geist_Mono({ subsets: ['latin'], variable: '--font-mono', display: 'swap' });

export const metadata: Metadata = {
  title: 'Bindle',
  description: 'Observability for AI agents',
  robots: { index: false, follow: false },
};

// Applies a saved theme choice before first paint (no flash). A constant
// string: nothing user-controlled is interpolated.
const THEME_SCRIPT = `try{var t=localStorage.getItem('bindle-theme');if(t==='light'||t==='dark')document.documentElement.dataset.theme=t}catch(e){}`;

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${sans.variable} ${mono.variable}`} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_SCRIPT }} />
      </head>
      <body>
        <div className="shell">
          <Sidebar signOut={authConfig() !== null} />
          <main className="main">{children}</main>
        </div>
      </body>
    </html>
  );
}
