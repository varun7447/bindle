import { describe, expect, it } from 'vitest';
import { segments } from './search';

describe('search snippets', () => {
  it('splits matches from text', () => {
    expect(segments('call \uE000timeout\uE001 after \uE00030s\uE001')).toEqual([
      { text: 'call ', match: false },
      { text: 'timeout', match: true },
      { text: ' after ', match: false },
      { text: '30s', match: true },
    ]);
    expect(segments('no marks')).toEqual([{ text: 'no marks', match: false }]);
    expect(segments('\uE000open')).toEqual([{ text: 'open', match: true }]);
    expect(segments('')).toEqual([]);
  });
});
