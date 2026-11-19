'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { CoinIcon, KeyIcon, PlugIcon, ShieldIcon, SlidersIcon } from './Icons';

const TABS = [
  { href: '/settings/connect', label: 'Connect an agent', icon: PlugIcon },
  { href: '/settings/keys', label: 'Ingest keys', icon: KeyIcon },
  { href: '/settings/sdk', label: 'SDK settings', icon: SlidersIcon },
  { href: '/settings/limits', label: 'Agent limits', icon: ShieldIcon },
  { href: '/settings/prices', label: 'Model prices', icon: CoinIcon },
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
