// Alert rules and channels: types and form parsing. Pure; unit-tested.
// The API validates authoritatively; this shapes input and display.

export type RuleKind =
  | 'error_rate'
  | 'runaway_loop'
  | 'run_cost'
  | 'negative_feedback'
  | 'cost_regression'
  | 'new_error'
  | 'weekly_digest'
  | 'security_signal';

/** Kinds whose threshold is a percentage in the form (stored as a fraction). */
const PERCENT: RuleKind[] = ['error_rate', 'negative_feedback', 'cost_regression'];
/** Kinds that take a minimum count (runs or feedback). */
export const HAS_MIN: RuleKind[] = ['error_rate', 'negative_feedback', 'cost_regression'];
export type ChannelKind = 'slack' | 'webhook' | 'email';

export interface AlertRule {
  id: string;
  kind: RuleKind;
  threshold: number;
  window_minutes: number;
  min_runs: number;
  enabled: boolean;
  created_at: string;
}

export interface AlertChannel {
  id: string;
  kind: ChannelKind;
  name: string;
  target_display: string;
  enabled: boolean;
  created_at: string;
}

export interface AlertEvent {
  id: string;
  rule_id: string;
  kind: RuleKind;
  summary: string;
  trace_id?: string;
  details: Record<string, unknown>;
  delivery: Record<string, string>;
  fired_at: string;
}

export const RULE_KINDS: { kind: RuleKind; label: string; unit: string; help: string }[] = [
  { kind: 'runaway_loop', label: 'Runaway loop', unit: 'repeats', help: 'A run calls the same tool with the same input this many times.' },
  { kind: 'error_rate', label: 'Failure rate', unit: '% of runs', help: 'This share of runs failed in the window.' },
  { kind: 'run_cost', label: 'Expensive run', unit: 'USD per run', help: 'One run cost at least this much (known prices only).' },
  { kind: 'negative_feedback', label: 'Unhappy users', unit: '% negative', help: 'This share of user feedback (bindle.feedback) was negative in the window. Minimum: feedback needed (default 5).' },
  { kind: 'cost_regression', label: 'Cost regression', unit: '% more per run', help: "An agent's newest version costs this much more per run than the previous one. Minimum: runs per version (default 5)." },
  { kind: 'new_error', label: 'New error', unit: 'failures', help: "An error that didn't occur in the 7 days before appeared this many times." },
  { kind: 'security_signal', label: 'Security signal', unit: 'flagged steps', help: "An agent's steps were flagged this many times in the window for possible prompt injection, an exposed credential or possible data exfiltration." },
  { kind: 'weekly_digest', label: 'Weekly digest', unit: '', help: 'A summary every Monday at 09:00 UTC: runs, failure rate and spend against the week before, what failed most, and what users asked about more.' },
];

export const WINDOWS = [
  { minutes: 15, label: '15 minutes' },
  { minutes: 60, label: '1 hour' },
  { minutes: 360, label: '6 hours' },
  { minutes: 1440, label: '24 hours' },
] as const;

export const CHANNEL_KINDS: { kind: ChannelKind; label: string; placeholder: string }[] = [
  { kind: 'slack', label: 'Slack', placeholder: 'https://hooks.slack.com/services/…' },
  { kind: 'webhook', label: 'Webhook', placeholder: 'https://example.com/bindle-alerts' },
  { kind: 'email', label: 'Email', placeholder: 'oncall@example.com' },
];

export function isRuleKind(v: unknown): v is RuleKind {
  return RULE_KINDS.some((k) => k.kind === v);
}

export function isChannelKind(v: unknown): v is ChannelKind {
  return CHANNEL_KINDS.some((k) => k.kind === v);
}

/** Threshold as the user types it (percent for failure rate). */
export function thresholdInput(kind: RuleKind, threshold: number): string {
  return PERCENT.includes(kind) ? String(Math.round(threshold * 1000) / 10) : String(threshold);
}

export function windowLabel(minutes: number): string {
  return WINDOWS.find((w) => w.minutes === minutes)?.label ?? `${minutes} minutes`;
}

export function describeRule(r: Pick<AlertRule, 'kind' | 'threshold' | 'window_minutes' | 'min_runs'>): string {
  const win = windowLabel(r.window_minutes);
  switch (r.kind) {
    case 'runaway_loop':
      return `A tool is called ${r.threshold}+ times with the same input in one run (checked over ${win})`;
    case 'error_rate':
      return `${thresholdInput('error_rate', r.threshold)}%+ of runs fail over ${win}${r.min_runs ? ` (at least ${r.min_runs} runs)` : ''}`;
    case 'run_cost':
      return `A run costs $${r.threshold >= 0.01 ? r.threshold.toFixed(2) : String(r.threshold)} or more (checked over ${win})`;
    case 'negative_feedback':
      return `${thresholdInput('negative_feedback', r.threshold)}%+ of user feedback is negative over ${win}${r.min_runs ? ` (at least ${r.min_runs})` : ''}`;
    case 'cost_regression':
      return `A new agent version costs ${thresholdInput('cost_regression', r.threshold)}%+ more per run than the previous one (runs over ${win})`;
    case 'new_error':
      return `A new error appears ${r.threshold}+ times over ${win}`;
    case 'security_signal':
      return `An agent has ${r.threshold}+ step${r.threshold === 1 ? '' : 's'} flagged for the same security signal over ${win}`;
    case 'weekly_digest':
      return 'Every Monday at 09:00 UTC';
  }
}

export type ParsedRule = { threshold: number; window_minutes: number; min_runs: number } | { error: string };

export function parseRule(kind: RuleKind, threshold: unknown, window: unknown, minRuns: unknown): ParsedRule {
  const t = typeof threshold === 'string' && threshold.trim() !== '' ? Number(threshold.trim().replace(/^\$/, '').replace(/%$/, '')) : NaN;
  const w = Number(window);
  if (!WINDOWS.some((x) => x.minutes === w)) return { error: 'Choose a window.' };
  const m = minRuns === null || minRuns === undefined || String(minRuns).trim() === '' ? 0 : Number(minRuns);
  if (!Number.isInteger(m) || m < 0 || m > 100000) return { error: 'Minimum runs must be a whole number.' };
  if (!Number.isFinite(t)) return { error: 'Enter a threshold.' };
  switch (kind) {
    case 'error_rate':
      if (t <= 0 || t > 100) return { error: 'Failure rate must be above 0% and at most 100%.' };
      return { threshold: Math.round(t * 10) / 1000, window_minutes: w, min_runs: m };
    case 'runaway_loop':
      if (!Number.isInteger(t) || t < 2 || t > 10000) return { error: 'Repeats must be a whole number from 2 to 10000.' };
      return { threshold: t, window_minutes: w, min_runs: 0 };
    case 'run_cost':
      if (t <= 0 || t > 1e6) return { error: 'Cost must be above $0.' };
      return { threshold: t, window_minutes: w, min_runs: 0 };
    case 'negative_feedback':
      if (t <= 0 || t > 100) return { error: 'Enter a share above 0% and at most 100%.' };
      return { threshold: Math.round(t * 10) / 1000, window_minutes: w, min_runs: m };
    case 'cost_regression':
      if (t <= 0 || t > 10000) return { error: 'Enter an increase above 0%.' };
      return { threshold: Math.round(t * 10) / 1000, window_minutes: w, min_runs: m };
    case 'new_error':
      if (!Number.isInteger(t) || t < 1) return { error: 'Failures must be a whole number, at least 1.' };
      return { threshold: t, window_minutes: w, min_runs: 0 };
    case 'security_signal':
      if (!Number.isInteger(t) || t < 1) return { error: 'Flagged steps must be a whole number, at least 1.' };
      return { threshold: t, window_minutes: w, min_runs: 0 };
    case 'weekly_digest':
      return { threshold: 1, window_minutes: 1440, min_runs: 0 };
  }
}

/** Delivery summary for an event, e.g. "Sent to 2 of 3". */
export function deliverySummary(delivery: Record<string, string>): { ok: boolean; text: string } {
  const all = Object.values(delivery ?? {});
  if (all.length === 0) return { ok: false, text: 'No channels' };
  const sent = all.filter((s) => s === 'sent').length;
  return { ok: sent === all.length, text: sent === all.length ? `Sent (${sent})` : `Sent to ${sent} of ${all.length}` };
}
