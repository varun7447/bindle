// Outcome economics (cost per successful outcome, wasted spend) and turning
// production problems into eval cases. Pure; unit-tested.

import { formatCost, formatCount, formatPercent } from './format';
import type { AttentionItem } from './insights';
import type { Scorer } from './evals';

// ---- problem runs -> dataset ----------------------------------------------

export const PROBLEM_KINDS = ['failure', 'negative_feedback', 'failed_score', 'failed_runs'] as const;
export type ProblemKind = (typeof PROBLEM_KINDS)[number];

export interface ProblemSource {
  kind: ProblemKind;
  step_name?: string;
  pattern?: string;
  agent?: string;
  score?: string;
  window?: '24h' | '7d' | '30d';
}

export interface Suggestion {
  scorer: Omit<Scorer, 'id' | 'created_at'>;
  reason: string;
  catches: number;
  graded: number;
  exists: boolean;
}

export interface SuggestionsResponse {
  source: string;
  agent: string;
  matched: number;
  trace_ids: string[];
  suggestions: Suggestion[];
}

const FIELDS = ['step_name', 'pattern', 'agent', 'score'] as const;

/** Reads a problem source from search params; null when it isn't valid. */
export function parseProblemSource(sp: Record<string, string | string[] | undefined>): ProblemSource | null {
  const one = (k: string) => {
    const v = sp[k];
    return typeof v === 'string' ? v : undefined;
  };
  const kind = one('kind');
  if (!kind || !(PROBLEM_KINDS as readonly string[]).includes(kind)) return null;
  const src: ProblemSource = { kind: kind as ProblemKind };
  for (const f of FIELDS) {
    const v = one(f);
    if (v === undefined || v === '') continue;
    if (v.length > 256 || /[\u0000-\u001f\u007f]/.test(v)) return null;
    src[f] = v;
  }
  const w = one('window');
  if (w === '24h' || w === '7d' || w === '30d') src.window = w;
  if (src.kind === 'failure' && !src.step_name && !src.pattern) return null;
  return src;
}

/** The query string for a source (also the form's hidden fields). */
export function problemParams(src: ProblemSource): Record<string, string> {
  const out: Record<string, string> = { kind: src.kind };
  for (const f of FIELDS) if (src[f]) out[f] = src[f]!;
  if (src.window) out.window = src.window;
  return out;
}

const CHECK_KINDS = new Set(['contains', 'not_contains', 'regex', 'json_valid', 'tool_called', 'tool_not_called', 'max_steps', 'max_cost_usd', 'max_duration_ms', 'run_succeeded']);

/** Reads the suggested checks a user kept (JSON form values); the API validates them again. */
export function parseChecks(values: string[]): Omit<Scorer, 'id' | 'created_at' | 'enabled'>[] {
  const out: Omit<Scorer, 'id' | 'created_at' | 'enabled'>[] = [];
  for (const v of values.slice(0, 10)) {
    try {
      const c = JSON.parse(v) as Record<string, unknown>;
      if (typeof c.name !== 'string' || typeof c.kind !== 'string' || !CHECK_KINDS.has(c.kind)) continue;
      out.push({
        name: c.name.slice(0, 80),
        kind: c.kind,
        agent: typeof c.agent === 'string' ? c.agent : '',
        value: typeof c.value === 'string' ? c.value : '',
        number: typeof c.number === 'number' && Number.isFinite(c.number) ? c.number : 0,
        case_sensitive: c.case_sensitive === true,
      });
    } catch {
      // ignore a tampered value
    }
  }
  return out;
}

export function problemHref(src: ProblemSource): string {
  return `/evals/from-problems?${new URLSearchParams(problemParams(src))}`;
}

export function problemTitle(src: ProblemSource): string {
  const who = src.agent ? ` of ${src.agent}` : '';
  switch (src.kind) {
    case 'failure':
      return `Runs where ${src.step_name ?? 'a step'} failed${src.pattern ? ` with “${src.pattern}”` : ''}${who}`;
    case 'negative_feedback':
      return `Runs users gave a thumbs down${who}`;
    case 'failed_score':
      return `Runs that failed ${src.score ? `the “${src.score}” score` : 'a score'}${who}`;
    case 'failed_runs':
      return `Failed runs${who}`;
  }
}

// ---- outcome economics -----------------------------------------------------------

export interface Economics {
  agent: string;
  version: string;
  definition: 'outcome' | 'no_failure_or_negative_feedback';
  runs: number;
  successes: number;
  wasted_runs: number;
  outcome_runs: number;
  cost_usd: number;
  wasted_usd: number;
  cost_per_success_usd: number | null;
  unknown_cost_runs: number;
}

export function definitionLabel(d: Economics['definition']): string {
  return d === 'outcome' ? 'from bindle.outcome()' : 'no failed step and no thumbs down';
}

/** One agent's versions added up. */
export function agentTotals(rows: Economics[]): Economics[] {
  const by = new Map<string, Economics>();
  for (const r of rows) {
    const t = by.get(r.agent);
    if (!t) {
      by.set(r.agent, { ...r, version: '' });
      continue;
    }
    t.runs += r.runs;
    t.successes += r.successes;
    t.wasted_runs += r.wasted_runs;
    t.outcome_runs += r.outcome_runs;
    t.cost_usd += r.cost_usd;
    t.wasted_usd += r.wasted_usd;
    t.unknown_cost_runs += r.unknown_cost_runs;
    if (r.definition === 'outcome') t.definition = 'outcome';
  }
  for (const t of by.values()) t.cost_per_success_usd = t.successes > 0 ? t.cost_usd / t.successes : null;
  return [...by.values()];
}

/** Flag agents when this share of priced spend went to runs without a good outcome. */
export const WASTE_SHARE = 0.25;
export const WASTE_MIN_RUNS = 10;
export const WASTE_MIN_USD = 0.01;

export function wasteItems(rows: Economics[], windowParam: string): AttentionItem[] {
  const items: AttentionItem[] = [];
  for (const t of agentTotals(rows)) {
    if (t.runs < WASTE_MIN_RUNS || t.cost_usd <= 0 || t.wasted_usd < WASTE_MIN_USD) continue;
    const share = t.wasted_usd / t.cost_usd;
    if (share < WASTE_SHARE) continue;
    const name = t.agent || 'Unnamed agent';
    const partial = t.unknown_cost_runs > 0;
    const per = t.cost_per_success_usd === null ? 'no successful outcomes yet' : `${formatCost(t.cost_per_success_usd, partial)} per successful outcome`;
    items.push({
      key: `waste:${t.agent}`,
      severity: share >= 0.5 ? 'critical' : 'warning',
      title: `${name}: ${formatPercent(share)} of spend went to runs that didn't succeed`,
      detail: `${formatCost(t.wasted_usd, partial)} of ${formatCost(t.cost_usd, partial)} on ${formatCount(t.wasted_runs)} run${t.wasted_runs === 1 ? '' : 's'} · ${per} (${definitionLabel(t.definition)}).`,
      href: `/agents?${new URLSearchParams({ agent: t.agent, ...(windowParam === '7d' ? {} : { window: windowParam }) })}#economics-h`,
      cta: 'See cost per outcome',
    });
  }
  return items;
}
