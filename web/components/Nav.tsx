'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';

const LINKS = [
  { href: '/', label: 'Overview' },
  { href: '/runs', label: 'Runs' },
  { href: '/alerts', label: 'Alerts' },
  { href: '/connect', label: 'Connect an agent' },
  { href: '/keys', label: 'Ingest keys' },
  { href: '/settings', label: 'SDK settings' },
];

export function Nav({ signOut }: { signOut: boolean }) {
  const path = usePathname();
  const active = (href: string) =>
    href === '/' ? path === '/' : path === href || path.startsWith(`${href}/`) || (href === '/runs' && path.startsWith('/traces/'));
  return (
    <nav className="nav" aria-label="Main">
      <Link href="/" className="brand">
        <span className="brand-mark" aria-hidden="true" />
        bindle
      </Link>
      {LINKS.map((l) => (
        <Link key={l.href} href={l.href} aria-current={active(l.href) ? 'page' : undefined}>
          {l.label}
        </Link>
      ))}
      {signOut ? (
        <form method="post" action="/auth/logout" style={{ marginTop: 24 }}>
          <button type="submit" className="nav-signout">
            Sign out
          </button>
        </form>
      ) : null}
    </nav>
  );
}
