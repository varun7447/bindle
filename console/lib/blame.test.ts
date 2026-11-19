import { describe, expect, it } from 'vitest';
import { likelyCause } from './blame';
import { buildStory } from './story';
import type { Run, StepSummary } from './types';

let clock = 0;
const step = (o: Partial<StepSummary>): StepSummary => {
  clock += 100;
  return {
    step_id: 's', parent_step_id: 'root', name: 'x', type: 'tool_call', status: 'success', attempt: 0,
    start_time: new Date(Date.UTC(2026, 10, 17) + clock).toISOString(), duration_ms: 10, provider: '', model: '', tool_name: '',
    input_tokens: 0, output_tokens: 0, cached_tokens: 0, cost_usd: 0, cost_known: false, error_message: '',
    input_bytes: 10, output_bytes: 10, reasoning_bytes: 0, redacted: false, ...o,
  };
};
const root = () => step({ step_id: 'root', parent_step_id: '', type: 'agent', name: 'agent run' });
const failed = { status: 'failed', agent: 'support', duration_ms: 900 } as Run;
const ok = { status: 'ok', agent: 'support', duration_ms: 900 } as Run;

describe('likely cause', () => {
  it('blames an empty tool result that a later LLM call answered without', () => {
    const steps = [
      root(),
      step({ step_id: 'l1', type: 'llm_call', name: 'plan' }),
      step({ step_id: 't1', name: 'lookup_order', tool_name: 'lookup_order', output_bytes: 0 }),
      step({ step_id: 'l2', type: 'llm_call', name: 'answer' }),
      step({ step_id: 'g1', type: 'guardrail', name: 'grounding check', status: 'failure', error_message: 'answer not grounded' }),
    ];
    const c = likelyCause(failed, steps)!;
    expect(c).toMatchObject({ stepId: 't1', kind: 'empty_result', consumerId: 'l2', stepNumber: 3 });
    expect(c.text).toBe('Likely cause: step 3 — lookup_order returned nothing, and step 4 answered without it.');
    const story = buildStory(failed, steps);
    expect(story.causeId).toBe('t1');
    expect(story.lines.map((l) => l.text)).toContain(c.text);
  });

  it('ignores empty results when content capture is off', () => {
    const steps = [root(), step({ step_id: 't1', output_bytes: 0, input_bytes: 0 }), step({ step_id: 'l1', type: 'llm_call', output_bytes: 0, input_bytes: 0, status: 'failure' })];
    expect(likelyCause(failed, steps)).toBeNull();
  });

  it('blames bad arguments', () => {
    const steps = [
      root(),
      step({ step_id: 'l1', type: 'llm_call' }),
      step({ step_id: 't1', tool_name: 'create_refund', status: 'failure', error_message: 'ValidationError: missing required parameter order_id' }),
    ];
    expect(likelyCause(failed, steps)).toMatchObject({ stepId: 't1', kind: 'bad_arguments' });
  });

  it('blames a cut-off answer, even on a run that succeeded', () => {
    const steps = [root(), step({ step_id: 'l1', type: 'llm_call', name: 'draft', finish_reason: 'length' })];
    const c = likelyCause(ok, steps)!;
    expect(c.kind).toBe('truncated');
    expect(c.text).toContain('hit its output token limit (finish reason “length”)');
    expect(buildStory(ok, steps).tone).toBe('warning');
    // an earlier cut-off that a later call recovered from is fine on a successful run
    expect(likelyCause(ok, [root(), step({ type: 'llm_call', step_id: 'a', finish_reason: 'max_tokens' }), step({ type: 'llm_call', step_id: 'b', finish_reason: 'stop' })])).toBeNull();
  });

  it('blames the first of repeated identical failures', () => {
    const steps = [
      root(),
      step({ step_id: 'f1', tool_name: 'charge', status: 'failure', error_message: 'card declined' }),
      step({ step_id: 'f2', tool_name: 'charge', status: 'failure', error_message: 'card declined', attempt: 1 }),
      step({ step_id: 'f3', tool_name: 'charge', status: 'failure', error_message: 'card declined', attempt: 2 }),
    ];
    const c = likelyCause(failed, steps)!;
    expect(c).toMatchObject({ stepId: 'f1', kind: 'repeated_failure' });
    expect(c.text).toContain('failed the same way 3 times: “card declined”');
  });

  it('blames a huge tool result when the prompt overflows', () => {
    const steps = [
      root(),
      step({ step_id: 't1', tool_name: 'fetch_logs', output_bytes: 240_000 }),
      step({ step_id: 'l1', type: 'llm_call', status: 'failure', error_message: 'prompt is too long: 230000 tokens > 200000 maximum' }),
    ];
    expect(likelyCause(failed, steps)).toMatchObject({ stepId: 't1', kind: 'context_overflow', consumerId: 'l1' });
  });

  it('blames a failed tool the model carried on without', () => {
    const steps = [
      root(),
      step({ step_id: 't1', tool_name: 'search', status: 'failure', error_message: 'timeout' }),
      step({ step_id: 'l1', type: 'llm_call' }),
    ];
    expect(likelyCause(failed, steps)).toMatchObject({ stepId: 't1', kind: 'tool_error', consumerId: 'l1' });
    // ...but not when a retry succeeded first
    const retried = [...steps.slice(0, 2), step({ step_id: 't2', tool_name: 'search', attempt: 1 }), step({ step_id: 'l2', type: 'llm_call' })];
    expect(likelyCause(failed, retried)).toBeNull();
  });

  it('flags a prompt that jumped in size before a failed call', () => {
    const steps = [
      root(),
      step({ step_id: 'l1', type: 'llm_call', input_tokens: 4_000 }),
      step({ step_id: 'l2', type: 'llm_call', input_tokens: 60_000, status: 'failure', error_message: 'overloaded' }),
    ];
    expect(likelyCause(failed, steps)?.text).toBe('Likely cause: step 3 — its prompt jumped from 4,000 to 60,000 tokens before it failed.');
  });

  it('says nothing for a clean run', () => {
    expect(likelyCause(ok, [root(), step({ type: 'llm_call', finish_reason: 'stop' })])).toBeNull();
    expect(likelyCause(ok, [])).toBeNull();
  });
});
