import { describe, expect, it } from 'vitest';
import { type Economics, agentTotals, parseChecks, parseProblemSource, problemHref, problemTitle, wasteItems } from './outcomes';

const econ = (o: Partial<Economics>): Economics => ({
  agent: 'refunds',
  version: 'v1',
  definition: 'no_failure_or_negative_feedback',
  runs: 0,
  successes: 0,
  wasted_runs: 0,
  outcome_runs: 0,
  cost_usd: 0,
  wasted_usd: 0,
  cost_per_success_usd: null,
  unknown_cost_runs: 0,
  ...o,
});

describe('problem sources', () => {
  it('round-trips through the URL and rejects bad input', () => {
    const href = problemHref({ kind: 'failure', step_name: 'lookup_order', pattern: 'timeout after <n>s', window: '30d' });
    const sp = Object.fromEntries(new URL(href, 'http://x').searchParams);
    expect(parseProblemSource(sp)).toEqual({ kind: 'failure', step_name: 'lookup_order', pattern: 'timeout after <n>s', window: '30d' });
    expect(parseProblemSource({ kind: 'nope' })).toBeNull();
    expect(parseProblemSource({ kind: 'failure' })).toBeNull(); // a failure needs a group
    expect(parseProblemSource({ kind: 'failed_runs', agent: 'a\nb' })).toBeNull();
    expect(parseProblemSource({ kind: 'failed_runs', agent: 'x'.repeat(257) })).toBeNull();
    expect(parseProblemSource({ kind: 'negative_feedback', window: '1y' })).toEqual({ kind: 'negative_feedback' });
  });
  it('titles sources in plain words', () => {
    expect(problemTitle({ kind: 'failure', step_name: 'lookup_order', pattern: 'timeout' })).toBe('Runs where lookup_order failed with “timeout”');
    expect(problemTitle({ kind: 'negative_feedback', agent: 'chat' })).toBe('Runs users gave a thumbs down of chat');
    expect(problemTitle({ kind: 'failed_score', score: 'judge' })).toBe('Runs that failed the “judge” score');
  });
  it('keeps only well-formed checks', () => {
    const ok = JSON.stringify({ name: 'No step fails', kind: 'run_succeeded', agent: 'a', value: '', number: 0, case_sensitive: false });
    const out = parseChecks([ok, '{bad', JSON.stringify({ name: 'x', kind: 'llm_judge' }), JSON.stringify({ name: 'y', kind: 'max_steps', number: 'NaN' })]);
    expect(out).toEqual([
      { name: 'No step fails', kind: 'run_succeeded', agent: 'a', value: '', number: 0, case_sensitive: false },
      { name: 'y', kind: 'max_steps', agent: '', value: '', number: 0, case_sensitive: false },
    ]);
  });
});

describe('outcome economics', () => {
  it('adds versions up per agent', () => {
    const [t] = agentTotals([
      econ({ version: 'v2', runs: 10, successes: 8, cost_usd: 2, wasted_usd: 0.4, wasted_runs: 2, definition: 'outcome' }),
      econ({ version: 'v1', runs: 10, successes: 2, cost_usd: 2, wasted_usd: 1.6, wasted_runs: 8, unknown_cost_runs: 1 }),
    ]);
    expect(t).toMatchObject({ runs: 20, successes: 10, cost_usd: 4, wasted_usd: 2, wasted_runs: 10, unknown_cost_runs: 1, definition: 'outcome' });
    expect(t!.cost_per_success_usd).toBeCloseTo(0.4);
  });
  it('flags agents that waste a large share of spend', () => {
    const items = wasteItems(
      [
        econ({ agent: 'refunds', runs: 20, successes: 10, cost_usd: 4, wasted_usd: 2.2, wasted_runs: 10, cost_per_success_usd: 0.4 }),
        econ({ agent: 'fine', runs: 20, successes: 19, cost_usd: 4, wasted_usd: 0.2, wasted_runs: 1 }),
        econ({ agent: 'tiny', runs: 3, cost_usd: 4, wasted_usd: 4, wasted_runs: 3 }),
        econ({ agent: 'unpriced', runs: 30, cost_usd: 0, wasted_runs: 30, unknown_cost_runs: 30 }),
      ],
      '30d',
    );
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ key: 'waste:refunds', severity: 'critical', cta: 'See cost per outcome' });
    expect(items[0]!.title).toBe('refunds: 55% of spend went to runs that didn’t succeed'.replace('’', "'"));
    expect(items[0]!.detail).toContain('$2.20 of $4.00 on 10 runs · $0.40 per successful outcome (no failed step and no thumbs down)');
    expect(items[0]!.href).toBe('/agents?agent=refunds&window=30d#economics-h');
  });
});
