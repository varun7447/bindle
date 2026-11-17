import { describe, expect, it } from 'vitest';
import { fillSeries, niceTicks } from './chart';

describe('fillSeries', () => {
  it('fills empty buckets with zeros, aligned to the bucket size', () => {
    const b = fillSeries(
      [{ bucket: '2026-11-16T10:00:00Z', runs: 3, failed_runs: 1, cost_usd: 0 }],
      '2026-11-16T08:30:00Z',
      '2026-11-16T11:15:00Z',
      3600,
    );
    expect(b.map((x) => new Date(x.start).toISOString().slice(11, 16))).toEqual(['08:00', '09:00', '10:00', '11:00']);
    expect(b.map((x) => x.runs)).toEqual([0, 0, 3, 0]);
    expect(b[2]!.failed).toBe(1);
  });

  it('clamps failed to runs and rejects bad input', () => {
    expect(fillSeries([{ bucket: '2026-11-16T10:00:00Z', runs: 1, failed_runs: 5, cost_usd: 0 }], '2026-11-16T10:00:00Z', '2026-11-16T10:00:00Z', 3600)[0]!.failed).toBe(1);
    expect(fillSeries([], 'x', 'y', 3600)).toEqual([]);
    expect(fillSeries([], '2026-11-16T10:00:00Z', '2026-11-16T09:00:00Z', 3600)).toEqual([]);
  });
});

describe('niceTicks', () => {
  it('produces clean ticks covering the max', () => {
    expect(niceTicks(0)).toEqual([0, 1]);
    expect(niceTicks(3)).toEqual([0, 1, 2, 3]);
    expect(niceTicks(9)).toEqual([0, 5, 10]);
    expect(niceTicks(1234)).toEqual([0, 500, 1000, 1500]);
    for (const m of [1, 7, 42, 999, 12345]) expect(niceTicks(m).at(-1)!).toBeGreaterThanOrEqual(m);
  });
});
