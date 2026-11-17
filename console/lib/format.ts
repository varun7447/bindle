// Display formatting. Pure functions, unit-tested.

const compact = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 });
const full = new Intl.NumberFormat('en-US');

/** 1,284 below 10,000; 12.9K above. */
export function formatCount(n: number): string {
  return Math.abs(n) < 10_000 ? full.format(n) : compact.format(n);
}

export function formatPercent(ratio: number): string {
  if (!Number.isFinite(ratio) || ratio === 0) return '0%';
  const pct = ratio * 100;
  if (pct < 0.1) return '<0.1%';
  return `${pct < 10 ? pct.toFixed(1) : Math.round(pct)}%`;
}

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—';
  if (ms === 0) return '0 ms';
  if (ms < 1) return `${Math.round(ms * 1000)} µs`;
  if (ms < 1000) return `${ms < 10 ? ms.toFixed(1) : Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s < 10 ? s.toFixed(2) : s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

/**
 * Cost from known prices only. When some model steps have no known price the
 * total is a lower bound, never presented as the full cost.
 */
export function formatCost(usd: number, partial = false): string {
  // Nothing priced at all: a "$0" or "≥ $0" would read as free.
  if (partial && usd === 0) return 'Unknown';
  let s: string;
  if (usd === 0) s = '$0';
  else if (usd < 0.01) s = `$${usd.toFixed(4)}`;
  else if (usd < 1000) s = `$${usd.toFixed(2)}`;
  else s = `$${compact.format(usd)}`;
  return partial ? `≥ ${s}` : s;
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

/** "Nov 16, 23:12:15 UTC". The console shows UTC so all viewers agree. */
export function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return (
    d.toLocaleString('en-US', {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
      timeZone: 'UTC',
    }) + ' UTC'
  );
}

export function formatRelative(iso: string, now = Date.now()): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '—';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

/** Pretty-prints JSON payloads; returns other text unchanged. */
export function prettyPayload(s: string): string {
  const t = s.trim();
  if (!(t.startsWith('{') || t.startsWith('['))) return s;
  try {
    return JSON.stringify(JSON.parse(t), null, 2);
  } catch {
    return s;
  }
}

/**
 * Splits an RFC 3339 timestamp into whole epoch milliseconds and the sub-ms
 * remainder. Epoch ms in a double cannot hold nanoseconds, so offsets are
 * computed part by part with diffMs.
 */
export function timeParts(iso: string): { ms: number; subMs: number } {
  const m = /^(.*?)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$/.exec(iso);
  if (!m) return { ms: Date.parse(iso), subMs: 0 };
  const frac = (m[2] ?? '').padEnd(9, '0').slice(0, 9); // nanoseconds
  return {
    ms: Date.parse(`${m[1]}${m[3]}`) + Number(frac.slice(0, 3)),
    subMs: Number(frac.slice(3)) / 1e6,
  };
}

/** a - b in milliseconds, precise to the nanosecond for nearby times. */
export function diffMs(a: string, b: string): number {
  const pa = timeParts(a);
  const pb = timeParts(b);
  return pa.ms - pb.ms + (pa.subMs - pb.subMs);
}
