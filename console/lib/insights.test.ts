import { describe, expect, it } from 'vitest';
import { paletteCommands } from './commands';
import { attentionItems, delta, headline } from './insights';
import { buildStory, joinList } from './story';
import type { Insights, OverviewResponse, Run, StepSummary } from './types';

const ov = (o: Partial<OverviewResponse> = {}): OverviewResponse =>
  ({ window: '24h', from: '', to: '', bucket_seconds: 3600, runs: 30, failed_runs: 6, run_failure_rate: 0.2, step_failure_rate: 0.05,
    p50_run_ms: 8, p95_run_ms: 974, cost_usd: 1.5, cost_partial: false, cost_unknown_steps: 0, input_tokens: 1, output_tokens: 1,
    series: [], top_failing_tools: [], ...o }) as OverviewResponse;

const ins = (o: Partial<Insights> = {}): Insights => ({
  agents: [
    { agent: 'refund-agent', runs: 4, failed_runs: 4, cost_usd: 0, cost_partial: false, p95_run_ms: 250, last_run_start: '' },
    { agent: 'ok-agent', runs: 20, failed_runs: 1, cost_usd: 0, cost_partial: false, p95_run_ms: 10, last_run_start: '' },
    { agent: 'tiny', runs: 2, failed_runs: 2, cost_usd: 0, cost_partial: false, p95_run_ms: 10, last_run_start: '' },
  ],
  issues: [
    { step_name: 'invoke_agent refund', step_type: 'agent', failures: 1, runs: 1, last_seen: '', sample_error: 'x', sample_trace_id: 't1', sample_step_id: 'a' },
    { step_name: 'refund_payment', step_type: 'tool_call', failures: 4, runs: 4, last_seen: '', sample_error: 'payments API timed out', sample_trace_id: 't1', sample_step_id: 'b' },
  ],
  previous: { runs: 10, failed_runs: 0, cost_usd: 1, p95_run_ms: 900 },
  ...o,
});

describe('attention', () => {
  it('ranks failures, flags struggling agents, skips echoes and tiny samples', () => {
    const items = attentionItems(ov(), ins(), [{ kind: 'runaway_loop', severity: 'warning', trace_id: 't9', tool_name: 'search', repeats: 7, threshold: 5, first_seen: '', last_seen: '' }], '7d');
    expect(items.map((i) => i.key)).toEqual(['issue:refund_payment', 'agent:refund-agent', 'loop:t9:search']);
    expect(items[0]!.href).toBe('/traces/t1?step=b');
    expect(items[0]!.detail).toContain('payments API timed out');
    expect(items[1]!.href).toBe('/runs?range=7d&agent=refund-agent&status=failed');
    expect(items[0]!.secondary).toEqual({ href: '/evals/from-problems?kind=failure&step_name=refund_payment&window=7d', label: 'Add these runs to a dataset' });
  });
  it('adds wasted-spend items and ranks them with the rest', () => {
    const waste = { agent: 'refund-agent', version: 'v1', definition: 'outcome' as const, runs: 20, successes: 5, wasted_runs: 15, outcome_runs: 20,
      cost_usd: 2, wasted_usd: 1.5, cost_per_success_usd: 0.4, unknown_cost_runs: 0 };
    const items = attentionItems(ov(), ins(), [], '24h', [waste]);
    expect(items.map((i) => i.key)).toEqual(['issue:refund_payment', 'agent:refund-agent', 'waste:refund-agent']);
  });
  it('says all clear, or how many things need attention', () => {
    expect(headline(ov({ failed_runs: 0 }), ins({ issues: [], agents: [] }), [], 'Last 24 hours').title).toBe('All clear');
    const items = attentionItems(ov(), ins(), [], '24h');
    const h = headline(ov(), ins(), items, 'Last 24 hours');
    expect(h).toMatchObject({ tone: 'critical', title: '2 issues need attention' });
    expect(h.detail).toContain('30 runs from 3 agents, 20% failed');
    expect(headline(ov({ runs: 0 }), ins(), [], 'Last 24 hours').tone).toBe('empty');
  });
  it('flags unpriced models as info only', () => {
    const items = attentionItems(ov({ cost_partial: true, cost_unknown_steps: 3 }), ins({ issues: [], agents: [] }), [], '24h');
    expect(items).toHaveLength(1);
    expect(items[0]!.severity).toBe('info');
    expect(headline(ov(), ins({ issues: [], agents: [] }), items, 'x').tone).toBe('good');
  });
});

describe('delta', () => {
  it('compares with the previous period', () => {
    expect(delta(30, 10)).toEqual({ text: '+200%', direction: 'up', good: undefined });
    expect(delta(0.2, 0.05, { higherIsBetter: false, percentPoints: true })).toEqual({ text: '+15 pts', direction: 'up', good: false });
    expect(delta(800, 1000, { higherIsBetter: false })).toMatchObject({ direction: 'down', good: true });
    expect(delta(5, 0).direction).toBe('new');
    expect(delta(0, 0).direction).toBe('flat');
    expect(delta(100, 100.1).direction).toBe('flat');
  });
});

const step = (o: Partial<StepSummary>): StepSummary =>
  ({ step_id: 's', parent_step_id: 'root', name: 'x', type: 'tool_call', status: 'success', attempt: 0, start_time: '2026-11-17T00:00:00Z',
    duration_ms: 10, provider: '', model: '', tool_name: '', input_tokens: 0, output_tokens: 0, cached_tokens: 0, cost_usd: 0,
    cost_known: false, error_message: '', input_bytes: 0, output_bytes: 0, reasoning_bytes: 0, redacted: false, ...o }) as StepSummary;

describe('run story', () => {
  const run = { agent: 'refund-agent', status: 'failed', duration_ms: 253, cost_usd: 0, cost_partial: true, cost_unknown_steps: 2 } as Run;
  const steps = [
    step({ step_id: 'root', parent_step_id: '', type: 'agent', name: 'refund-agent run', status: 'failure', duration_ms: 253 }),
    step({ step_id: 'l1', type: 'llm_call', name: 'plan', duration_ms: 59 }),
    step({ step_id: 'f1', name: 'refund_payment', tool_name: 'refund_payment', status: 'failure', error_message: 'payments API timed out after 20ms', duration_ms: 23 }),
    step({ step_id: 'f2', name: 'refund_payment', tool_name: 'refund_payment', attempt: 1, duration_ms: 20 }),
    ...Array.from({ length: 6 }, (_, i) => step({ step_id: `g${i}`, name: 'get_order', tool_name: 'get_order', duration_ms: 12 })),
    step({ step_id: 'l2', type: 'llm_call', name: 'respond', duration_ms: 37 }),
  ];
  it('explains what happened', () => {
    const s = buildStory(run, steps);
    expect(s.headline).toBe('refund-agent failed after 253 ms.');
    expect(s.tone).toBe('critical');
    expect(s.firstFailureId).toBe('f1');
    const text = s.lines.map((l) => l.text);
    expect(text[0]).toBe('It made 2 LLM calls and 8 tool calls.');
    expect(text[1]).toBe('refund_payment failed: “payments API timed out after 20ms”. A retry succeeded.');
    expect(text).toContain('get_order was called 6 times, which may be a loop.');
    expect(text.some((t) => t.startsWith('Most time went to 8 tool calls (55%). The slowest step was plan at 59 ms.'))).toBe(true);
    expect(text.at(-1)).toBe('Its cost is unknown: 2 model steps have no known price.');
  });
  it('counts only leaf steps for time', () => {
    const s = buildStory(run, steps);
    const total = s.time.reduce((n, t) => n + t.ms, 0);
    expect(total).toBe(59 + 23 + 20 + 6 * 12 + 37);
    expect(s.time.find((t) => t.type === 'agent')).toBeUndefined();
  });
  it('is calm for a clean run', () => {
    const s = buildStory({ ...run, status: 'ok', cost_partial: false, cost_usd: 0.0123 } as Run, [step({ step_id: 'a', type: 'llm_call', parent_step_id: '' })]);
    expect(s.tone).toBe('good');
    expect(s.headline).toBe('refund-agent succeeded after 253 ms.');
    expect(joinList(['a', 'b', 'c'])).toBe('a, b and c');
  });
});

describe('command palette', () => {
  it('jumps to pages, traces and agents', () => {
    expect(paletteCommands('').length).toBeGreaterThan(5);
    expect(paletteCommands('keys')[0]!.href).toBe('/settings/keys');
    const id = 'a'.repeat(32);
    expect(paletteCommands(id)[0]!.href).toBe(`/traces/${id}`);
    expect(paletteCommands('refund').at(-1)!.href).toBe('/runs?agent=refund');
  });
});

describe('attention: error variants and unusual runs', () => {
  const run = { trace_id: 'tz', agent: 'busy', cost_partial: false } as unknown as import('./types').Run;
  it('lists distinct errors behind one issue', () => {
    const issue = { ...ins().issues[1]!, patterns: [
      { pattern: 'timeout after <n>s', failures: 3, sample_trace_id: 't1', sample_step_id: 'b' },
      { pattern: 'HTTP <n>', failures: 1, sample_trace_id: 't2', sample_step_id: 'c' },
    ] };
    const items = attentionItems(ov(), ins({ issues: [issue], agents: [] }), [], '24h');
    expect(items[0]!.variants).toEqual([
      { text: 'timeout after <n>s', count: 3, href: '/traces/t1?step=b' },
      { text: 'HTTP <n>', count: 1, href: '/traces/t2?step=c' },
    ]);
    const one = attentionItems(ov(), ins({ issues: [{ ...issue, patterns: issue.patterns.slice(0, 1) }], agents: [] }), [], '24h');
    expect(one[0]!.variants).toBeUndefined();
  });
  it('flags runs far above the agent median', () => {
    const items = attentionItems(ov(), ins({ issues: [], agents: [], outliers: [
      { run, reason: 'steps', value: 30, typical: 2 },
      { run: { ...run, trace_id: 'ty' }, reason: 'cost', value: 0.9, typical: 0.2 },
    ] }), [], '24h');
    expect(items.map((i) => i.title)).toEqual(['busy: a run took 30 steps', 'busy: a run cost $0.90']);
    expect(items[0]!.detail).toBe('15× the typical 2 steps for this agent. It may be stuck or looping.');
    expect(items[1]!.detail).toContain('4.5× the typical');
    expect(items[0]!.href).toBe('/traces/tz');
  });
});

describe('attention: unhappy sessions', () => {
  it('links to the conversation', () => {
    const items = attentionItems(ov(), ins({ issues: [], agents: [], unhappy_sessions: [{ session_id: 'conv/1', feedback: { positive: 0, negative: 2, count: 2 } }] }), [], '24h');
    expect(items[0]).toMatchObject({ severity: 'warning', href: '/sessions/conv%2F1', cta: 'Open conversation' });
    expect(items[0]!.detail).toContain('2 thumbs down');
    expect(items[0]!.secondary?.href).toBe('/evals/from-problems?kind=negative_feedback&window=24h');
  });
});
