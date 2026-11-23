'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { CheckCircleIcon, CoinIcon, KeyIcon, PlugIcon, ShieldIcon, SlidersIcon } from './Icons';

const TABS = [
  { href: '/settings/connect', label: 'Connect an agent', icon: PlugIcon },
  { href: '/settings/keys', label: 'Ingest keys', icon: KeyIcon },
  { href: '/settings/sdk', label: 'SDK settings', icon: SlidersIcon },
  { href: '/settings/limits', label: 'Agent limits', icon: ShieldIcon },
  { href: '/settings/prices', label: 'Model prices', icon: CoinIcon },
];

// Tabs for features that are off by default (operator-set flags).
const FLAGGED_TABS = [{ href: '/settings/outcomes', label: 'Outcomes', icon: CheckCircleIcon, feature: 'outcome_grading' }];

export function SettingsNav({ features = [] }: { features?: string[] }) {
  const path = usePathname();
  const tabs = [...TABS, ...FLAGGED_TABS.filter((t) => features.includes(t.feature))];
  return (
    <nav className="subnav" aria-label="Settings">
      {tabs.map(({ href, label, icon: Icon }) => (
        <Link key={href} href={href} aria-current={path.startsWith(href) ? 'page' : undefined}>
          <Icon />
          {label}
        </Link>
      ))}
    </nav>
  );
}
