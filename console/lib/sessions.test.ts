import { describe, expect, it } from 'vitest';
import { agentText, feedbackLabel, outcome, userText } from './sessions';

const fb = (positive = 0, negative = 0) => ({ positive, negative, count: positive + negative });

describe('sessions', () => {
  it('labels how a session went', () => {
    expect(outcome({ feedback: fb(0, 1), last_failed: false, failed_runs: 0 }).label).toBe('Unhappy user');
    expect(outcome({ feedback: fb(), last_failed: true, failed_runs: 1 }).label).toBe('Ended in a failure');
    expect(outcome({ feedback: fb(), last_failed: false, failed_runs: 1 }).label).toBe('Recovered');
    expect(outcome({ feedback: fb(2, 1), last_failed: false, failed_runs: 0 }).label).toBe('Happy user');
    expect(outcome({ feedback: fb(), last_failed: false, failed_runs: 0 }).label).toBe('OK');
  });
  it('pulls the user message and the answer out of captured content', () => {
    const input = JSON.stringify([
      { role: 'system', content: 'Be brief.' },
      { role: 'user', content: 'Hi' },
      { role: 'assistant', content: 'Hello!' },
      { role: 'user', content: 'Where is my refund?' },
    ]);
    expect(userText(input)).toBe('Where is my refund?');
    expect(userText('plain text')).toBe('plain text');
    expect(agentText(JSON.stringify([{ role: 'assistant', content: 'Issued on Nov 12.' }]))).toBe('Issued on Nov 12.');
  });
  it('summarizes feedback', () => {
    expect(feedbackLabel(fb(2, 1))).toBe('👍 2 · 👎 1');
    expect(feedbackLabel({ positive: 0, negative: 0, count: 1, average: 4 })).toBe('rated 4');
    expect(feedbackLabel(undefined)).toBeNull();
  });
});
