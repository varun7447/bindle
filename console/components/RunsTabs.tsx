import Link from 'next/link';

const TABS = [
  { href: '/runs', label: 'Runs' },
  { href: '/sessions', label: 'Sessions' },
  { href: '/search', label: 'Search' },
] as const;

/** Runs, sessions and search are three views of the same data. */
export function RunsTabs({ current }: { current: (typeof TABS)[number]['href'] }) {
  return (
    <nav className="seg" aria-label="View">
      {TABS.map((t) => (
        <Link key={t.href} href={t.href} aria-current={t.href === current ? 'true' : undefined}>
          {t.label}
        </Link>
      ))}
    </nav>
  );
}
