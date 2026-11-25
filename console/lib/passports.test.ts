import { describe, expect, it } from 'vitest';
import { driftDetail, driftItems, driftLabel, driftLines, passportFacts, passportSentence, type PassportVersion } from './passports';
import type { StepSummary } from './types';

const version: PassportVersion = {
  agent: 'refunds',
  version: 3,
  sha256: 'ab',
  pushed_by: 'key:mkey_1',
  pushed_at: '2026-11-24T10:00:00Z',
  passport: {
    version: 'bindle.passport/v1',
    agent: 'refunds',
    owner: 'payments',
    tools: { allow: [] },
    data_classes: { allow: null, egress_block: ['CARD'] },
    limits: [{ tool: 'refund_payment', max: 3, window: '5m' }],
    budget: { per_day_usd: 200 },
    release_rights: 'none',
  },
};

describe('passport drift', () => {
  it('labels kinds and details', () => {
    expect(driftLabel('egress_blocked')).toBe('Blocked data sent out');
    expect(driftLabel('something_new')).toBe('something new');
    expect(driftDetail('tool_not_allowed:mcp:orders/get')).toBe('mcp:orders/get');
    expect(driftDetail('nothing')).toBe('');
  });

  it('makes Home items for per-step drift and for limits', () => {
    const items = driftItems({
      signals: [
        {
          kind: 'egress_blocked',
          agent: 'refunds',
          steps: 2,
          runs: 1,
          last_seen: '',
          reasons: { 'egress_blocked:CARD': 2 },
          examples: [{ trace_id: 't1', step_id: 's1', step_name: 'send', reasons: [] }],
        },
      ],
      findings: [{ kind: 'rate_over_limit', agent: 'refunds', detail: 'refund_payment', value: 4, limit: 3, window: '5m', summary: 'refunds called refund_payment 4 times in the last 5m (passport limit 3)' }],
    });
    expect(items).toHaveLength(2);
    expect(items[0]).toMatchObject({ severity: 'critical', title: 'refunds: blocked data sent out', href: '/traces/t1?step=s1' });
    expect(items[0]!.detail).toBe('2 steps in 1 run (CARD ×2). Observed against its passport; nothing was blocked.');
    expect(items[1]).toMatchObject({ severity: 'warning', href: '/agents?agent=refunds', cta: 'Open agent' });
  });

  it('adds run-summary lines, most severe first, at most three', () => {
    const step = (id: string, drift: string[]): StepSummary => ({ step_id: id, name: `step ${id}`, passport_drift: drift }) as unknown as StepSummary;
    const lines = driftLines([
      step('a', ['tool_not_allowed:x']),
      step('b', ['release_not_allowed:deploy_prod']),
      step('c', ['data_class_not_allowed:EMAIL', 'egress_blocked:CARD']),
    ]);
    expect(lines).toHaveLength(3);
    expect(lines[0]).toMatchObject({ tone: 'critical', stepId: 'b' });
    expect(lines[0]!.text).toBe('step b: release action without rights (deploy_prod). Not blocked; the passport is observe-only.');
    expect(lines.map((l) => l.stepId)).toEqual(['b', 'c', 'a']);
  });
});

describe('passport summary', () => {
  it('says when there is no passport', () => {
    expect(passportSentence('refunds', null)).toBe("refunds has no passport. Bindle doesn't check what it may do until one is pushed.");
  });

  it('counts drift for the agent only', () => {
    const drift = {
      signals: [
        { kind: 'tool_not_allowed', agent: 'refunds', steps: 4, runs: 2, last_seen: '', reasons: {}, examples: [] },
        { kind: 'tool_not_allowed', agent: 'other', steps: 9, runs: 2, last_seen: '', reasons: {}, examples: [] },
      ],
      findings: [{ kind: 'day_over_budget', agent: 'refunds', detail: '2026-11-24', value: 201, limit: 200, summary: '' }],
    };
    expect(passportSentence('refunds', version, drift)).toBe(
      'Passport v3, owned by payments. 4 steps outside it and 1 limit exceeded in this period (observed, not blocked).',
    );
    expect(passportSentence('refunds', version, { signals: [], findings: [] })).toBe('Passport v3, owned by payments. No drift in this period.');
  });

  it('lists what the passport declares', () => {
    const facts = passportFacts(version.passport);
    expect(facts).toContainEqual({ label: 'Tools', value: 'none allowed' });
    expect(facts).toContainEqual({ label: 'Never sent out', value: 'CARD' });
    expect(facts).toContainEqual({ label: 'Rate limit', value: 'refund_payment: at most 3 per 5m' });
    expect(facts).toContainEqual({ label: 'Daily budget', value: '$200' });
    expect(facts.find((f) => f.label === 'Data classes')).toBeUndefined(); // allow: null is "not declared"
  });
});
