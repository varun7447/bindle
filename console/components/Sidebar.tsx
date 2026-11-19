'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { useEffect, useState } from 'react';
import { CommandPalette } from './CommandPalette';
import { BindleLogo } from './Logo';
import { BellIcon, BotIcon, CheckCircleIcon, GearIcon, HomeIcon, LogoutIcon, MoonIcon, RunsIcon, SearchIcon, SunIcon } from './Icons';

// Five places to look, one place to configure.
const PRIMARY = [
  { href: '/', label: 'Home', icon: HomeIcon },
  { href: '/runs', label: 'Runs', icon: RunsIcon },
  { href: '/agents', label: 'Agents', icon: BotIcon },
  { href: '/evals', label: 'Evals', icon: CheckCircleIcon },
  { href: '/alerts', label: 'Alerts', icon: BellIcon },
];

type Theme = 'light' | 'dark';

function currentTheme(): Theme {
  const stamped = document.documentElement.dataset.theme;
  if (stamped === 'light' || stamped === 'dark') return stamped;
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function Sidebar({ signOut }: { signOut: boolean }) {
  const path = usePathname();
  const [palette, setPalette] = useState(false);
  const [theme, setTheme] = useState<Theme | null>(null);

  useEffect(() => setTheme(currentTheme()), []);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const active = (href: string) =>
    href === '/' ? path === '/' : path === href || path.startsWith(`${href}/`) || (href === '/runs' && path.startsWith('/traces/'));

  const toggleTheme = () => {
    const next: Theme = currentTheme() === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    try {
      localStorage.setItem('bindle-theme', next);
    } catch {
      // storage unavailable: the choice lasts for this page only
    }
    setTheme(next);
  };

  return (
    <aside className="side" aria-label="Main">
      <Link href="/" className="brand" aria-label="Bindle home">
        <BindleLogo />
      </Link>
      <button type="button" className="side-search" onClick={() => setPalette(true)} aria-label="Search or jump to (Ctrl+K)">
        <SearchIcon />
        <span>Search or jump to…</span>
        <kbd>⌘K</kbd>
      </button>
      <nav aria-label="Sections">
        {PRIMARY.map(({ href, label, icon: Icon }) => (
          <Link key={href} href={href} className="nav-item" aria-current={active(href) ? 'page' : undefined}>
            <Icon />
            {label}
          </Link>
        ))}
      </nav>
      <div className="side-foot">
        <Link href="/settings" className="nav-item" aria-current={active('/settings') ? 'page' : undefined}>
          <GearIcon />
          Settings
        </Link>
        <button type="button" onClick={toggleTheme} aria-label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}>
          {theme === 'dark' ? <SunIcon /> : <MoonIcon />}
          {theme === 'dark' ? 'Light theme' : 'Dark theme'}
        </button>
        {signOut ? (
          <form method="post" action="/auth/logout">
            <button type="submit">
              <LogoutIcon />
              Sign out
            </button>
          </form>
        ) : null}
      </div>
      {palette ? <CommandPalette onClose={() => setPalette(false)} /> : null}
    </aside>
  );
}
