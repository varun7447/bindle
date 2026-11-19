// Security signals (prompt injection, exposed credentials, possible
// exfiltration) in plain language. Signals are leads for a person to
// review, never verdicts. Pure; unit-tested.

import { formatCount } from './format';
import type { AttentionItem } from './insights';
import type { StepSummary } from './types';

export type SecurityFlag = 'prompt_injection' | 'secret_exposed' | 'possible_exfiltration';

export interface SecurityExample {
  trace_id: string;
  step_id: string;
  step_name: string;
  tool_name?: string;
  reasons: string[];
}

export interface SecuritySignal {
  flag: string;
  agent: string;
  steps: number;
  runs: number;
  last_seen: string;
  reasons: Record<string, number>;
  examples: SecurityExample[];
}

export interface SecurityResponse {
  window: string;
  signals: SecuritySignal[];
  explanations: Record<string, string>;
}

export const FLAG_LABEL: Record<string, string> = {
  prompt_injection: 'Possible prompt injection',
  secret_exposed: 'Credential exposed',
  possible_exfiltration: 'Possible data exfiltration',
};

/** What each flag means, for the step badge's tooltip and help text. */
export const FLAG_HELP: Record<string, string> = {
  prompt_injection:
    'Content the agent read (a tool result or retrieved document) contains instructions aimed at the model, such as "ignore previous instructions". The agent may have followed them.',
  secret_exposed:
    'A credential (API key, token or private key) appeared in a result. Bindle redacted it before storage; rotate it if it is real.',
  possible_exfiltration:
    'A tool call may be sending data out: a URL with a very long query string, a long encoded blob, or sensitive data passed to a tool that sends messages or requests.',
};

/** Plain-language reasons; kept in step with the backend (security.Explain). */
export const REASON_TEXT: Record<string, string> = {
  'prompt_injection:ignore_instructions': 'tells the agent to ignore its instructions',
  'prompt_injection:reveal_system_prompt': 'asks the agent to reveal its system prompt',
  'prompt_injection:conceal_from_user': 'tells the agent to hide something from the user',
  'prompt_injection:role_override': 'tries to give the agent a new, unrestricted role',
  'prompt_injection:chat_markup': 'contains chat-format control tokens',
  'prompt_injection:hidden_text': 'contains invisible text',
  'secret_exposed:credential_in_output': 'a credential appeared in the result',
  'possible_exfiltration:long_url_query': 'sent a URL with a very long query string',
  'possible_exfiltration:encoded_blob': 'sent a long encoded blob',
  'possible_exfiltration:sensitive_data_to_outbound_tool': 'passed sensitive data to a tool that sends data out',
};

export function flagLabel(flag: string): string {
  return FLAG_LABEL[flag] ?? flag.replace(/_/g, ' ');
}

export function reasonText(reason: string, server?: Record<string, string>): string {
  return REASON_TEXT[reason] ?? server?.[reason] ?? reason.split(':').pop()!.replace(/_/g, ' ');
}

/** The most frequent reason (ties: alphabetical). */
export function topReason(reasons: Record<string, number>): string | undefined {
  return Object.entries(reasons).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))[0]?.[0];
}

const SEVERITY: Record<string, AttentionItem['severity']> = {
  prompt_injection: 'critical',
  secret_exposed: 'critical',
  possible_exfiltration: 'warning',
};

/** Home "needs attention" items: one per flag and agent. */
export function securityItems(signals: SecuritySignal[], explanations?: Record<string, string>): AttentionItem[] {
  return signals.map((s) => {
    const who = s.agent || 'Unnamed agent';
    const why = topReason(s.reasons);
    const ex = s.examples[0];
    const steps = `${formatCount(s.steps)} step${s.steps === 1 ? '' : 's'} in ${formatCount(s.runs)} run${s.runs === 1 ? '' : 's'}`;
    return {
      key: `security:${s.flag}:${s.agent}`,
      severity: SEVERITY[s.flag] ?? 'warning',
      title: `${who}: ${flagLabel(s.flag).toLowerCase()}`,
      detail: `${steps}${why ? `: ${whereText(s.flag)} ${reasonText(why, explanations)}` : ''}. A signal to review, not a verdict.`,
      href: ex ? `/traces/${ex.trace_id}?step=${ex.step_id}` : '/runs',
      cta: ex ? 'Open latest' : 'See runs',
    };
  });
}

function whereText(flag: string): string {
  switch (flag) {
    case 'prompt_injection':
      return 'content the agent read';
    case 'possible_exfiltration':
      return 'a tool call';
    default:
      return '';
  }
}

/** One run-summary line per flagged step (at most three), most severe first. */
export function securityLines(steps: StepSummary[]): { tone: 'critical' | 'warning'; text: string; stepId: string }[] {
  const out: { tone: 'critical' | 'warning'; text: string; stepId: string; rank: number }[] = [];
  for (const s of steps) {
    for (const flag of s.security_flags ?? []) {
      const reasons = (s.security_reasons ?? []).filter((r) => r.startsWith(flag + ':')).map((r) => reasonText(r));
      const subject = flag === 'prompt_injection' ? `What ${s.name} returned` : flag === 'secret_exposed' ? `In ${s.name}, ` : `${s.name}`;
      const what = reasons.length ? reasons.join('; ') : FLAG_HELP[flag] ?? flagLabel(flag);
      const text =
        flag === 'secret_exposed'
          ? `${subject}${what} (redacted before storage).`
          : `${subject} ${what}: ${flagLabel(flag).toLowerCase()}.`;
      out.push({ tone: SEVERITY[flag] === 'critical' ? 'critical' : 'warning', text, stepId: s.step_id, rank: SEVERITY[flag] === 'critical' ? 0 : 1 });
    }
  }
  return out
    .sort((a, b) => a.rank - b.rank)
    .slice(0, 3)
    .map(({ rank: _rank, ...l }) => l);
}
