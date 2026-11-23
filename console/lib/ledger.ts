// The tamper-evident ledger (feature "ledger"): types and the plain-language
// lines the Ledger tab leads with.
import { formatCount } from './format';

export interface LedgerEntry {
  agent: string;
  seq: number;
  day: string;
  at: string;
  kind: string;
  trace_id?: string;
  step_id?: string;
  step_type?: string;
  step_name?: string;
  tool_name?: string;
  status?: string;
  error_class?: string;
  input_tokens: number;
  output_tokens: number;
  cost_usd: string;
  cost_known: boolean;
  security_flags?: string[];
  prev_hash: string;
  hash: string;
}

export interface LedgerResult {
  ok: boolean;
  entries: number;
  from: string;
  to: string;
  first_seq?: number;
  last_seq?: number;
  first_broken?: { seq: number; at: string; reason: string };
}

const UTC = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false, timeZone: 'UTC' });

/** "Nov 3, 14:02 UTC", or '' for a missing time. */
export function utcStamp(iso: string | undefined): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (!Number.isFinite(t) || t <= 0) return '';
  return `${UTC.format(t)} UTC`;
}

function period(days: number): string {
  return days === 1 ? 'today' : `the last ${days} days`;
}

/** The sentence the Ledger tab opens with. */
export function ledgerSentence(r: LedgerResult, days: number): { tone: 'good' | 'critical' | 'empty'; title: string; detail: string } {
  if (!r.ok && r.first_broken) {
    const when = utcStamp(r.first_broken.at);
    return {
      tone: 'critical',
      title: `Ledger broken at entry ${formatCount(r.first_broken.seq)}${when ? ` (${when})` : ''}`,
      detail: `${capitalize(r.first_broken.reason)}. Entries before it verified; anything after it can't be trusted until this is explained.`,
    };
  }
  if (r.entries === 0) {
    return { tone: 'empty', title: `No ledger entries in ${period(days)}`, detail: 'Entries are written as this agent’s steps arrive.' };
  }
  return {
    tone: 'good',
    title: `Ledger intact for ${period(days)}: ${formatCount(r.entries)} ${r.entries === 1 ? 'entry' : 'entries'}`,
    detail: `Every entry’s SHA-256 hash matches its contents and the entry before it (entries ${formatCount(r.first_seq ?? 0)}–${formatCount(r.last_seq ?? 0)}).`,
  };
}

function capitalize(s: string): string {
  return s ? s[0]!.toUpperCase() + s.slice(1) : s;
}

/** First 12 hex characters, for display; the full hash is in the title. */
export function shortHash(h: string): string {
  return h.length > 12 ? `${h.slice(0, 12)}…` : h;
}

/** YYYY-MM-DD for the UTC day `daysAgo` days before now. */
export function utcDay(daysAgo: number, now = Date.now()): string {
  return new Date(now - daysAgo * 86_400_000).toISOString().slice(0, 10);
}

/** Download link for one day's plain-English summary. */
export function summaryHref(agent: string, day: string): string {
  return `/console-api/agents/ledger/summary?${new URLSearchParams({ agent, day }).toString()}`;
}

/** Days the tab can show. */
export const LEDGER_DAYS = [1, 7, 30, 90] as const;

export function asLedgerDays(v: string | undefined): number {
  const n = Number(v);
  return (LEDGER_DAYS as readonly number[]).includes(n) ? n : 30;
}

/** Cost cell: unknown model cost is shown as unknown, never as $0. */
export function entryCost(e: LedgerEntry): string {
  if (!e.cost_known) return e.step_type === 'llm_call' ? 'Unknown' : '—';
  const v = Number(e.cost_usd);
  if (!Number.isFinite(v)) return '—';
  return v >= 1 ? `$${v.toFixed(2)}` : `$${v.toFixed(4)}`;
}
