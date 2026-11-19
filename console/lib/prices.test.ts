import { describe, expect, it } from 'vitest';
import { covers, emptyRow, fromAWS, parseRows, toRow } from './prices';

describe('model prices form', () => {
  it('parses rows', () => {
    const r = parseRows([{ provider: 'OpenAI', model: ' gpt-x ', input: '$1.25', output: '10', cached: '', note: ' contract ' }, emptyRow()]);
    expect(r.error).toBeUndefined();
    expect(r.prices).toEqual([{ provider: 'openai', model: 'gpt-x', input_per_mtok: 1.25, output_per_mtok: 10, cached_input_per_mtok: null, note: 'contract' }]);
    expect(parseRows([toRow(r.prices[0]!)]).prices).toEqual(r.prices);
  });
  it('explains bad input', () => {
    expect(parseRows([{ ...emptyRow(), input: '1' }]).error).toBe('Row 1: enter a model ID');
    expect(parseRows([{ ...emptyRow('', 'm'), output: '1' }]).error).toBe('Row 1: enter the input price');
    expect(parseRows([{ ...emptyRow('', 'a b'), input: '1', output: '1' }]).error).toContain('no spaces');
    expect(parseRows([{ ...emptyRow('', 'm'), input: '-1', output: '1' }]).error).toContain('between 0 and 10,000');
    const row = { ...emptyRow('', 'M'), input: '1', output: '1' };
    expect(parseRows([row, { ...row, model: 'm' }]).error).toBe('Row 2: m is listed twice');
  });
  it('matches models like the server', () => {
    expect(covers({ provider: '', model: 'gpt-x' }, { provider: 'openai', model: 'gpt-x-2026-01-01' })).toBe(true);
    expect(covers({ provider: '', model: 'gpt-x' }, { provider: 'openai', model: 'gpt-x-mini' })).toBe(false);
    expect(covers({ provider: 'anthropic', model: 'c' }, { provider: 'openai', model: 'c' })).toBe(false);
  });
  it('turns an AWS list price into a row to complete', () => {
    const r = fromAWS({ model: 'Claude Opus 4.5', provider: 'Anthropic', tier: 'global', input_per_mtok: 5, output_per_mtok: 25, cache_read_per_mtok: 0.5,
      cache_write_per_mtok: 6.25, region: 'us-east-1', source: 's', as_of: '2026-11-09' });
    expect(r).toMatchObject({ model: '', input: '5', output: '25', cached: '0.5' });
    expect(r.note).toBe('AWS list price: Anthropic Claude Opus 4.5, global, us-east-1, as of 2026-11-09');
    expect(parseRows([r]).error).toBe('Row 1: enter a model ID');
  });
});
