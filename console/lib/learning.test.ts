import { describe, expect, it } from 'vitest';
import { learningSentence, lessonItems, nextStep, noteHref, scanReason, targetHref, vaultGraph, type Lesson, type VaultNote } from './learning';

const lesson = (agent: string, runs: number): Lesson => ({
  id: 'les_0123456789abcdef',
  agent,
  step_name: 'lookup_order',
  tool: 'lookup_order',
  error_pattern: 'order <N> not found',
  cause: 'not_found',
  title: `lookup_order failed in ${runs} runs`,
  runs,
  failures: runs + 1,
  last_seen: '2026-11-20T00:00:00Z',
  evidence: ['0123456789abcdef0123456789abcdef'],
});

const skill = (status: VaultNote['status'], over: Partial<VaultNote> = {}): VaultNote => ({
  agent: 'refund',
  path: 'skills/retry/SKILL.md',
  kind: 'skill',
  status,
  version: 1,
  sha256: 'a'.repeat(64),
  scan: { flagged: false, reasons: [] },
  passport_check: { passport: true, ok: true },
  updated_by: 'key:mkey_1',
  updated_at: '2026-11-20T00:00:00Z',
  ...over,
});

describe('learningSentence', () => {
  it('says when there is nothing to learn', () => {
    expect(learningSentence('refund', [], [])).toBe('refund has no repeated failures to learn from yet.');
  });
  it('leads with lessons and what is waiting on a person', () => {
    const s = learningSentence('refund', [lesson('refund', 6), lesson('refund', 5)], [skill('proven'), skill('draft', { scan: { flagged: true, reasons: [] } })]);
    expect(s).toBe(
      'refund: 2 lessons from repeated failures (most often: lookup_order, 6 runs); 2 skills (1 waiting for approval, 1 flagged by the security scan). Nothing reaches the agent in this phase.',
    );
  });
});

describe('nextStep', () => {
  it('names what blocks approval', () => {
    expect(nextStep(skill('proven'), 'owner@example.com', false)).toMatch(/passport/);
    expect(nextStep(skill('proven', { scan: { flagged: true, reasons: [] } }), 'o', true)).toMatch(/security reviewer/);
    expect(nextStep(skill('proven', { scan: { flagged: true, reasons: [] }, reviewed_sha256: 'a'.repeat(64) }), 'owner@example.com', true)).toBe(
      'Waiting for owner@example.com to approve.',
    );
    expect(nextStep(skill('draft'), 'o', true)).toMatch(/proof/);
    expect(nextStep({ ...skill('draft'), kind: 'note', status: undefined }, 'o', true)).toBe('');
  });
});

describe('graph and links', () => {
  it('lays out notes and targets deterministically', () => {
    const notes = [skill('draft'), { ...skill(undefined), path: 'notes/a.md', kind: 'note' as const, status: undefined }];
    const links = [
      { path: 'notes/a.md', target: 'skills/retry/SKILL.md' },
      { path: 'skills/retry/SKILL.md', target: 'run:0123456789abcdef0123456789abcdef' },
    ];
    const g = vaultGraph(notes, links);
    expect(g.nodes.map((n) => [n.id, n.kind, n.label])).toEqual([
      ['notes/a.md', 'note', 'a'],
      ['skills/retry/SKILL.md', 'skill', 'retry'],
      ['run:0123456789abcdef0123456789abcdef', 'run', 'run 01234567'],
    ]);
    expect(g.edges).toHaveLength(2);
    expect(vaultGraph(notes, links)).toEqual(g);
  });
  it('opens link targets in the right place', () => {
    expect(targetHref('run:0123456789abcdef0123456789abcdef', 'refund')).toBe('/traces/0123456789abcdef0123456789abcdef');
    expect(targetHref('notes/a.md', 'refund')).toBe(noteHref('refund', 'notes/a.md'));
    expect(noteHref('a b', 'notes/x.md')).toBe('/agents/learning/note?agent=a+b&path=notes%2Fx.md');
  });
});

describe('Home items and scan reasons', () => {
  it('turns lessons into one info item per agent', () => {
    const items = lessonItems([lesson('refund', 6), lesson('refund', 5), lesson('', 7)]);
    expect(items).toHaveLength(2);
    expect(items[0]!.title).toBe('refund: 2 lessons from repeated failures');
    expect(items[0]!.severity).toBe('info');
    expect(items[1]!.title).toBe('Unnamed agent: 1 lesson from repeated failures');
  });
  it('explains scan reasons in plain words', () => {
    expect(scanReason('prompt_injection:ignore_instructions')).toBe('tells the model to ignore its instructions');
    expect(scanReason('x:unknown')).toBe('x: unknown');
  });
});
