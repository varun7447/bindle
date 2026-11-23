// Agent passports: what an agent may do, and where it went outside that
// ("drift"). Drift is observed, never blocked. Pure; unit-tested.

import { formatCount } from './format';
import type { AttentionItem } from './insights';
import type { StepSummary } from './types';

export interface Passport {
  version: string;
  agent: string;
  owner: string;
  identity?: { kind: string; id: string };
  autonomy_tier?: number;
  tools?: { allow: string[] };
  data_classes?: { allow: string[] | null; egress_block?: string[] };
  budget?: { per_run_usd?: number; per_day_usd?: number };
  limits?: { tool: string; max: number; window: string }[];
  release_rights?: 'none' | 'request' | 'full';
  release_tools?: string[];
}

export interface PassportVersion {
  agent: string;
  version: number;
  sha256: string;
  pushed_by: string;
  pushed_at: string;
  passport: Passport;
}

export interface AgentPassportResponse {
  agent: string;
  active: PassportVersion | null;
  history: PassportVersion[];
}

export interface DriftSignal {
  kind: string;
  agent: string;
  steps: number;
  runs: number;
  last_seen: string;
  reasons: Record<string, number>;
  examples: { trace_id: string; step_id: string; step_name: string; tool_name?: string; reasons: string[] }[];
}

export interface DriftFinding {
  kind: string;
  agent: string;
  detail: string;
  value: number;
  limit: number;
  window?: string;
  trace_id?: string;
  summary: string;
}

export interface DriftResponse {
  window: string;
  signals: DriftSignal[];
  findings: DriftFinding[];
  agents_with_passport: string[];
  explanations: Record<string, string>;
}

export interface MgmtKey {
  id: string;
  name: string;
  display_prefix: string;
  scopes: string[];
  created_by?: string;
  created_at: string;
  revoked_at?: string;
  last_used_at?: string;
}

export const SCOPE_LABEL: Record<string, string> = {
  'passport:write': 'Push passports',
  'vault:sync': 'Sync the knowledge vault',
  'skills:write': 'Push skills',
};

export const DRIFT_LABEL: Record<string, string> = {
  tool_not_allowed: 'Tool outside the passport',
  data_class_not_allowed: 'Data class not allowed',
  egress_blocked: 'Blocked data sent out',
  release_not_allowed: 'Release action without rights',
  rate_over_limit: 'Tool over its rate limit',
  run_over_budget: 'Run over budget',
  day_over_budget: 'Daily budget exceeded',
};

/** Kept in step with the backend (passport.Explain). */
export const DRIFT_HELP: Record<string, string> = {
  tool_not_allowed: "The agent called a tool its passport doesn't list.",
  data_class_not_allowed: "A step handled a kind of data its passport doesn't allow.",
  egress_blocked: 'Data its passport blocks from leaving was passed to a tool that sends data out.',
  release_not_allowed: 'The agent took a release action (deploy, merge, push, publish) and its passport gives it no release rights.',
  rate_over_limit: 'A tool was called more often than its passport allows in the window.',
  run_over_budget: "One run cost more than the passport's per-run budget.",
  day_over_budget: "The agent's spend today is over the passport's daily budget.",
};

const SEVERITY: Record<string, AttentionItem['severity']> = {
  egress_blocked: 'critical',
  release_not_allowed: 'critical',
  tool_not_allowed: 'warning',
  data_class_not_allowed: 'warning',
  rate_over_limit: 'warning',
  run_over_budget: 'warning',
  day_over_budget: 'warning',
};

export function driftLabel(kind: string): string {
  return DRIFT_LABEL[kind] ?? kind.replace(/_/g, ' ');
}

/** "tool_not_allowed:refund_payment" -> "refund_payment". */
export function driftDetail(reason: string): string {
  const i = reason.indexOf(':');
  return i < 0 ? '' : reason.slice(i + 1);
}

function plural(n: number, word: string): string {
  return `${formatCount(n)} ${word}${n === 1 ? '' : 's'}`;
}

function topDetails(reasons: Record<string, number>, max = 3): string {
  const list = Object.entries(reasons).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  const shown = list.slice(0, max).map(([r, n]) => (n > 1 ? `${driftDetail(r)} ×${n}` : driftDetail(r)));
  return shown.join(', ') + (list.length > max ? ', …' : '');
}

/** Home "needs attention" items: one per drift kind and agent, plus rate and budget findings. */
export function driftItems(res: Pick<DriftResponse, 'signals' | 'findings'>): AttentionItem[] {
  const items: AttentionItem[] = res.signals.map((s) => {
    const who = s.agent || 'Unnamed agent';
    const ex = s.examples[0];
    const details = topDetails(s.reasons);
    return {
      key: `drift:${s.kind}:${s.agent}`,
      severity: SEVERITY[s.kind] ?? 'warning',
      title: `${who}: ${driftLabel(s.kind).toLowerCase()}`,
      detail: `${plural(s.steps, 'step')} in ${plural(s.runs, 'run')}${details ? ` (${details})` : ''}. Observed against its passport; nothing was blocked.`,
      href: ex ? `/traces/${ex.trace_id}?step=${ex.step_id}` : '/runs',
      cta: ex ? 'Open latest' : 'See runs',
    };
  });
  for (const f of res.findings) {
    items.push({
      key: `drift:${f.kind}:${f.agent}:${f.detail}`,
      severity: SEVERITY[f.kind] ?? 'warning',
      title: `${f.agent || 'Unnamed agent'}: ${driftLabel(f.kind).toLowerCase()}`,
      detail: `${f.summary}. Observed against its passport; nothing was blocked.`,
      href: f.trace_id ? `/traces/${f.trace_id}` : `/agents?agent=${encodeURIComponent(f.agent)}`,
      cta: f.trace_id ? 'Open run' : 'Open agent',
    });
  }
  return items;
}

/** Run-summary lines for drifting steps (at most three), most severe first. */
export function driftLines(steps: StepSummary[]): { tone: 'critical' | 'warning'; text: string; stepId: string }[] {
  const out: { tone: 'critical' | 'warning'; text: string; stepId: string; rank: number }[] = [];
  for (const s of steps) {
    for (const r of s.passport_drift ?? []) {
      const kind = r.split(':')[0]!;
      const detail = driftDetail(r);
      const critical = SEVERITY[kind] === 'critical';
      out.push({
        tone: critical ? 'critical' : 'warning',
        text: `${s.name}: ${driftLabel(kind).toLowerCase()}${detail ? ` (${detail})` : ''}. Not blocked; the passport is observe-only.`,
        stepId: s.step_id,
        rank: critical ? 0 : 1,
      });
    }
  }
  return out
    .sort((a, b) => a.rank - b.rank)
    .slice(0, 3)
    .map(({ rank: _rank, ...l }) => l);
}

/** The agent page's one-line answer about the passport. */
export function passportSentence(agent: string, active: PassportVersion | null, res?: Pick<DriftResponse, 'signals' | 'findings'>): string {
  if (!active) return `${agent || 'This agent'} has no passport. Bindle doesn't check what it may do until one is pushed.`;
  const steps = (res?.signals ?? []).filter((s) => s.agent === agent).reduce((n, s) => n + s.steps, 0);
  const findings = (res?.findings ?? []).filter((f) => f.agent === agent).length;
  const base = `Passport v${active.version}, owned by ${active.passport.owner}.`;
  if (!res) return base;
  if (steps === 0 && findings === 0) return `${base} No drift in this period.`;
  const parts = [steps ? plural(steps, 'step') + ' outside it' : '', findings ? plural(findings, 'limit') + ' exceeded' : ''].filter(Boolean);
  return `${base} ${parts.join(' and ')} in this period (observed, not blocked).`;
}

/** What a passport declares, as short lines for the agent page. */
export function passportFacts(p: Passport): { label: string; value: string }[] {
  const facts: { label: string; value: string }[] = [{ label: 'Owner', value: p.owner }];
  if (p.identity) facts.push({ label: 'Identity', value: `${p.identity.kind.replace('_', ' ')}: ${p.identity.id}` });
  if (p.autonomy_tier !== undefined) facts.push({ label: 'Autonomy tier', value: String(p.autonomy_tier) });
  if (p.tools) facts.push({ label: 'Tools', value: p.tools.allow.length ? p.tools.allow.join(', ') : 'none allowed' });
  if (p.data_classes) {
    const allow = p.data_classes.allow;
    if (allow) facts.push({ label: 'Data classes', value: allow.length ? allow.join(', ') : 'none allowed' });
    if (p.data_classes.egress_block?.length) facts.push({ label: 'Never sent out', value: p.data_classes.egress_block.join(', ') });
  }
  if (p.budget?.per_run_usd) facts.push({ label: 'Per-run budget', value: `$${p.budget.per_run_usd}` });
  if (p.budget?.per_day_usd) facts.push({ label: 'Daily budget', value: `$${p.budget.per_day_usd}` });
  for (const l of p.limits ?? []) facts.push({ label: 'Rate limit', value: `${l.tool}: at most ${l.max} per ${l.window}` });
  if (p.release_rights) facts.push({ label: 'Release rights', value: p.release_rights });
  return facts;
}
