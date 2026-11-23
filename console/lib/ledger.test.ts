import { describe, expect, it } from 'vitest';
import { asLedgerDays, entryCost, ledgerSentence, shortHash, summaryHref, utcDay, utcStamp, type LedgerEntry } from './ledger';

describe('ledgerSentence', () => {
  it('says the ledger is intact, with the entry count', () => {
    const s = ledgerSentence({ ok: true, entries: 1204, from: '2026-10-25', to: '2026-11-23', first_seq: 1, last_seq: 1204 }, 30);
    expect(s.tone).toBe('good');
    expect(s.title).toBe('Ledger intact for the last 30 days: 1,204 entries');
    expect(s.detail).toContain('entries 1–1,204');
  });

  it('names the first broken entry and when', () => {
    const s = ledgerSentence(
      { ok: false, entries: 811, from: '', to: '', first_broken: { seq: 812, at: '2026-11-03T14:02:09Z', reason: "its hash doesn't match its contents" } },
      30,
    );
    expect(s.tone).toBe('critical');
    expect(s.title).toBe('Ledger broken at entry 812 (Nov 3, 14:02 UTC)');
    expect(s.detail.startsWith("Its hash doesn't match its contents.")).toBe(true);
  });

  it('handles an empty period and a missing time', () => {
    expect(ledgerSentence({ ok: true, entries: 0, from: '', to: '' }, 1).title).toBe('No ledger entries in today');
    const s = ledgerSentence({ ok: false, entries: 2, from: '', to: '', first_broken: { seq: 3, at: '0001-01-01T00:00:00Z', reason: 'entry is missing from the chain' } }, 7);
    expect(s.title).toBe('Ledger broken at entry 3');
  });
});

describe('helpers', () => {
  it('formats stamps, hashes, days and links', () => {
    expect(utcStamp('2026-11-23T09:05:00Z')).toBe('Nov 23, 09:05 UTC');
    expect(utcStamp(undefined)).toBe('');
    expect(shortHash('a'.repeat(64))).toBe('aaaaaaaaaaaa…');
    expect(utcDay(1, Date.parse('2026-11-23T00:30:00Z'))).toBe('2026-11-22');
    expect(summaryHref('refund agent', '2026-11-22')).toBe('/console-api/agents/ledger/summary?agent=refund+agent&day=2026-11-22');
    expect(asLedgerDays('7')).toBe(7);
    expect(asLedgerDays('9999')).toBe(30);
  });

  it('never shows unknown model cost as $0', () => {
    const base: LedgerEntry = { agent: 'a', seq: 1, day: '', at: '', kind: 'step', input_tokens: 0, output_tokens: 0, cost_usd: '0', cost_known: false, prev_hash: '', hash: '' };
    expect(entryCost({ ...base, step_type: 'llm_call' })).toBe('Unknown');
    expect(entryCost({ ...base, step_type: 'tool_call' })).toBe('—');
    expect(entryCost({ ...base, cost_known: true, cost_usd: '0.0025' })).toBe('$0.0025');
    expect(entryCost({ ...base, cost_known: true, cost_usd: '12.5' })).toBe('$12.50');
  });
});
