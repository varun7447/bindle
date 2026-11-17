import { describe, expect, it, vi } from 'vitest';
import { ApiError, api, apiGet, buildUrl } from './api';
import { diffMs, formatCost, formatCount, formatDuration, formatPercent, prettyPayload } from './format';
import { asWindow, isStepId, isTraceId, validSince } from './ids';
import { stepKind } from './steps';
import type { StepSummary } from './types';
import { buildWaterfall, defaultStepId } from './waterfall';

const T = '0123456789abcdef0123456789abcdef';

function step(id: string, parent: string, startUs: number, durMs: number, extra: Partial<StepSummary> = {}): StepSummary {
  // 2026-11-16T12:00:00 plus startUs microseconds, at nanosecond resolution.
  const ns = String(Math.round(startUs * 1000)).padStart(9, '0');
  const iso = `2026-11-16T12:00:00.${ns}Z`;
  return {
    step_id: id, parent_step_id: parent, name: id, type: 'tool_call', status: 'success', attempt: 0,
    start_time: iso, duration_ms: durMs, provider: '', model: '', tool_name: '', input_tokens: 0,
    output_tokens: 0, cached_tokens: 0, cost_usd: 0, cost_known: false, error_message: '',
    input_bytes: 0, output_bytes: 0, reasoning_bytes: 0, redacted: false, ...extra,
  };
}

describe('format', () => {
  it('formats counts, percents and durations', () => {
    expect(formatCount(1284)).toBe('1,284');
    expect(formatCount(12_900)).toBe('12.9K');
    expect(formatPercent(0)).toBe('0%');
    expect(formatPercent(0.0004)).toBe('<0.1%');
    expect(formatPercent(0.156)).toBe('16%');
    expect(formatPercent(0.0625)).toBe('6.3%');
    expect(formatDuration(0.03)).toBe('30 µs');
    expect(formatDuration(2.5)).toBe('2.5 ms');
    expect(formatDuration(1500)).toBe('1.50 s');
    expect(formatDuration(125_000)).toBe('2m 5s');
    expect(formatDuration(-1)).toBe('—');
  });

  it('marks partial cost as a lower bound', () => {
    expect(formatCost(0)).toBe('$0');
    expect(formatCost(0.004)).toBe('$0.0040');
    expect(formatCost(12.345)).toBe('$12.35');
    expect(formatCost(1.5, true)).toBe('≥ $1.50');
    expect(formatCost(0, true)).toBe('Unknown');
  });

  it('pretty-prints JSON only', () => {
    expect(prettyPayload('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(prettyPayload('{not json')).toBe('{not json');
    expect(prettyPayload('plain text')).toBe('plain text');
  });

  it('keeps sub-millisecond precision', () => {
    expect(diffMs('2026-11-16T12:00:00.000030000Z', '2026-11-16T12:00:00Z')).toBeCloseTo(0.03, 9);
    expect(diffMs('2026-11-16T12:00:00.000000001Z', '2026-11-16T12:00:00Z')).toBeCloseTo(0.000001, 12);
    expect(diffMs('2026-11-16T12:00:00.5+00:00', '2026-11-16T12:00:00Z')).toBeCloseTo(500, 9);
    expect(diffMs('2026-11-16T12:00:01.25Z', '2026-11-16T12:00:00.75Z')).toBeCloseTo(500, 9);
  });
});

describe('ids', () => {
  it('accepts only lowercase hex IDs of the right length', () => {
    expect(isTraceId(T)).toBe(true);
    expect(isTraceId(T.toUpperCase())).toBe(false);
    expect(isTraceId('../overview')).toBe(false);
    expect(isStepId('0123456789abcdef')).toBe(true);
    expect(isStepId(T)).toBe(false);
  });

  it('validates windows and since', () => {
    expect(asWindow('7d')).toBe('7d');
    expect(asWindow('1y')).toBe('24h');
    expect(asWindow(undefined)).toBe('24h');
    const now = Date.UTC(2026, 10, 16);
    expect(validSince('2026-11-15T00:00:00Z', now)).toBe('2026-11-15T00:00:00.000Z');
    expect(validSince('2026-11-17T00:00:00Z', now)).toBeNull();
    expect(validSince('2026-01-01T00:00:00Z', now)).toBeNull();
    expect(validSince('nope', now)).toBeNull();
    expect(validSince(null, now)).toBeNull();
  });
});

describe('api client', () => {
  it('drops empty params and encodes values', () => {
    const u = new URL(buildUrl('http://h:1', '/api/runs', { agent: "a b&c", status: '', cursor: undefined }));
    expect(u.searchParams.get('agent')).toBe('a b&c');
    expect(u.searchParams.has('status')).toBe(false);
    expect(u.searchParams.has('cursor')).toBe(false);
  });

  it('maps API errors and never sends a tenant', async () => {
    const f = vi.fn(async () =>
      new Response(JSON.stringify({ error: { code: 'unauthenticated', message: 'a valid session is required' } }), { status: 401 }),
    );
    await expect(apiGet('/api/runs', {}, f as unknown as typeof fetch)).rejects.toMatchObject({
      status: 401,
      code: 'unauthenticated',
    });
    const calledUrl = String((f.mock.calls[0] as unknown[])[0]);
    expect(calledUrl).not.toMatch(/tenant/);
  });

  it('reports an unreachable API', async () => {
    const f = vi.fn(async () => {
      throw new TypeError('fetch failed');
    });
    await expect(apiGet('/api/runs', {}, f as unknown as typeof fetch)).rejects.toMatchObject({ status: 0, code: 'unreachable' });
  });

  it('handles non-JSON error bodies', async () => {
    const f = vi.fn(async () => new Response('<html>bad gateway</html>', { status: 502 }));
    const err = await apiGet('/x', {}, f as unknown as typeof fetch).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(502);
  });

  it('refuses malformed IDs before calling the API', async () => {
    await expect(api.trace('../overview')).rejects.toMatchObject({ status: 400 });
    await expect(api.step(T, '../../x')).rejects.toMatchObject({ status: 400 });
  });
});

describe('waterfall', () => {
  it('orders depth-first by start time and positions rows', () => {
    const steps = [
      step('c', 'a', 600, 0.2),
      step('a', '', 0, 1),
      step('b', 'a', 100, 0.4),
      step('d', 'b', 200, 0.1),
    ];
    const w = buildWaterfall(steps);
    expect(w.rows.map((r) => `${r.step.step_id}@${r.depth}`)).toEqual(['a@0', 'b@1', 'd@2', 'c@1']);
    expect(w.spanMs).toBeCloseTo(1, 6);
    const b = w.rows[1]!;
    expect(b.leftPct).toBeCloseTo(10, 4);
    expect(b.widthPct).toBeCloseTo(40, 4);
  });

  it('treats orphans as roots and survives cycles', () => {
    const steps = [step('x', 'missing', 0, 1), step('p', 'q', 10, 1), step('q', 'p', 20, 1)];
    const w = buildWaterfall(steps);
    expect(w.rows).toHaveLength(3);
    expect(new Set(w.rows.map((r) => r.step.step_id))).toEqual(new Set(['x', 'p', 'q']));
  });

  it('gives zero-duration steps a visible minimum width', () => {
    const w = buildWaterfall([step('a', '', 0, 0), step('b', 'a', 0, 0)]);
    for (const r of w.rows) {
      expect(r.widthPct).toBeGreaterThan(0);
      expect(r.leftPct + r.widthPct).toBeLessThanOrEqual(100);
    }
  });

  it('defaults selection to the first failed step', () => {
    const w = buildWaterfall([step('a', '', 0, 1), step('b', 'a', 1, 1, { status: 'failure' })]);
    expect(defaultStepId(w.rows)).toBe('b');
    expect(defaultStepId(buildWaterfall([step('a', '', 0, 1)]).rows)).toBe('a');
    expect(defaultStepId([])).toBeUndefined();
  });

  it('empty trace', () => {
    expect(buildWaterfall([])).toEqual({ rows: [], spanMs: 0 });
  });
});

describe('step kinds', () => {
  it('uses theme color tokens and labels unknown types', () => {
    expect(stepKind('llm_call').color).toBe('var(--k-llm)');
    expect(stepKind('handoff').color).toBe('var(--k-handoff)');
    expect(stepKind('custom_thing').label).toBe('Custom thing');
  });
});
