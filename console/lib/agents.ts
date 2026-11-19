// The Agents page: compare an agent's versions, and tool health. Pure;
// unit-tested.

import { type Delta, delta } from './insights';

export interface VersionStats {
  version: string;
  runs: number;
  failed_runs: number;
  cost_usd: number;
  cost_partial: boolean;
  p50_run_ms: number;
  p95_run_ms: number;
  avg_steps: number;
  avg_llm_calls: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  tool_calls: number;
  tool_failures: number;
  first_run_start: string;
  last_run_start: string;
}

export interface ToolHealth {
  tool_name: string;
  framework: string;
  calls: number;
  failures: number;
  argument_errors: number;
  runs: number;
  p50_ms: number;
  p95_ms: number;
  avg_output_bytes: number;
  max_output_bytes: number;
  top_error: string;
  sample_trace_id: string;
  sample_step_id: string;
}

export interface VersionRow {
  v: VersionStats;
  failureRate: number;
  costPerRun: number;
  cacheRate: number;
  toolErrorRate: number;
  /** Change versus the next older version (undefined for the oldest). */
  vs?: { version: string; failureRate: Delta; costPerRun: Delta; p95: Delta; steps: Delta; toolErrorRate: Delta };
}

/** Fewer runs than this and a comparison is noise. */
export const MIN_COMPARE_RUNS = 5;

const rate = (a: number, b: number) => (b ? a / b : 0);

export function versionRows(versions: VersionStats[]): VersionRow[] {
  const rows: VersionRow[] = versions.map((v) => ({
    v,
    failureRate: rate(v.failed_runs, v.runs),
    costPerRun: rate(v.cost_usd, v.runs),
    cacheRate: rate(v.cached_tokens, v.input_tokens),
    toolErrorRate: rate(v.tool_failures, v.tool_calls),
  }));
  // versions arrive most recent first; compare each with the next older one
  for (let i = 0; i < rows.length - 1; i++) {
    const a = rows[i]!;
    const b = rows[i + 1]!;
    if (a.v.runs < MIN_COMPARE_RUNS || b.v.runs < MIN_COMPARE_RUNS) continue;
    a.vs = {
      version: b.v.version,
      failureRate: delta(a.failureRate, b.failureRate, { higherIsBetter: false, percentPoints: true }),
      costPerRun: delta(a.costPerRun, b.costPerRun, { higherIsBetter: false }),
      p95: delta(a.v.p95_run_ms, b.v.p95_run_ms, { higherIsBetter: false }),
      steps: delta(a.v.avg_steps, b.v.avg_steps, { higherIsBetter: false }),
      toolErrorRate: delta(a.toolErrorRate, b.toolErrorRate, { higherIsBetter: false, percentPoints: true }),
    };
  }
  return rows;
}

/** One sentence on the newest version against the one before. */
export function verdict(rows: VersionRow[]): string | null {
  const top = rows[0];
  if (!top?.vs) return null;
  const parts: string[] = [];
  const say = (label: string, d: Delta) => {
    if (d.direction === 'flat' || d.direction === 'new') return;
    parts.push(`${label} ${d.text}${d.good === undefined ? '' : d.good ? ' (better)' : ' (worse)'}`);
  };
  say('failure rate', top.vs.failureRate);
  say('cost per run', top.vs.costPerRun);
  say('p95 duration', top.vs.p95);
  say('steps per run', top.vs.steps);
  say('tool errors', top.vs.toolErrorRate);
  const name = (v: string) => v || 'unversioned';
  if (!parts.length) return `${name(top.v.version)} performs like ${name(top.vs.version)}.`;
  return `${name(top.v.version)} vs ${name(top.vs.version)}: ${parts.join(', ')}.`;
}

/** Tools that need a look, worst first. */
export function toolFlags(t: ToolHealth): string[] {
  const flags: string[] = [];
  if (t.calls && t.failures / t.calls >= 0.1) flags.push('fails often');
  if (t.argument_errors >= 2) flags.push('bad arguments');
  if (t.max_output_bytes >= 50_000) flags.push('large results');
  if (t.p95_ms >= 10_000) flags.push('slow');
  return flags;
}
