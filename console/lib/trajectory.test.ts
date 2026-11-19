import { describe, expect, it } from 'vitest';
import { argsDiffer, compareRuns, normalizeArgs, signature } from './trajectory';
import type { StepSummary } from './types';

let clock = 0;
const step = (o: Partial<StepSummary>): StepSummary => {
  clock += 100;
  return {
    step_id: `s${clock}`, parent_step_id: 'root', name: 'x', type: 'tool_call', status: 'success', attempt: 0,
    start_time: new Date(Date.UTC(2026, 10, 17) + clock).toISOString(), duration_ms: 10, provider: '', model: 'm1', tool_name: '',
    input_tokens: 0, output_tokens: 0, cached_tokens: 0, cost_usd: 0, cost_known: false, error_message: '',
    input_bytes: 0, output_bytes: 0, reasoning_bytes: 0, redacted: false, ...o,
  };
};
const llm = (o: Partial<StepSummary> = {}) => step({ type: 'llm_call', name: 'chat', ...o });
const tool = (name: string, o: Partial<StepSummary> = {}) => step({ name, tool_name: name, ...o });
const agent = () => step({ type: 'agent', name: 'run', parent_step_id: '' });

describe('trajectory diff', () => {
  it('finds where two runs diverge', () => {
    const a = [agent(), llm(), tool('lookup_order'), llm()];
    const b = [agent(), llm(), tool('search'), tool('search'), tool('lookup_order'), llm()];
    const t = compareRuns(a, b);
    expect(t.same).toBe(3);
    expect(t.onlyA).toBe(0);
    expect(t.onlyB).toBe(2);
    expect(t.divergeAt).toBe(1);
    expect(t.rows.map((r) => r.kind)).toEqual(['same', 'only_b', 'only_b', 'same', 'same']);
    expect(t.summary[0]).toBe("The runs match for the first 1 step, then diverge: B calls search, which A doesn't.");
    expect(t.summary).toContain("B has 2 steps A doesn't (3 in common).");
    expect(t.summary).toContain('search: called 0 times in A, 2 times in B.');
  });

  it('names both sides when each takes a different step', () => {
    const t = compareRuns([llm(), tool('refund')], [llm(), tool('escalate')]);
    expect(t.summary[0]).toBe('The runs match for the first 1 step, then diverge: A calls refund, B calls escalate.');
  });

  it('reports identical paths and per-step changes', () => {
    const a = [llm({ duration_ms: 300, input_tokens: 1000 }), tool('search', { status: 'success' })];
    const b = [llm({ duration_ms: 1500, input_tokens: 3000, model: 'm2' }), tool('search', { status: 'failure' })];
    const t = compareRuns(a, b);
    expect(t.divergeAt).toBe(-1);
    expect(t.summary[0]).toBe('Both runs took the same 2 steps in the same order; 2 steps behaved differently.');
    expect(t.rows[0]!.changes.map((c) => c.field)).toEqual(['duration', 'tokens', 'model']);
    expect(t.rows[1]!.changes[0]).toEqual({ field: 'status', text: 'succeeded in A, failed in B' });
    expect(t.summary.at(-1)).toBe('search fails in B but not in A.');
  });

  it('ignores agent wrappers and small changes', () => {
    const t = compareRuns([agent(), llm({ duration_ms: 100 })], [llm({ duration_ms: 150 })]);
    expect(t.rows).toHaveLength(1);
    expect(t.rows[0]!.changes).toEqual([]);
    expect(compareRuns([], []).summary).toEqual(['Neither run has steps to compare.']);
  });

  it('matches LLM calls regardless of model, tools by name', () => {
    expect(signature(llm({ model: 'a' }))).toBe(signature(llm({ model: 'b' })));
    expect(signature(tool('a'))).not.toBe(signature(tool('b')));
  });

  it('compares tool arguments ignoring key order and whitespace', () => {
    expect(normalizeArgs('{"b": 1, "a": {"y": 2, "x": [1, {"d": 1, "c": 2}]}}')).toBe('{"a":{"x":[1,{"c":2,"d":1}],"y":2},"b":1}');
    expect(argsDiffer('{"id": 1, "q": "x"}', '{"q":"x","id":1}')).toBe(false);
    expect(argsDiffer('{"id": 1}', '{"id": 2}')).toBe(true);
    expect(argsDiffer('order  42', 'order 42')).toBe(false);
  });
});
