// The home page's "what needs attention?" logic. Pure; unit-tested.

import { formatCost, formatCount, formatDuration, formatPercent } from './format';
import type { Alert, Insights, OverviewResponse } from './types';

export type Severity = 'critical' | 'warning' | 'info';

export interface AttentionItem {
  key: string;
  severity: Severity;
  title: string;
  detail: string;
  href: string;
  cta: string;
  /** Distinct errors behind one issue, most frequent first. */
  variants?: { text: string; count: number; href: string }[];
}

export interface Headline {
  tone: 'good' | 'warning' | 'critical' | 'empty';
  title: string;
  detail: string;
}

/** An agent is flagged when this share of its runs failed (with enough runs). */
export const AGENT_FAILURE_RATE = 0.2;
export const AGENT_MIN_RUNS = 3;

export function agentLabel(name: string): string {
  return name || 'Unnamed agent';
}

function runsHref(params: Record<string, string>): string {
  const q = new URLSearchParams(params).toString();
  return q ? `/runs?${q}` : '/runs';
}

export function attentionItems(ov: OverviewResponse, ins: Insights, loops: Alert[], windowParam: string): AttentionItem[] {
  const items: AttentionItem[] = [];
  const range: Record<string, string> = windowParam === '24h' ? {} : { range: windowParam };

  // Failing steps, grouped. Skip agent wrappers that only echo a child's
  // failure from the same run.
  const leafTraces = new Set(ins.issues.filter((i) => i.step_type !== 'agent').map((i) => i.sample_trace_id));
  for (const is of ins.issues) {
    if (is.step_type === 'agent' && leafTraces.has(is.sample_trace_id)) continue;
    items.push({
      key: `issue:${is.step_name}`,
      severity: 'critical',
      title: `${is.step_name} is failing`,
      detail: `Failed ${formatCount(is.failures)} time${is.failures === 1 ? '' : 's'} in ${formatCount(is.runs)} run${is.runs === 1 ? '' : 's'}${is.sample_error ? `. Latest error: ${is.sample_error}` : ''}`,
      href: `/traces/${is.sample_trace_id}?step=${is.sample_step_id}`,
      cta: 'Open latest failure',
      variants: (is.patterns?.length ?? 0) > 1
        ? is.patterns!.map((p) => ({ text: p.pattern, count: p.failures, href: `/traces/${p.sample_trace_id}?step=${p.sample_step_id}` }))
        : undefined,
    });
  }

  // Agents with a high failure rate.
  for (const a of ins.agents) {
    if (a.runs < AGENT_MIN_RUNS || a.failed_runs / a.runs < AGENT_FAILURE_RATE) continue;
    items.push({
      key: `agent:${a.agent}`,
      severity: a.failed_runs === a.runs ? 'critical' : 'warning',
      title: `${agentLabel(a.agent)}: ${formatPercent(a.failed_runs / a.runs)} of runs failed`,
      detail: `${formatCount(a.failed_runs)} of ${formatCount(a.runs)} runs in this period.`,
      href: runsHref({ ...range, agent: a.agent, status: 'failed' }),
      cta: 'See failed runs',
    });
  }

  // Runaway loops (last hour).
  for (const l of loops) {
    items.push({
      key: `loop:${l.trace_id}:${l.tool_name}`,
      severity: 'warning',
      title: `Possible runaway loop: ${l.tool_name}`,
      detail: `Called ${l.repeats} times with the same input in one run.`,
      href: `/traces/${l.trace_id}`,
      cta: 'Open run',
    });
  }

  // Unusual runs: far more steps or cost than the agent's typical run.
  for (const o of ins.outliers ?? []) {
    const who = agentLabel(o.run.agent);
    const times = o.typical > 0 ? `${formatRatio(o.value / o.typical)} the typical` : 'far above the typical';
    items.push({
      key: `outlier:${o.run.trace_id}`,
      severity: 'warning',
      title: o.reason === 'steps' ? `${who}: a run took ${formatCount(o.value)} steps` : `${who}: a run cost ${formatCost(o.value, o.run.cost_partial)}`,
      detail:
        o.reason === 'steps'
          ? `${times} ${formatCount(Math.round(o.typical))} steps for this agent. It may be stuck or looping.`
          : `${times} ${formatCost(o.typical, false)} per run for this agent.`,
      href: `/traces/${o.run.trace_id}`,
      cta: 'Open run',
    });
  }

  // Sessions where users gave negative feedback.
  for (const u of ins.unhappy_sessions ?? []) {
    items.push({
      key: `unhappy:${u.session_id}`,
      severity: 'warning',
      title: `A user was unhappy with a conversation`,
      detail: `${u.feedback.negative} thumbs down${u.feedback.positive ? `, ${u.feedback.positive} up` : ''} in session ${u.session_id.length > 24 ? u.session_id.slice(0, 24) + '…' : u.session_id}.`,
      href: `/sessions/${encodeURIComponent(u.session_id)}`,
      cta: 'Open conversation',
    });
  }

  // Cost we can't price.
  if (ov.cost_partial && ov.cost_unknown_steps > 0) {
    items.push({
      key: 'cost-unknown',
      severity: 'info',
      title: 'Some model calls have no known price',
      detail: `${formatCount(ov.cost_unknown_steps)} LLM step${ov.cost_unknown_steps === 1 ? '' : 's'} used models missing from the price table, so cost is a lower bound.`,
      href: runsHref(range),
      cta: 'See runs',
    });
  }

  const rank: Record<Severity, number> = { critical: 0, warning: 1, info: 2 };
  return items.sort((a, b) => rank[a.severity] - rank[b.severity]);
}

function formatRatio(x: number): string {
  return `${x >= 10 ? Math.round(x) : x.toFixed(1).replace(/\.0$/, '')}×`;
}

export function headline(ov: OverviewResponse, ins: Insights, items: AttentionItem[], windowLabel: string): Headline {
  if (ov.runs === 0) return { tone: 'empty', title: 'No runs yet', detail: `Nothing arrived in the ${windowLabel.toLowerCase()}.` };
  const agents = ins.agents.length;
  const base = `${formatCount(ov.runs)} run${ov.runs === 1 ? '' : 's'} from ${agents} agent${agents === 1 ? '' : 's'}`;
  const failed = ov.failed_runs ? `, ${formatPercent(ov.run_failure_rate)} failed` : ', none failed';
  const cost = ov.cost_partial && ov.cost_usd === 0 ? 'cost unknown' : `${formatCost(ov.cost_usd, ov.cost_partial)} spent`;
  const detail = `${base}${failed} · p95 ${formatDuration(ov.p95_run_ms)} · ${cost}`;
  const urgent = items.filter((i) => i.severity !== 'info');
  if (urgent.length === 0) return { tone: 'good', title: 'All clear', detail };
  const critical = urgent.some((i) => i.severity === 'critical');
  return {
    tone: critical ? 'critical' : 'warning',
    title: `${urgent.length} issue${urgent.length === 1 ? ' needs' : 's need'} attention`,
    detail,
  };
}

export interface Delta {
  text: string;
  direction: 'up' | 'down' | 'flat' | 'new';
  /** Whether the change is good news (for up/down). undefined = neutral. */
  good?: boolean;
}

/**
 * Change versus the previous period. higherIsBetter decides whether a rise
 * is good news; undefined keeps it neutral (e.g. run volume).
 */
export function delta(current: number, previous: number, opts: { higherIsBetter?: boolean; percentPoints?: boolean } = {}): Delta {
  if (previous === 0 && current === 0) return { text: 'no change', direction: 'flat' };
  if (previous === 0) return { text: 'new this period', direction: 'new' };
  let text: string;
  let diff: number;
  if (opts.percentPoints) {
    diff = (current - previous) * 100;
    if (Math.abs(diff) < 0.5) return { text: 'no change', direction: 'flat' };
    text = `${diff > 0 ? '+' : '−'}${Math.abs(diff).toFixed(diff > -10 && diff < 10 ? 1 : 0)} pts`;
  } else {
    diff = (current - previous) / previous;
    if (Math.abs(diff) < 0.005) return { text: 'no change', direction: 'flat' };
    text = `${diff > 0 ? '+' : '−'}${formatPercent(Math.abs(diff))}`;
  }
  const direction = diff > 0 ? 'up' : 'down';
  const good = opts.higherIsBetter === undefined ? undefined : (direction === 'up') === opts.higherIsBetter;
  return { text, direction, good };
}
