// Sessions (conversations) and user feedback. Pure; unit-tested.

import { parseMessages } from './messages';
import type { Run } from './types';

export interface FeedbackSummary {
  positive: number;
  negative: number;
  count: number;
  average?: number;
}

export interface Session {
  session_id: string;
  agent: string;
  runs: number;
  failed_runs: number;
  cost_usd: number;
  cost_partial: boolean;
  steps: number;
  input_tokens: number;
  output_tokens: number;
  duration_ms: number;
  first_start: string;
  last_start: string;
  last_failed: boolean;
  feedback: FeedbackSummary;
}

export interface Turn {
  run: Run;
  input: string;
  output: string;
  feedback?: FeedbackSummary;
}

export interface SessionDetail {
  session_id: string;
  agent: string;
  turns: Turn[];
  truncated: boolean;
  cost_usd: number;
  cost_partial: boolean;
  duration_ms: number;
  feedback?: FeedbackSummary;
}

/** How a session went, in a word, for the list. */
export function outcome(s: Pick<Session, 'feedback' | 'last_failed' | 'failed_runs'>): { label: string; tone: 'ok' | 'warn' | 'failed' } {
  if (s.feedback.negative > 0 && s.feedback.negative >= s.feedback.positive) return { label: 'Unhappy user', tone: 'failed' };
  if (s.last_failed) return { label: 'Ended in a failure', tone: 'failed' };
  if (s.failed_runs > 0) return { label: 'Recovered', tone: 'warn' };
  if (s.feedback.positive > 0) return { label: 'Happy user', tone: 'ok' };
  return { label: 'OK', tone: 'ok' };
}

/** The user's message in a turn: the last user message of the input, or the raw text. */
export function userText(input: string): string {
  const msgs = parseMessages(input, 'input');
  if (msgs) {
    for (let i = msgs.length - 1; i >= 0; i--) {
      const m = msgs[i]!;
      if (m.role !== 'user') continue;
      const t = m.parts.map((p) => (p.kind === 'text' ? p.text : '')).join(' ').trim();
      if (t) return t;
    }
  }
  return input.trim();
}

/** The agent's answer in a turn: the last assistant text, or the raw text. */
export function agentText(output: string): string {
  const msgs = parseMessages(output, 'output');
  if (msgs) {
    for (let i = msgs.length - 1; i >= 0; i--) {
      const m = msgs[i]!;
      if (m.role !== 'assistant') continue;
      const t = m.parts.map((p) => (p.kind === 'text' ? p.text : '')).join(' ').trim();
      if (t) return t;
    }
  }
  return output.trim();
}

export function feedbackLabel(f?: FeedbackSummary): string | null {
  if (!f || f.count === 0) return null;
  const parts: string[] = [];
  if (f.positive) parts.push(`👍 ${f.positive}`);
  if (f.negative) parts.push(`👎 ${f.negative}`);
  if (!parts.length && f.average !== undefined) parts.push(`rated ${Math.round(f.average * 10) / 10}`);
  return parts.join(' · ') || `${f.count} feedback`;
}
