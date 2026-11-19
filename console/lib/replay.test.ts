import { describe, expect, it } from 'vitest';
import { defaultModel, defaultProvider, hasRedactions, toReplayMessages } from './replay';

describe('replay', () => {
  it('turns a captured conversation into editable turns', () => {
    const input = JSON.stringify([
      { role: 'system', content: 'You are a refunds agent.' },
      { role: 'user', content: 'Where is my refund? I am [REDACTED:EMAIL]' },
      { role: 'assistant', content: '', tool_calls: [{ function: { name: 'lookup_order', arguments: '{"email":"x"}' } }] },
      { role: 'tool', content: '{"status":"issued"}' },
    ]);
    const msgs = toReplayMessages(input);
    expect(msgs.map((m) => m.role)).toEqual(['system', 'user', 'assistant', 'tool']);
    expect(msgs[2]!.content).toContain('[called lookup_order with');
    expect(hasRedactions(msgs)).toBe(true);
  });
  it('falls back to one user turn for plain text', () => {
    expect(toReplayMessages('hello there')).toEqual([{ role: 'user', content: 'hello there' }]);
    expect(toReplayMessages('')).toEqual([{ role: 'user', content: '' }]);
  });
  it('picks a provider and model', () => {
    expect(defaultProvider('openai', 'gpt-4o')).toBe('openai');
    expect(defaultProvider('', 'o3-mini')).toBe('openai');
    expect(defaultProvider('anthropic', 'claude-sonnet-4-5')).toBe('anthropic');
    expect(defaultModel('us.anthropic.claude-sonnet-4-5-20250929-v1:0')).toBe('claude-sonnet-4-5-20250929');
    expect(defaultModel('claude-opus-5-5')).toBe('claude-opus-5-5');
    expect(defaultModel('')).toBe('claude-opus-5-5');
    expect(defaultModel('', 'openai')).toBe('');
  });
});
