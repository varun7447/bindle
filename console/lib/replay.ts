// Step replay form logic. Pure; unit-tested.

import { parseMessages, type Message } from './messages';

export interface ReplayMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content: string;
}

export interface ReplayResponse {
  result: { output: string; model: string; stop_reason: string; input_tokens: number; output_tokens: number; cached_tokens: number; latency_ms: number };
  cost_usd: number;
  cost_known: boolean;
  original: { model: string; output: string; input_tokens: number; output_tokens: number; cached_tokens: number; cost_usd: number; cost_known: boolean; duration_ms: number };
}

const ROLES = new Set(['system', 'user', 'assistant', 'tool']);

function flatten(m: Message): string {
  return m.parts
    .map((p) => (p.kind === 'text' ? p.text : p.kind === 'tool_call' ? `[called ${p.name} with ${p.args}]` : p.text))
    .filter((t) => t.trim() !== '')
    .join('\n\n');
}

/** A captured LLM input as editable turns. Tool calls become text: replay sends no tool definitions. */
export function toReplayMessages(input: string): ReplayMessage[] {
  const parsed = input ? parseMessages(input, 'input') : null;
  if (!parsed || parsed.length === 0) return input.trim() ? [{ role: 'user', content: input }] : [{ role: 'user', content: '' }];
  return parsed
    .map((m) => ({ role: (ROLES.has(m.role) ? m.role : 'user') as ReplayMessage['role'], content: flatten(m) }))
    .filter((m) => m.content.trim() !== '');
}

/** The provider to replay with, from the captured step's provider. */
export function defaultProvider(provider: string, model: string): 'anthropic' | 'openai' {
  const p = `${provider} ${model}`.toLowerCase();
  if (p.includes('openai') || /\bgpt|\bo\d/.test(model.toLowerCase())) return 'openai';
  return 'anthropic';
}

/** Model IDs on partner platforms ("anthropic.claude-…-v1:0", "us.anthropic…") as the provider's own ID, when obvious. */
export function defaultModel(model: string, provider: 'anthropic' | 'openai' = 'anthropic'): string {
  const m = model.replace(/^(?:[a-z]{2}\.)?anthropic\./, '').replace(/-v\d+(?::\d+)?$/, '');
  // No model recorded (or a test stub): suggest a current model for the provider.
  if (!m || m.startsWith('stub')) return provider === 'anthropic' ? 'claude-opus-5-5' : '';
  return m;
}

export function hasRedactions(msgs: ReplayMessage[]): boolean {
  return msgs.some((m) => /\[REDACTED:[A-Z0-9_]+\]/.test(m.content));
}
