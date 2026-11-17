// Pure helpers for the overview chart. Unit-tested.

import type { SeriesPoint } from './types';

export interface Bucket {
  start: number; // epoch ms
  runs: number;
  failed: number;
}

/**
 * Expands the API's sparse series (only buckets with runs) to every bucket in
 * [from, to]. The API aligns buckets to multiples of the
 * bucket size since the epoch, so the same alignment is used here.
 */
export function fillSeries(series: SeriesPoint[], fromIso: string, toIso: string, bucketSeconds: number): Bucket[] {
  const size = bucketSeconds * 1000;
  const from = Date.parse(fromIso);
  const to = Date.parse(toIso);
  if (!(size > 0) || Number.isNaN(from) || Number.isNaN(to) || to < from) return [];
  const byStart = new Map<number, SeriesPoint>();
  for (const p of series) byStart.set(Math.floor(Date.parse(p.bucket) / size) * size, p);
  const out: Bucket[] = [];
  for (let t = Math.floor(from / size) * size; t <= to && out.length < 1000; t += size) {
    const p = byStart.get(t);
    out.push({ start: t, runs: p?.runs ?? 0, failed: Math.min(p?.failed_runs ?? 0, p?.runs ?? 0) });
  }
  return out;
}

/** Clean axis ticks from 0 to >= max, about `target` intervals, steps of 1/2/5 x 10^k. */
export function niceTicks(max: number, target = 4): number[] {
  if (!(max > 0)) return [0, 1];
  const raw = max / target;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? 10 * mag;
  const s = Math.max(1, step); // counts are integers
  const ticks: number[] = [];
  for (let v = 0; v < max + s; v += s) ticks.push(v);
  return ticks;
}

export function bucketLabel(ms: number, bucketSeconds: number): string {
  const d = new Date(ms);
  const opts: Intl.DateTimeFormatOptions =
    bucketSeconds >= 86400
      ? { month: 'short', day: 'numeric', timeZone: 'UTC' }
      : { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false, timeZone: 'UTC' };
  return d.toLocaleString('en-US', opts);
}
