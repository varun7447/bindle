import { describe, expect, it } from 'vitest';
import { eventState, gradingOn, leadGrade, outcomeLabel, parseReversals, type Grade, type OutcomeEvent } from './grading';

const ev = (over: Partial<OutcomeEvent>): OutcomeEvent => ({
  id: 'oev_1', correlation_keys: ['ticket_id'], name: 'ticket_reopened', success: false, source: 'api',
  occurred_at: '2026-11-20T10:00:00Z', received_at: '2026-11-20T10:00:00Z', status: 'pending', attempts: 1,
  next_attempt_at: '2026-11-20T10:01:00Z', expires_at: '2026-11-20T15:00:00Z', matched_traces: [], ...over,
});

describe('grading helpers', () => {
  it('detects the flag', () => {
    expect(gradingOn(['ledger', 'outcome_grading'])).toBe(true);
    expect(gradingOn(['ledger'])).toBe(false);
    expect(gradingOn(undefined)).toBe(false);
  });

  it('parses reversal lists', () => {
    expect(parseReversals('ticket_reopened\nrefund_reversed, ticket_reopened\n\n')).toEqual({ names: ['refund_reversed', 'ticket_reopened'] });
    expect(parseReversals('a'.repeat(81)).error).toBeDefined();
    expect(parseReversals(Array.from({ length: 51 }, (_, i) => `o${i}`).join('\n')).error).toBe('At most 50 reversal outcomes.');
  });

  it('explains event states', () => {
    const now = Date.parse('2026-11-20T10:00:00Z');
    expect(eventState(ev({}), now)).toBe('Waiting for a matching run; retried for 5 more hours.');
    expect(eventState(ev({ status: 'matched', matched_traces: ['a'] }), now)).toBe('Matched to 1 run.');
    expect(eventState(ev({ status: 'unmatched' }), now)).toContain('retry window ended');
    expect(outcomeLabel('ticket_reopened')).toBe('ticket reopened');
  });

  it('leads with a version that had reversals', () => {
    const g = (version: string, reversed: number) => ({ version, reversed }) as Grade;
    expect(leadGrade([g('v13', 0), g('v12', 2)])?.version).toBe('v12');
    expect(leadGrade([g('v13', 0)])?.version).toBe('v13');
    expect(leadGrade([])).toBeUndefined();
  });
});
