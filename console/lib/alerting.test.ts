import { describe, expect, it } from 'vitest';
import { deliverySummary, describeRule, parseRule, thresholdInput } from './alerting';

describe('parseRule', () => {
  it('converts failure-rate percent to a fraction', () => {
    expect(parseRule('error_rate', '20%', '60', '10')).toEqual({ threshold: 0.2, window_minutes: 60, min_runs: 10 });
    expect(parseRule('error_rate', '12.5', '15', '')).toEqual({ threshold: 0.125, window_minutes: 15, min_runs: 0 });
  });
  it('rejects bad input', () => {
    expect(parseRule('error_rate', '150', '60', '')).toHaveProperty('error');
    expect(parseRule('error_rate', '', '60', '')).toHaveProperty('error');
    expect(parseRule('runaway_loop', '2.5', '60', '')).toHaveProperty('error');
    expect(parseRule('runaway_loop', '1', '60', '')).toHaveProperty('error');
    expect(parseRule('run_cost', '0', '60', '')).toHaveProperty('error');
    expect(parseRule('run_cost', '5', '7', '')).toHaveProperty('error');
    expect(parseRule('error_rate', '5', '60', '-1')).toHaveProperty('error');
  });
  it('accepts dollars and repeats', () => {
    expect(parseRule('run_cost', '$2.50', '60', '')).toEqual({ threshold: 2.5, window_minutes: 60, min_runs: 0 });
    expect(parseRule('runaway_loop', '5', '1440', '9')).toEqual({ threshold: 5, window_minutes: 1440, min_runs: 0 });
  });
});

describe('display', () => {
  it('round-trips the failure rate', () => {
    expect(thresholdInput('error_rate', 0.2)).toBe('20');
    expect(thresholdInput('run_cost', 2.5)).toBe('2.5');
  });
  it('describes rules', () => {
    expect(describeRule({ kind: 'error_rate', threshold: 0.2, window_minutes: 60, min_runs: 20 })).toBe(
      '20%+ of runs fail over 1 hour (at least 20 runs)',
    );
    expect(describeRule({ kind: 'run_cost', threshold: 5, window_minutes: 15, min_runs: 0 })).toContain('$5.00');
  });
  it('summarizes delivery', () => {
    expect(deliverySummary({})).toEqual({ ok: false, text: 'No channels' });
    expect(deliverySummary({ a: 'sent', b: 'sent' })).toEqual({ ok: true, text: 'Sent (2)' });
    expect(deliverySummary({ a: 'sent', b: 'failed: HTTP 500' })).toEqual({ ok: false, text: 'Sent to 1 of 2' });
  });
});

describe('quality and digest rules', () => {
  it('parses and describes the new kinds', async () => {
    const { describeRule, parseRule } = await import('./alerting');
    expect(parseRule('negative_feedback', '30', '60', '10')).toEqual({ threshold: 0.3, window_minutes: 60, min_runs: 10 });
    expect(parseRule('cost_regression', '25%', '1440', '')).toEqual({ threshold: 0.25, window_minutes: 1440, min_runs: 0 });
    expect(parseRule('new_error', '1.5', '60', '')).toEqual({ error: 'Failures must be a whole number, at least 1.' });
    expect(parseRule('weekly_digest', '1', '1440', '')).toEqual({ threshold: 1, window_minutes: 1440, min_runs: 0 });
    expect(describeRule({ kind: 'cost_regression', threshold: 0.25, window_minutes: 1440, min_runs: 0 })).toContain('25%+ more per run');
    expect(describeRule({ kind: 'weekly_digest', threshold: 1, window_minutes: 1440, min_runs: 0 })).toBe('Every Monday at 09:00 UTC');
  });
});
