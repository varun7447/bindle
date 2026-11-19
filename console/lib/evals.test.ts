import { describe, expect, it } from 'vitest';
import { checkMatrix, describeScorer, overall, parseScorerForm, passRate } from './evals';

describe('evals', () => {
  it('describes checks', () => {
    expect(describeScorer({ kind: 'contains', value: 'refund', number: 0, agent: '' })).toBe('output contains “refund”');
    expect(describeScorer({ kind: 'max_cost_usd', value: '', number: 0.05, agent: 'refunds' })).toBe('costs at most $0.05 (refunds only)');
  });
  it('computes pass rates, ignoring skipped results', () => {
    expect(passRate({ passed: 3, failed: 1 })).toBe(0.75);
    expect(passRate({ passed: 0, failed: 0 })).toBeNull();
    expect(
      overall([
        { scorer_id: 'a', name: 'a', passed: 1, failed: 1, skipped: 9 },
        { scorer_id: 'b', name: 'b', passed: 2, failed: 0, skipped: 0 },
      ]),
    ).toBe(0.75);
  });
  it('builds the check-by-version matrix', () => {
    const t = (name: string, passed: number) => ({ scorer_id: name, name, passed, failed: 1, skipped: 0 });
    const m = checkMatrix({
      window: '7d',
      agent: 'a',
      scores: [],
      versions: [
        { version: 'v2', runs_checked: 5, checks: [t('json', 4), t('cheap', 5)] },
        { version: 'v1', runs_checked: 5, checks: [t('json', 2)] },
      ],
    });
    expect(m.versions).toEqual(['v2', 'v1']);
    expect(m.rows.map((r) => r.name)).toEqual(['json', 'cheap']);
    expect(m.rows[1]!.cells[1]).toBeUndefined();
  });
  it('validates the new-check form', () => {
    const base = { name: 'x', kind: 'contains', agent: '', value: 'refund', number: '', case_sensitive: false };
    expect(parseScorerForm(base).scorer).toMatchObject({ kind: 'contains', value: 'refund', number: 0 });
    expect(parseScorerForm({ ...base, value: ' ' }).error).toBe('Enter the text to look for.');
    expect(parseScorerForm({ ...base, kind: 'max_cost_usd', number: '$0.10' }).scorer!.number).toBe(0.1);
    expect(parseScorerForm({ ...base, kind: 'max_steps', number: '0' }).error).toContain('greater than 0');
    expect(parseScorerForm({ ...base, kind: 'nope' }).error).toBe('Choose what to check.');
    expect(parseScorerForm({ ...base, kind: 'json_valid', value: '' }).scorer!.value).toBe('');
  });
});
