import { describe, expect, it } from 'vitest';
import { attentionItems } from './insights';
import { REASON_TEXT, securityItems, securityLines, topReason, type SecuritySignal } from './security';
import { buildStory } from './story';
import { topicRows, topicsSummary, type Topics } from './topics';
import type { Insights, OverviewResponse, Run, StepSummary } from './types';

const sig = (o: Partial<SecuritySignal> = {}): SecuritySignal => ({
  flag: 'prompt_injection',
  agent: 'support',
  steps: 3,
  runs: 2,
  last_seen: '',
  reasons: { 'prompt_injection:ignore_instructions': 3, 'prompt_injection:chat_markup': 1 },
  examples: [{ trace_id: 't1', step_id: 's1', step_name: 'fetch', reasons: ['prompt_injection:ignore_instructions'] }],
  ...o,
});

const step = (o: Partial<StepSummary>): StepSummary =>
  ({ step_id: 's', parent_step_id: 'root', name: 'step', type: 'tool_call', status: 'success', attempt: 0, start_time: '', duration_ms: 1,
    provider: '', model: '', tool_name: '', input_tokens: 0, output_tokens: 0, cached_tokens: 0, cost_usd: 0, cost_known: true,
    error_message: '', input_bytes: 0, output_bytes: 0, reasoning_bytes: 0, redacted: false, ...o }) as StepSummary;

describe('security signals', () => {
  it('turns signals into Home items with plain reasons', () => {
    const [it0, it1] = securityItems([sig(), sig({ flag: 'possible_exfiltration', agent: '', steps: 1, runs: 1, reasons: { 'possible_exfiltration:long_url_query': 1 }, examples: [] })]);
    expect(it0).toMatchObject({
      severity: 'critical',
      title: 'support: possible prompt injection',
      detail: '3 steps in 2 runs: content the agent read tells the agent to ignore its instructions. A signal to review, not a verdict.',
      href: '/traces/t1?step=s1',
    });
    expect(it1).toMatchObject({ severity: 'warning', title: 'Unnamed agent: possible data exfiltration', href: '/runs', cta: 'See runs' });
    expect(it1!.detail).toContain('a tool call sent a URL with a very long query string');
  });

  it('ranks security items with the other attention items', () => {
    const ov = { runs: 5, failed_runs: 0, cost_partial: false, cost_unknown_steps: 0 } as OverviewResponse;
    const ins: Insights = { agents: [], issues: [], previous: { runs: 0, failed_runs: 0, cost_usd: 0, p95_run_ms: 0 } };
    const loops = [{ trace_id: 'x', tool_name: 'search', repeats: 6 }] as never;
    const items = attentionItems(ov, ins, loops, '24h', securityItems([sig()]));
    expect(items.map((i) => i.key)).toEqual(['security:prompt_injection:support', 'loop:x:search']);
  });

  it('picks the most frequent reason, ties alphabetical', () => {
    expect(topReason({ 'a:x': 1, 'a:b': 1, 'a:c': 3 })).toBe('a:c');
    expect(topReason({ 'a:x': 1, 'a:b': 1 })).toBe('a:b');
    expect(topReason({})).toBeUndefined();
  });

  it('adds run-summary lines for flagged steps, most severe first', () => {
    const steps = [
      step({ step_id: 'a', name: 'send_email', security_flags: ['possible_exfiltration'], security_reasons: ['possible_exfiltration:sensitive_data_to_outbound_tool'] }),
      step({ step_id: 'b', name: 'fetch', security_flags: ['prompt_injection', 'secret_exposed'],
        security_reasons: ['prompt_injection:ignore_instructions', 'secret_exposed:credential_in_output'] }),
      step({ step_id: 'c', name: 'plain' }),
    ];
    expect(securityLines(steps)).toEqual([
      { tone: 'critical', text: 'What fetch returned tells the agent to ignore its instructions: possible prompt injection.', stepId: 'b' },
      { tone: 'critical', text: 'In fetch, a credential appeared in the result (redacted before storage).', stepId: 'b' },
      { tone: 'warning', text: 'send_email passed sensitive data to a tool that sends data out: possible data exfiltration.', stepId: 'a' },
    ]);
    const run = { trace_id: 't', status: 'ok', agent: 'a', duration_ms: 5, cost_usd: 0, cost_partial: false, cost_unknown_steps: 0 } as Run;
    const story = buildStory(run, steps);
    expect(story.tone).toBe('warning');
    expect(story.lines.some((l) => l.stepId === 'b' && l.text.includes('prompt injection'))).toBe(true);
  });

  it('explains every backend reason', () => {
    for (const r of ['ignore_instructions', 'reveal_system_prompt', 'conceal_from_user', 'role_override', 'chat_markup', 'hidden_text']) {
      expect(REASON_TEXT[`prompt_injection:${r}`]).toBeTruthy();
    }
    for (const r of ['secret_exposed:credential_in_output', 'possible_exfiltration:long_url_query', 'possible_exfiltration:encoded_blob', 'possible_exfiltration:sensitive_data_to_outbound_tool']) {
      expect(REASON_TEXT[r]).toBeTruthy();
    }
  });
});

describe('topics', () => {
  const t: Topics = {
    rising: [{ term: 'refund', runs: 40, previous_runs: 5, share: 0.4, previous_share: 0.1, example_trace_ids: ['a'] }],
    new: [{ term: 'outage', runs: 1, previous_runs: 0, share: 0.01, previous_share: 0, example_trace_ids: [] }],
    runs: 100,
    previous_runs: 50,
    avg_chars: 150,
    previous_avg_chars: 100,
  };

  it('lists new topics first, then rising ones', () => {
    expect(topicRows(t)).toEqual([
      { term: 'outage', kind: 'new', detail: '1 run (1.0%), not asked about before', share: 0.01, previousShare: 0, examples: [] },
      { term: 'refund', kind: 'rising', detail: '40% of runs, up from 10%', share: 0.4, previousShare: 0.1, examples: ['a'] },
    ]);
  });

  it('summarises volume and message length', () => {
    expect(topicsSummary(t, 'previous 7 days')).toBe('100 runs with a user message, 50 in the previous 7 days, messages are 50% longer on average.');
    expect(topicsSummary({ ...t, previous_runs: 0 }, 'previous 7 days')).toBe('100 runs with a user message, none in the previous 7 days to compare with.');
    expect(topicsSummary({ ...t, runs: 0 }, 'previous 7 days')).toBeNull();
  });
});
