import { describe, expect, it } from 'vitest';
import { contextProfile } from './context';
import type { StepSummary } from './types';

const step = (o: Partial<StepSummary>): StepSummary =>
  ({ step_id: 's', parent_step_id: '', name: 'chat', type: 'llm_call', status: 'success', start_time: '2026-11-17T00:00:00Z', duration_ms: 1,
    provider: '', model: 'm', tool_name: '', input_tokens: 0, output_tokens: 0, cached_tokens: 0, cost_usd: 0, cost_known: true,
    error_message: '', input_bytes: 0, output_bytes: 0, reasoning_bytes: 0, redacted: false, attempt: 0, ...o }) as StepSummary;

describe('context profile', () => {
  it('needs two LLM calls', () => {
    expect(contextProfile([step({ input_tokens: 10 })])).toBeNull();
  });
  it('orders calls, totals cache use and flags growth, low caching and big tool results', () => {
    const p = contextProfile([
      step({ step_id: 'c', start_time: '2026-11-17T00:00:03Z', input_tokens: 9000, cached_tokens: 100 }),
      step({ step_id: 'a', start_time: '2026-11-17T00:00:01Z', input_tokens: 1000 }),
      step({ step_id: 'b', start_time: '2026-11-17T00:00:02Z', input_tokens: 4000, cached_tokens: 900 }),
      step({ step_id: 't', type: 'tool_call', name: 'search', tool_name: 'search', output_bytes: 48_000 }),
    ])!;
    expect(p.calls.map((c) => c.stepId)).toEqual(['a', 'b', 'c']);
    expect(p).toMatchObject({ peak: 9000, first: 1000, last: 9000, input: 14000, cached: 1000 });
    expect(p.hints[0]).toContain('grew 9× during the run, from 1K to 9K tokens');
    expect(p.hints[1]).toContain('Only 7% of input came from the prompt cache');
    expect(p.hints[2]).toContain('search returned 48 KB');
    expect(p.bigResults[0]).toEqual({ stepId: 't', name: 'search', bytes: 48000 });
  });
  it('stays quiet for a well-cached, flat run', () => {
    const p = contextProfile([1, 2, 3].map((i) => step({ step_id: String(i), start_time: `2026-11-17T00:00:0${i}Z`, input_tokens: 5000, cached_tokens: 4000 })))!;
    expect(p.hints).toEqual([]);
    expect(p.cacheRate).toBe(0.8);
  });
});
