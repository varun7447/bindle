import { describe, expect, it } from 'vitest';
import { measurementLine, parseSkillRef, proofLine, releaseState, rollbackItems, shippingSentence, skillsLine, type Measurement, type Proof, type Release } from './shipping';

const rel = (over: Partial<Release> = {}): Release => ({
  path: 'skills/retry-refund/SKILL.md',
  release: 2,
  sha256: 'a'.repeat(64),
  state: 'active',
  released_by: 'user:u',
  released_at: '2026-11-20T00:00:00Z',
  serving: true,
  ...over,
});

const side = (runs: number, rate: number, tokens = 1000) => ({ runs, successes: Math.round(runs * rate), success_rate: rate, recurred: 0, recurrence: 0, avg_tokens: tokens, avg_ms: 200 });

describe('shipping', () => {
  it('says how skills reach the agent', () => {
    const s = { agent: 'refunds', ship: false, kill_all: false, killed: [] as string[] };
    expect(shippingSentence('refunds', s, [rel()])).toMatch(/shipping is off/);
    expect(shippingSentence('refunds', { ...s, ship: true }, [rel()])).toBe('1 skill is shipping to refunds.');
    expect(shippingSentence('refunds', { ...s, ship: true, kill_all: true }, [rel()])).toMatch(/switched off/);
    expect(shippingSentence('refunds', { ...s, ship: true, killed: [rel().path] }, [rel()])).toMatch(/no approved skill is shipping/);
  });

  it('describes releases and measurements', () => {
    expect(releaseState(rel(), [])).toBe('Shipping');
    expect(releaseState(rel({ serving: false }), [])).toBe('Replaced by a newer release');
    expect(releaseState(rel({ state: 'rolled_back', state_reason: 'success fell from 80% to 60%' }), [])).toBe('Rolled back: success fell from 80% to 60%');
    expect(releaseState(rel(), [rel().path])).toBe('Switched off');
    expect(measurementLine(undefined)).toMatch(/Not measured yet/);
    const m: Measurement = { agent: 'refunds', path: rel().path, release: 2, with: side(30, 0.9, 1300), without: side(30, 0.8), decision: 'keep', reason: '', computed_at: '' };
    expect(measurementLine(m)).toBe('Success 90% with it, 80% without (30 and 30 runs); +30% tokens per run.');
    expect(measurementLine({ ...m, decision: 'too_few_runs', with: side(4, 1) })).toBe('Measuring: 4 runs with it, 30 without (20 of each needed).');
  });

  it('summarizes proofs and rollbacks for Home', () => {
    const p: Proof = {
      id: 1, sha256: 'a', passed: false, uploaded_by: 'key:m', uploaded_at: '',
      verdict: { total: 5, fixed: 3, fixed_rate: 0.6, threshold: 80, regressions: 0, passed: false, reason: 'fixed 3 of 5 replayed failures (60%); 80% needed' },
      report: { agent: 'refunds', path: rel().path, sha256: 'a', lesson: 'les_1', runs: [], eval_runs: 0, regressions: 0, gaps: ['x'] },
    };
    expect(proofLine(p)).toBe("✕ Failed: fixed 3 of 5 replayed failures (60%); 80% needed. 1 thing replay couldn't cover.");
    const [item] = rollbackItems([{ agent: 'refunds', path: rel().path, release: 3, reason: 'success fell from 80% to 60%', at: '' }]);
    expect(item!.title).toBe('refunds: skill retry-refund release 3 was rolled back');
    expect(item!.detail).toBe('success fell from 80% to 60%. The release before it is shipping again.');
    expect(item!.href).toContain('path=skills%2Fretry-refund%2FSKILL.md');
  });
});

describe('skills in runs', () => {
  it('names the learned skills a run loaded', () => {
    expect(parseSkillRef('skills/retry-refund/SKILL.md@2')).toEqual({ path: 'skills/retry-refund/SKILL.md', name: 'retry-refund', release: 2 });
    expect(parseSkillRef('notes/x.md@1')).toBeNull();
    expect(skillsLine([{ step_id: 's0' }])).toBeNull();
    const line = skillsLine([
      { step_id: 's1', skills_used: ['skills/retry-refund/SKILL.md@2'] },
      { step_id: 's2', skills_used: ['skills/retry-refund/SKILL.md@2', 'skills/check-address/SKILL.md@1', 'junk'] },
    ]);
    expect(line).toEqual({ tone: 'neutral', text: 'It loaded 2 learned skills: retry-refund (release 2), check-address (release 1).', stepId: 's1' });
  });
});
