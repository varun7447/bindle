// Command palette entries. Pure; unit-tested.

export interface Command {
  id: string;
  label: string;
  href: string;
  icon: 'home' | 'runs' | 'alerts' | 'settings' | 'connect' | 'keys' | 'sdk' | 'trace';
  hint?: string;
  keywords?: string;
}

export const PAGES: Command[] = [
  { id: 'home', label: 'Home', href: '/', icon: 'home', keywords: 'overview dashboard attention' },
  { id: 'runs', label: 'Runs', href: '/runs', icon: 'runs', keywords: 'traces list' },
  { id: 'failed', label: 'Failed runs', href: '/runs?status=failed', icon: 'runs', keywords: 'errors failures' },
  { id: 'alerts', label: 'Alerts', href: '/alerts', icon: 'alerts', keywords: 'rules channels slack webhook email' },
  { id: 'connect', label: 'Connect an agent', href: '/settings/connect', icon: 'connect', hint: 'Settings', keywords: 'setup install sdk quickstart' },
  { id: 'keys', label: 'Ingest keys', href: '/settings/keys', icon: 'keys', hint: 'Settings', keywords: 'api key rotate revoke' },
  { id: 'sdk', label: 'SDK settings', href: '/settings/sdk', icon: 'sdk', hint: 'Settings', keywords: 'sampling redaction capture content' },
];

const TRACE_ID = /^[0-9a-f]{32}$/;

export function paletteCommands(query: string): Command[] {
  const q = query.trim().toLowerCase();
  if (!q) return PAGES;
  const out: Command[] = [];
  if (TRACE_ID.test(q)) out.push({ id: 'trace', label: `Open trace ${q.slice(0, 12)}…`, href: `/traces/${q}`, icon: 'trace' });
  out.push(...PAGES.filter((p) => `${p.label} ${p.keywords ?? ''}`.toLowerCase().includes(q)));
  if (!TRACE_ID.test(q) && q.length <= 256) {
    out.push({ id: 'agent', label: `Runs for agent “${query.trim()}”`, href: `/runs?agent=${encodeURIComponent(query.trim())}`, icon: 'runs' });
  }
  return out;
}
