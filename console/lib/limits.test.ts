import { describe, expect, it } from 'vitest';
import { emptyRow, parseRows, rowState, toRow } from './limits';

describe('limits form', () => {
  it('parses rows, dropping blank ones', () => {
    const r = parseRows([
      { agent: ' refunds ', paused: true, maxSteps: '50', maxTokens: '', budget: '$5.50', pauseOverBudget: true, maxRepeats: '3' },
      emptyRow(),
      { ...emptyRow('*'), maxTokens: '200000' },
    ]);
    expect(r.error).toBeUndefined();
    expect(r.limits).toEqual([
      { agent: 'refunds', paused: true, max_steps_per_run: 50, max_tokens_per_run: 0, daily_budget_usd: 5.5, pause_over_budget: true, max_repeat_calls: 3 },
      { agent: '*', paused: false, max_steps_per_run: 0, max_tokens_per_run: 200000, daily_budget_usd: 0, pause_over_budget: false, max_repeat_calls: 0 },
    ]);
  });
  it('explains bad input', () => {
    expect(parseRows([{ ...emptyRow(), maxSteps: '5' }]).error).toBe('Row 1: enter an agent name, or * for all agents');
    expect(parseRows([emptyRow('a'), emptyRow('a')]).error).toBe('Row 2: a is listed twice');
    expect(parseRows([{ ...emptyRow('a'), maxSteps: '1.5' }]).error).toContain('whole number');
    expect(parseRows([{ ...emptyRow('a'), budget: '-1' }]).error).toContain('daily budget');
    expect(parseRows([{ ...emptyRow('a'), pauseOverBudget: true }]).error).toBe('Row 1: set a daily budget to pause over it');
    expect(parseRows([{ ...emptyRow('a'), maxRepeats: '10001' }]).error).toContain('Max repeated calls');
    expect(parseRows([{ ...emptyRow(), maxRepeats: '2' }]).error).toBe('Row 1: enter an agent name, or * for all agents');
  });
  it('round-trips API limits', () => {
    const l = { agent: 'a', paused: false, max_steps_per_run: 0, max_tokens_per_run: 9, daily_budget_usd: 2, pause_over_budget: false, max_repeat_calls: 4 };
    expect(parseRows([toRow(l)]).limits).toEqual([l]);
  });
  it('describes today', () => {
    const base = { agent: 'a', paused: false, daily_budget_usd: 5, pause_over_budget: false };
    expect(rowState({ ...base, paused: true }, { over_budget: [] }).label).toBe('Paused');
    expect(rowState(base, { over_budget: ['a'] })).toEqual({ label: 'Over budget', tone: 'warning' });
    expect(rowState({ ...base, pause_over_budget: true }, { over_budget: ['a'] }).label).toBe('Paused: over budget');
    expect(rowState({ ...base, agent: '*' }, { over_budget: ['x', 'y'] }).label).toBe('2 agents over budget');
    expect(rowState(base, { over_budget: [] }).label).toBe('Running');
  });
});
