// The context view for one run: how big each LLM call's prompt was, how much
// was served from the prompt cache, and which tool results grew it. Pure;
// unit-tested. Token counts come from the provider's usage report.

import type { StepSummary } from './types';

export interface ContextCall {
  stepId: string;
  name: string;
  model: string;
  input: number; // all input tokens (cached included)
  cached: number;
  output: number;
}

export interface ContextProfile {
  calls: ContextCall[];
  peak: number;
  first: number;
  last: number;
  input: number;
  cached: number;
  cacheRate: number; // cached / input over the run
  bigResults: { stepId: string; name: string; bytes: number }[];
  hints: string[];
}

/** Below this many LLM calls a context profile says little. */
export const MIN_CALLS = 2;
const LOW_CACHE = 0.3;
const GROWTH = 3;

export function contextProfile(steps: StepSummary[]): ContextProfile | null {
  const calls = steps
    .filter((s) => s.type === 'llm_call' && s.input_tokens > 0)
    .sort((a, b) => a.start_time.localeCompare(b.start_time))
    .map((s) => ({ stepId: s.step_id, name: s.name, model: s.model, input: s.input_tokens, cached: Math.min(s.cached_tokens, s.input_tokens), output: s.output_tokens }));
  if (calls.length < MIN_CALLS) return null;
  const input = calls.reduce((n, c) => n + c.input, 0);
  const cached = calls.reduce((n, c) => n + c.cached, 0);
  const peak = Math.max(...calls.map((c) => c.input));
  const first = calls[0]!.input;
  const last = calls[calls.length - 1]!.input;
  const cacheRate = input ? cached / input : 0;
  const bigResults = steps
    .filter((s) => (s.type === 'tool_call' || s.type === 'retrieval') && s.output_bytes > 0)
    .sort((a, b) => b.output_bytes - a.output_bytes)
    .slice(0, 3)
    .map((s) => ({ stepId: s.step_id, name: s.tool_name || s.name, bytes: s.output_bytes }));

  const hints: string[] = [];
  if (last >= first * GROWTH && calls.length >= 3) {
    hints.push(`The prompt grew ${Math.round(last / first)}× during the run, from ${fmt(first)} to ${fmt(last)} tokens: each call resends the conversation so far.`);
  }
  if (calls.length >= 3 && cacheRate < LOW_CACHE) {
    hints.push(
      cached === 0
        ? 'No input came from the prompt cache. If calls share a long, stable prefix (system prompt, tools, earlier turns), prompt caching could cut input cost.'
        : `Only ${Math.round(cacheRate * 100)}% of input came from the prompt cache. Keeping the prompt prefix stable between calls raises the hit rate.`,
    );
  }
  const big = bigResults[0];
  if (big && big.bytes >= 20_000) {
    hints.push(`${big.name} returned ${fmtBytes(big.bytes)}, which later calls carry in their prompt. Trimming or summarizing large tool results keeps context small.`);
  }
  return { calls, peak, first, last, input, cached, cacheRate, bigResults, hints };
}

function fmt(n: number): string {
  return n >= 10_000 ? `${Math.round(n / 1000)}K` : n >= 1000 ? `${(n / 1000).toFixed(1).replace(/\.0$/, '')}K` : String(n);
}

export function fmtBytes(b: number): string {
  return b >= 1_000_000 ? `${(b / 1_000_000).toFixed(1)} MB` : b >= 1000 ? `${Math.round(b / 1000)} KB` : `${b} B`;
}
