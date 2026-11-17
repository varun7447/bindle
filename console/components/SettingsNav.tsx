'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { KeyIcon, PlugIcon, SlidersIcon } from './Icons';

const TABS = [
  { href: '/settings/connect', label: 'Connect an agent', icon: PlugIcon },
  { href: '/settings/keys', label: 'Ingest keys', icon: KeyIcon },
  { href: '/settings/sdk', label: 'SDK settings', icon: SlidersIcon },
];

export function SettingsNav() {
  const path = usePathname();
  return (
    <nav className="subnav" aria-label="Settings">
      {TABS.map(({ href, label, icon: Icon }) => (
        <Link key={href} href={href} aria-current={path.startsWith(href) ? 'page' : undefined}>
          <Icon />
          {label}
        </Link>
      ))}
    </nav>
  );
}
