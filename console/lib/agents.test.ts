import { describe, expect, it } from 'vitest';
import { toolFlags, verdict, versionRows, type ToolHealth, type VersionStats } from './agents';

const v = (o: Partial<VersionStats>): VersionStats => ({
  version: 'v1', runs: 20, failed_runs: 2, cost_usd: 2, cost_partial: false, p50_run_ms: 100, p95_run_ms: 1000, avg_steps: 8, avg_llm_calls: 3,
  input_tokens: 1000, output_tokens: 100, cached_tokens: 500, tool_calls: 40, tool_failures: 4, first_run_start: '', last_run_start: '', ...o,
});

describe('version comparison', () => {
  it('compares each version with the one before', () => {
    const rows = versionRows([v({ version: 'v2', failed_runs: 1, cost_usd: 3, avg_steps: 8 }), v({})]);
    expect(rows[0]!.failureRate).toBe(0.05);
    expect(rows[0]!.costPerRun).toBe(0.15);
    expect(rows[0]!.vs!.failureRate).toMatchObject({ text: '−5.0 pts', good: true });
    expect(rows[0]!.vs!.costPerRun).toMatchObject({ text: '+50%', good: false });
    expect(rows[1]!.vs).toBeUndefined();
    expect(verdict(rows)).toBe('v2 vs v1: failure rate −5.0 pts (better), cost per run +50% (worse).');
  });
  it('skips comparisons with too few runs', () => {
    const rows = versionRows([v({ version: 'v2', runs: 2 }), v({})]);
    expect(rows[0]!.vs).toBeUndefined();
    expect(verdict(rows)).toBeNull();
  });
  it('says when nothing changed', () => {
    expect(verdict(versionRows([v({ version: 'v2' }), v({})]))).toBe('v2 performs like v1.');
  });
});

describe('tool flags', () => {
  it('flags failing, misused, bloated and slow tools', () => {
    const t: ToolHealth = { tool_name: 't', framework: '', calls: 10, failures: 3, argument_errors: 2, runs: 4, p50_ms: 10, p95_ms: 12_000,
      avg_output_bytes: 1, max_output_bytes: 80_000, top_error: '', sample_trace_id: '', sample_step_id: '' };
    expect(toolFlags(t)).toEqual(['fails often', 'bad arguments', 'large results', 'slow']);
    expect(toolFlags({ ...t, failures: 0, argument_errors: 0, max_output_bytes: 10, p95_ms: 5 })).toEqual([]);
  });
});
