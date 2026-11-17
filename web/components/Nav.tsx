'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';

const LINKS = [
  { href: '/', label: 'Overview' },
  { href: '/runs', label: 'Runs' },
  { href: '/connect', label: 'Connect an agent' },
];

export function Nav() {
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
    </nav>
  );
}
